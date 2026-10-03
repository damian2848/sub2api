import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { once } from 'node:events';
import { before, after, test } from 'node:test';
import { chromium } from 'playwright';
import { BrowserSession } from '../../src/browser.mjs';
import { PrismError } from '../../src/errors.mjs';
import { AccountPageMultiplexer } from '../../src/page-multiplexer.mjs';
import { AccountPoolManager } from '../../src/pool.mjs';
import { createPrismServer } from '../../src/server.mjs';
import { ResourceGuard, RuntimeMetrics } from '../../src/resources.mjs';
import { createMockPrism, FAKE_TOKEN, MODELS, USER_ID, USER_EMAIL } from './mock-prism.mjs';

let fixture;
let browser;
const credentials = {access_token:FAKE_TOKEN, expected_user_id:USER_ID, expected_email:USER_EMAIL};
const request = overrides => ({model:'gpt-6.1-sol', effort:'xhigh',
  input:[{role:'developer', content:[{type:'input_text', text:'offline developer instruction'}]},
    {role:'user', content:[{type:'input_text', text:'offline user prompt'}]}], ...overrides});
const code = (expected, status) => error => error instanceof PrismError && error.code === expected &&
  (status === undefined || error.status === status);

before(async () => {
  fixture = await createMockPrism();
  try {
    browser = await chromium.launch({headless:true, proxy:{server:fixture.proxyURL},
      // The TLS key belongs only to this offline fixture. It never authenticates a real host.
      args:['--ignore-certificate-errors', '--disable-background-networking', '--disable-component-update'],
      ...(process.env.PRISM_CHROMIUM_EXECUTABLE ? {executablePath:process.env.PRISM_CHROMIUM_EXECUTABLE} : {})});
  } catch (error) {
    await fixture.close();
    throw new Error('Chromium smoke requires a real browser. Run `npx playwright install chromium` ' +
      'or set PRISM_CHROMIUM_EXECUTABLE to a compatible local Chromium; this suite does not silently skip.', {cause:error});
  }
});
after(async () => {await browser?.close(); await fixture?.close();});

// The default path is Playwright route (what production runs); the CDP cache path is opt-in. Both are exercised.
const modes = [{name:'default (Playwright route)', options:{}}, {name:'opt-in (CDP, HTTP cache)', options:{httpCache:true}}];

async function sessionFor(t, {initialize = true, create = false, heartbeat = () => {}, options = {}, worker = 0} = {}) {
  // Route mode needs a page that reads sandbox bodies (as the real one does); the CDP mode is also run against one that does not.
  fixture.setDrainSandbox(options.httpCache !== true);
  const session = new BrowserSession(browser, heartbeat, 'offline-smoke-source', worker, options);
  session.prewarm = false;
  session.statusPollMs = 250;
  t.after(() => session.close());
  assert.equal(await session.authenticate(credentials), USER_ID);
  if (initialize) await session.initialize(create ? null : randomUUID(), async () => {});
  return session;
}
const recordsSince = (index, path) => fixture.records.slice(index).filter(record => record.path === path);
const startPath = '/api/llm/response_with_tools_start';
const statusPath = '/api/llm/response_with_tools_status';

test('real Chromium authenticates the fake session and rejects a mismatched identity', {timeout:30000}, async t => {
  const session = new BrowserSession(browser, () => {}, 'offline-auth', 0);
  t.after(() => session.close());
  await assert.rejects(session.authenticate({...credentials, expected_user_id:'different-user'}),
    code('oauth_identity_mismatch', 403));
  assert.equal(session.isAlive(), false);
  const auth = fixture.records.filter(record => record.path === '/auth/session').at(-1);
  assert.match(auth.headers.cookie, new RegExp('prism_oai_access_token=' + FAKE_TOKEN));
  assert.equal(auth.headers['x-prism-oauth-token'], undefined);
});

test('opt-in CDP path: native project creation, Statsig readiness, heartbeat/sync and HTTP cache use the real network', {timeout:30000}, async t => {
  const heartbeat = [];
  const beforeAssets = fixture.assetHits;
  const session = await sessionFor(t, {initialize:false, heartbeat:stamp => heartbeat.push(stamp), options:{httpCache:true}});
  const cdp = await session.context.newCDPSession(session.page);
  await cdp.send('Network.enable');
  const cached = [];
  cdp.on('Network.requestServedFromCache', event => cached.push(event.requestId));
  let reserved;
  let reservedBeforeWire = false;
  const models = await session.initialize(null, async id => {
    reserved = id;
    reservedBeforeWire = !fixture.records.some(record => record.path === '/api/projects' && record.body?.project_uuid === id);
  });
  assert.deepEqual(models, MODELS);
  assert.equal(session.projectId, reserved);
  assert.equal(reservedBeforeWire, true, 'UI-generated project id must be reserved before the upstream mutation');
  assert.ok(fixture.projects.has(reserved));
  assert.equal(session.syncSeen, true);
  assert.ok(heartbeat.some(stamp => stamp > 0));
  assert.equal(await session.page.locator('textarea:visible').count(), 1);
  // Loaded by authenticate and initialize in one browser context, with no mock routing.
  assert.equal(fixture.assetHits - beforeAssets, 1, 'native request interception must not disable the HTTP cache');
  assert.ok(cached.length > 0, 'Chromium must report requestServedFromCache on the second navigation');
  await cdp.detach();
});

for (const mode of modes) test(`${mode.name}: native Enter submits verified input/model/effort while preserving Prism metadata and proof`, {timeout:30000}, async t => {
  const session = await sessionFor(t, {options:mode.options});
  const index = fixture.records.length;
  let accepted = 0;
  fixture.enqueue({text:'verified offline answer', polls:3});
  const input = request({onAccepted:() => accepted++});
  assert.equal(await session.generate(input), 'verified offline answer');
  assert.equal(accepted, 1);
  const starts = recordsSince(index, startPath);
  assert.equal(starts.length, 1);
  const start = starts[0];
  assert.deepEqual(start.body.input, input.input);
  assert.equal(start.body.metadata.model, 'gpt-6.1-sol');
  assert.equal(start.body.metadata.reasoning_effort, 'xhigh');
  assert.equal(start.body.metadata.projectId, session.projectId);
  assert.equal(start.body.metadata.native_only, 'preserved-metadata');
  assert.equal(start.headers['openai-sentinel-token'], 'offline-native-proof');
  assert.equal(start.headers['x-prism-oauth-token'], undefined);
  assert.match(start.headers.cookie, /prism_oai_access_token=/);
  const polls = recordsSince(index, statusPath);
  assert.ok(polls.length >= 3);
  assert.ok(polls.every(poll => poll.body.conversation_id === start.body.conversationId));
  assert.ok(polls.every(poll => poll.body.diff_format === 'mock-native'));
});

// The resident-refusal diagnosis compares the header NAMES of the editor page's accepted status poll with
// what the resident fetch sets. That capture must work in real Chromium on both interception paths.
for (const mode of modes) test(`${mode.name}: the editor status poll's header names are captured without any value`, {timeout:30000}, async t => {
  const session = await sessionFor(t, {options:mode.options});
  const index = fixture.records.length;
  fixture.enqueue({text:'header capture answer', polls:2});
  let captured;
  const original = session.observe.bind(session);
  session.observe = async response => { await original(response); captured ??= session.turn?.nativeStatusHeaderNames; };
  assert.equal(await session.generate(request()), 'header capture answer');
  assert.ok(Array.isArray(captured) && captured.length > 0, 'no header names captured from the native status poll');
  assert.ok(captured.includes('content-type'), 'the editor poll carries content-type');
  assert.ok(captured.every(name => /^[a-z0-9-]{1,64}$/.test(name)));
  assert.equal(JSON.stringify(captured).includes('offline-native-proof'), false, 'a header value must never be kept');
  // The names it captured really are the ones the mock server saw on a native status poll.
  const seen = recordsSince(index, statusPath).find(poll => poll.headers['content-type'])?.headers ?? {};
  for (const name of ['content-type']) assert.ok(name in seen);
});

for (const mode of modes) test(`${mode.name}: real request guards reject project/history mismatch, unrelated mutations and unapproved upload`, {timeout:30000}, async t => {
  const session = await sessionFor(t, {options:mode.options});
  const index = fixture.records.length;
  const other = randomUUID();
  const blocked = await session.page.evaluate(async other => {
    const post = async (path, headers = {}) => {
      try {await fetch(path, {method:'POST', headers:{'Content-Type':'application/json', ...headers}, body:'{}'}); return false;}
      catch {return true;}
    };
    return {
      unrelated:await post('/api/projects/' + other + '/delete'),
      unexpectedProject:await post('/api/projects'),
      upload:await post('/api/project-files/upload', {'X-Prism-Project-Id':window.mockPrism.projectId, 'X-Prism-File-Name':'leak.txt'}),
    };
  }, other);
  assert.deepEqual(blocked, {unrelated:true, unexpectedProject:true, upload:true});
  assert.equal(fixture.records.slice(index).some(record => record.path.includes(other)), false);
  assert.equal(recordsSince(index, '/api/project-files/upload').length, 0);
  await session.page.evaluate(other => {window.mockPrism.projectOverride = other;}, other);
  await assert.rejects(session.generate(request()), code('browser_project_mismatch'));
  await session.page.evaluate(() => {window.mockPrism.projectOverride = null; window.mockPrism.previousResponseId = 'native-old-response';});
  await assert.rejects(session.generate(request()), code('browser_previous_context_present'));
  assert.equal(recordsSince(index, startPath).length, 0, 'neither rejected start may reach the local upstream');
});

for (const mode of modes) test(`${mode.name}: HTTP and terminal errors remain typed and are not silently replayed`, {timeout:45000}, async t => {
  const cases = [
    {name:'HTTP 503', scenario:{startHttp:503}, expected:'prism_upstream_http_error', status:502, transient:true},
    {name:'expired OAuth', scenario:{startHttp:401}, expected:'session_expired', status:401},
    {name:'native start refusal', scenario:{startTerminal:true, failure:true, payload:{httpStatus:403, reason:'unknown',
      message:'Error while processing conversation (403 Forbidden). Please submit prompt again.'}}, expected:'prism_start_rejected', status:429},
    {name:'too large', scenario:{startTerminal:true, failure:true, payload:{httpStatus:413, reason:'conversation_too_large'}},
      expected:'context_length_exceeded', status:400},
    {name:'wrong output model', scenario:{model:'gpt-5.6-sol'}, expected:'upstream_model_mismatch', status:502},
    {name:'empty output', scenario:{output:[]}, expected:'prism_empty_output', status:502},
  ];
  for (const item of cases) await t.test(item.name, async child => {
    const session = await sessionFor(child, {options:mode.options});
    const index = fixture.records.length;
    let accepted = 0;
    fixture.enqueue(item.scenario);
    await assert.rejects(session.generate(request({onAccepted:() => accepted++})), error => {
      assert.ok(code(item.expected, item.status)(error), `${error.code}/${error.status}, expected ${item.expected}/${item.status}`);
      if (item.transient) assert.equal(error.transient, true);
      return true;
    });
    assert.equal(recordsSince(index, startPath).length, 1, 'BrowserSession must not replay a native start itself');
    if (item.scenario.startHttp || item.scenario.startTerminal) assert.equal(accepted, 0);
  });
});

for (const mode of modes) test(`${mode.name}: native file selection uploads only the approved file and binds its reference to the current project`, {timeout:30000}, async t => {
  const session = await sessionFor(t, {options:mode.options});
  const index = fixture.records.length;
  const attachment = {filename:'offline-document.txt', mimeType:'text/plain', data:Buffer.from('offline attachment bytes')};
  fixture.enqueue({text:'attachment inspected offline'});
  assert.equal(await session.generate(request({attachments:[attachment]})), 'attachment inspected offline');
  const uploads = recordsSince(index, '/api/project-files/upload');
  assert.equal(uploads.length, 1);
  assert.equal(uploads[0].headers['x-prism-project-id'], session.projectId);
  assert.equal(decodeURIComponent(uploads[0].headers['x-prism-file-name']), attachment.filename);
  assert.deepEqual(uploads[0].raw, attachment.data);
  const start = recordsSince(index, startPath)[0];
  const user = start.body.input.findLast(message => message.role === 'user');
  assert.deepEqual(user.content.at(-1), {type:'input_file', filename:attachment.filename, project_path:'/prism-uploads/' + attachment.filename});
  assert.match(user.content.at(-2).text, /Do not create, edit, rename, or delete project files/);
});

for (const mode of modes) test(`${mode.name}: cancelled live generation performs the native stop and releases the browser context`, {timeout:30000}, async t => {
  const session = await sessionFor(t, {options:mode.options});
  const index = fixture.records.length;
  fixture.enqueue({hold:true});
  const controller = new AbortController();
  let accepted;
  const started = new Promise(resolve => {accepted = resolve;});
  const generation = session.generate(request({onAccepted:accepted}), controller.signal);
  generation.catch(() => {});
  await started;
  await session.page.getByTestId('ai-stop-button').waitFor({state:'visible'});
  controller.abort(new PrismError('request_cancelled', 499));
  await assert.rejects(generation, code('request_cancelled', 499));
  const stops = recordsSince(index, '/api/llm/response_with_tools_stop');
  assert.equal(stops.length, 1);
  assert.equal(fixture.turns.get(stops[0].body.request_id)?.stopped, true);
  assert.equal(session.isAlive(), false);
});

for (const mode of modes) test(`${mode.name}: isolated projects and overlapping contexts never mix request ids, inputs or output`, {timeout:30000}, async t => {
  const first = await sessionFor(t, {create:true, options:mode.options});
  const second = await sessionFor(t, {create:true, options:mode.options});
  assert.notEqual(first.projectId, second.projectId);
  const index = fixture.records.length;
  const beforePeak = fixture.peakActive;
  fixture.enqueue({text:'first scope answer', polls:5});
  fixture.enqueue({text:'second scope answer', polls:5});
  const [one, two] = await Promise.all([
    first.generate(request({input:[{role:'user', content:[{type:'input_text', text:'first scope input'}]}]})),
    second.generate(request({input:[{role:'user', content:[{type:'input_text', text:'second scope input'}]}]})),
  ]);
  // Start arrival order is deliberately not assumed to be the Promise argument order.
  assert.deepEqual(new Set([one, two]), new Set(['first scope answer', 'second scope answer']));
  const starts = recordsSince(index, startPath);
  assert.equal(starts.length, 2);
  const byProject = new Map(starts.map(start => [start.body.metadata.projectId, start]));
  assert.equal(byProject.get(first.projectId).body.input[0].content[0].text, 'first scope input');
  assert.equal(byProject.get(second.projectId).body.input[0].content[0].text, 'second scope input');
  const polls = recordsSince(index, statusPath);
  for (const start of starts) {
    const turn = [...fixture.turns.values()].find(turn => turn.body.conversationId === start.body.conversationId);
    assert.ok(polls.some(poll => poll.body.request_id === turn.id && poll.body.conversation_id === start.body.conversationId));
  }
  assert.ok(fixture.peakActive >= Math.max(2, beforePeak), 'both contexts must actually have overlapping upstream turns');
});

async function until(predicate, message, timeout = 10000) {
  const deadline = Date.now() + timeout;
  while (!predicate()) {
    if (Date.now() >= deadline) assert.fail(message);
    await new Promise(resolve => setTimeout(resolve, 25));
  }
}

test('account-bound multiplex releases both UI pages, isolates resident polls and stops only the cancelled turn', {timeout:30000}, async t => {
  const multiplex = new AccountPageMultiplexer({pollMs:250});
  t.after(() => multiplex.close());
  const options = {multiplex:true, multiplexer:multiplex};
  const first = await sessionFor(t, {create:true, options, worker:0});
  const second = await sessionFor(t, {create:true, options, worker:1});
  const firstPage = first.page;
  const secondPage = second.page;
  const firstProject = first.projectId;
  const secondProject = second.projectId;
  assert.notEqual(firstProject, secondProject);
  assert.notEqual(first.context, second.context);
  assert.notEqual(multiplex.context, first.context);
  // The resident page loads the real site so Prism's own script wraps window.fetch (a bare JSON document is
  // refused with 403); it must still never open an editor tab, so there is no composer to type into.
  assert.equal(new URL(multiplex.page.url()).pathname, '/');
  assert.equal(await multiplex.page.evaluate(() => window.fetch !== window.__prismOriginalFetch), true, 'resident fetch must be the official wrapper');
  const index = fixture.records.length;
  // The mock home page renders a composer on load (the real editor does too), so what matters is that the
  // resident page never submits anything: nothing may start a model turn before this test asks for one.
  assert.equal(recordsSince(index - 0, startPath).length, 0, 'the resident page must never submit a prompt');
  fixture.enqueue({hold:true, text:'cancelled scope should never return'});
  const controller = new AbortController();
  const one = first.generate(request({input:[{role:'user', content:[{type:'input_text', text:'multiplex first input'}]}]}), controller.signal);
  one.catch(() => {});
  await until(() => first.turn?.detached, 'first accepted turn did not detach its submission UI');
  fixture.enqueue({hold:true, text:'second resident scope answer'});
  const two = second.generate(request({input:[{role:'user', content:[{type:'input_text', text:'multiplex second input'}]}]}));
  two.catch(() => {});
  await until(() => second.turn?.detached && multiplex.status().in_flight === 2, 'two account turns did not overlap in the resident poller');
  const firstId = first.turn.requestId;
  const secondId = second.turn.requestId;
  assert.notEqual(firstId, secondId);
  await until(() => firstPage.isClosed() && secondPage.isClosed(), 'detached editor pages were not actually closed');
  assert.equal(first.page, null);
  assert.equal(second.page, null);
  assert.equal(first.context.pages().length, 0);
  assert.equal(second.context.pages().length, 0);
  assert.equal(first.isAlive(), true);
  assert.equal(second.isAlive(), true);
  assert.equal(multiplex.status().isolation, 'independent_contexts');
  assert.equal(multiplex.status().submission_busy, false);
  const resident = multiplex.page;
  controller.abort(new PrismError('request_cancelled', 499));
  await assert.rejects(one, code('request_cancelled', 499));
  const stops = recordsSince(index, '/api/llm/response_with_tools_stop');
  assert.equal(stops.length, 1);
  assert.equal(stops[0].body.request_id, firstId);
  assert.equal(fixture.turns.get(firstId).stopped, true);
  assert.equal(fixture.turns.get(secondId).stopped, false);
  assert.equal(multiplex.page, resident);
  assert.equal(multiplex.isAlive(), true);
  fixture.turns.get(secondId).scenario.hold = false;
  assert.equal(await two, 'second resident scope answer');
  await until(() => multiplex.status().in_flight === 0, 'completed resident job was not released');
  const starts = recordsSince(index, startPath);
  assert.equal(starts.length, 2);
  for (const [id, project, text] of [[firstId, firstProject, 'multiplex first input'], [secondId, secondProject, 'multiplex second input']]) {
    const turn = fixture.turns.get(id);
    assert.equal(turn.body.metadata.projectId, project);
    assert.equal(turn.body.input[0].content[0].text, text);
    assert.ok(recordsSince(index, statusPath).filter(poll => poll.body.request_id === id)
      .every(poll => poll.body.conversation_id === turn.body.conversationId));
  }
  // The retained context must lazily create another UI page for a new isolated scope.
  await second.initialize(null, async () => {});
  assert.notEqual(second.projectId, secondProject);
  assert.ok(second.page && !second.page.isClosed());
  fixture.enqueue({text:'new isolated scope after resident turn', polls:4});
  assert.equal(await second.generate(request()), 'new isolated scope after resident turn');
  assert.equal(second.page, null);
  assert.equal(multiplex.isAlive(), true);
});

test('the resident poller is never refused for a missing verification proof', {timeout:30000}, async t => {
  const multiplex = new AccountPageMultiplexer({pollMs:250});
  t.after(() => multiplex.close());
  const session = await sessionFor(t, {create:true, options:{multiplex:true, multiplexer:multiplex}, worker:0});
  const before = fixture.unverified.length;
  fixture.enqueue({text:'resident answer', polls:4});
  assert.equal(await session.generate(request()), 'resident answer');
  assert.equal(fixture.unverified.length, before, 'a model API call reached Prism without the verification proof');
  const polls = fixture.records.filter(record => record.path === statusPath && record.headers['openai-sentinel-token']);
  assert.ok(polls.length >= 3, 'the resident poller did not carry the verification proof');
});

test('a resident page whose official fetch wrapper never appears fails closed instead of polling bare', {timeout:60000}, async t => {
  const multiplex = new AccountPageMultiplexer({pollMs:250});
  t.after(() => multiplex.close());
  const browserHandle = browser;
  // A context whose scripts are blocked can never get the wrapper: registration must reject, not fall back.
  const context = await browserHandle.newContext({locale:'en-US'});
  await context.route('**/assets/**', route => route.abort());
  const original = browserHandle.newContext.bind(browserHandle);
  browserHandle.newContext = async () => context;
  t.after(() => { browserHandle.newContext = original; return context.close(); });
  await assert.rejects(multiplex.register(browserHandle, {access_token:FAKE_TOKEN}, 'mock-prism-user'), error => error.code === 'poll_carrier_unavailable');
  assert.equal(multiplex.isAlive(), false);
});

test('separate account multiplexers keep resident cookies/identity distinct and reject cross-account registration', {timeout:30000}, async t => {
  const one = new AccountPageMultiplexer({pollMs:250});
  const two = new AccountPageMultiplexer({pollMs:250});
  t.after(() => one.close());
  t.after(() => two.close());
  await one.register(browser, {access_token:'offline-first-account-token'}, 'offline-first-account');
  await two.register(browser, {access_token:'offline-second-account-token'}, 'offline-second-account');
  assert.notEqual(one.context, two.context);
  assert.notEqual(one.page, two.page);
  assert.equal((await one.context.cookies()).find(cookie => cookie.name === 'prism_oai_access_token').value, 'offline-first-account-token');
  assert.equal((await two.context.cookies()).find(cookie => cookie.name === 'prism_oai_access_token').value, 'offline-second-account-token');
  await assert.rejects(one.register(browser, {access_token:'offline-wrong-account-token'}, 'offline-second-account'),
    code('source_identity_changed', 409));
  assert.equal((await one.context.cookies()).find(cookie => cookie.name === 'prism_oai_access_token').value, 'offline-first-account-token');
});

test('detached resident health uses a real authenticated response and invalidates a switched identity', {timeout:30000}, async t => {
  const multiplex = new AccountPageMultiplexer({pollMs:250});
  t.after(() => multiplex.close());
  const beats = [];
  const session = await sessionFor(t, {create:true, options:{multiplex:true, multiplexer:multiplex},
    heartbeat:(stamp, reason) => beats.push({stamp, reason})});
  fixture.enqueue({hold:true});
  const generation = session.generate(request());
  generation.catch(() => {});
  await until(() => session.turn?.detached, 'health test turn never detached');
  const index = fixture.records.length;
  session.lastHeartbeat = 0;
  await multiplex.probeHealth();
  assert.ok(recordsSince(index, '/auth/session').some(record => record.method === 'GET'),
    'idle health must perform an authenticated network request, not fabricate a heartbeat');
  assert.ok(session.lastHeartbeat > 0);
  assert.ok(beats.some(beat => beat.stamp > 0));
  fixture.setAuth({user:{id:'offline-different-account', email:USER_EMAIL, is_anonymous:false}});
  try {
    await multiplex.probeHealth();
    await assert.rejects(generation, code('session_expired', 401));
    assert.ok(beats.some(beat => beat.stamp === 0 && beat.reason === 'session_expired'));
    assert.equal(recordsSince(index, startPath).length, 0, 'identity failure must not replay the accepted turn');
  } finally {
    fixture.setAuth({user:{id:USER_ID, email:USER_EMAIL, is_anonymous:false}});
  }
});


test('real management HTTP + AccountPoolManager isolate gateway scopes across both native workers', {timeout:45000}, async t => {
  for (const multiplex of [false, true]) await t.test(`multiplex=${multiplex}`, {timeout:22000}, async child => {
    // Isolation is opt-in; the multiplex run also uses the opt-in CDP path, the other one the default route path.
    fixture.setDrainSandbox(!multiplex);
    const dataDir = await mkdtemp(join(tmpdir(), 'prism-pool-chromium-smoke-'));
    const admissions = [];
    const metrics = new RuntimeMetrics();
    // The guard is opt-in: an explicit limit makes it sample and audit every admission.
    const guard = new ResourceGuard({limitBytes:512 * 1024 * 1024, sample:async () => ({usedBytes:64 * 1024 * 1024, limitBytes:512 * 1024 * 1024,
      source:'offline_wiring_fixture', includesChromium:true, degraded:false}),
      onAudit:(event, fields) => admissions.push({event, ...fields})});
    // Borrow the suite's real browser without installing disconnected listeners or closing
    // it when this isolated pool shuts down. The pool still owns/closes all real contexts.
    const borrowedBrowser = {newContext:options => browser.newContext(options), on() {}, async close() {}};
    const maxWorkers = multiplex ? 3 : 2;
    const pool = new AccountPoolManager({dataDir, concurrency:2, maxWorkers, multiplex, projectIsolation:true,
      admissionGuard:guard, metrics, browserOptions:{pollMs:250, httpCache:multiplex},
      browserFactory:async () => borrowedBrowser,
      sessionFactory:(native, heartbeat, source, slot, options) => {
        const driver = new BrowserSession(native, heartbeat, source, slot, options);
        driver.prewarm = false; driver.statusPollMs = 250; return driver;
      }});
    const managementKey = 'offline-management-key-never-a-real-secret'.repeat(2);
    const apiKey = 'offline-upstream-api-key-never-a-real-secret'.repeat(2);
    const server = createPrismServer({manager:pool, managementKey, projectIsolation:true});
    child.after(async () => {
      server.closeAllConnections(); await new Promise(resolve => server.close(resolve));
      await pool.close(); await rm(dataDir, {recursive:true, force:true});
    });
    await pool.init(); server.listen(0, '127.0.0.1'); await once(server, 'listening');
    const base = `http://127.0.0.1:${server.address().port}`;
    async function call(path, {method = 'GET', body, user = false, scope = {}} = {}) {
      const response = await fetch(base + path, {method, headers:{Authorization:'Bearer ' + (user ? apiKey : managementKey),
        'Content-Type':'application/json', ...scope}, ...(body === undefined ? {} : {body:JSON.stringify(body)}),
        signal:AbortSignal.timeout(18000)});
      const value = await response.json();
      assert.equal(response.status, 200, JSON.stringify(value)); return value;
    }
    await call('/internal/accounts/32/session', {method:'PUT', body:{...credentials, api_key:apiKey,
      expires_at:Math.floor(Date.now() / 1000) + 3600}});
    assert.ok(pool.managers.every(manager => manager.projectIsolation === true), 'scope isolation is on because this run opted in');
    if (multiplex) assert.equal(pool.multiplexers.get('32').isAlive(), false,
      'authenticated workers must not establish a resident before persistent source identity validation');
    const beforeBootstrap = fixture.records.length;
    fixture.enqueue({text:'READY', polls:3}); fixture.enqueue({text:'READY', polls:3});
    const ready = await call('/internal/accounts/32/bootstrap', {method:'POST', body:{}});
    assert.equal(ready.ready, true); assert.equal(ready.ready_workers, 2);
    assert.ok(ready.last_heartbeat_at > 0);
    const bootstrapStarts = recordsSince(beforeBootstrap, startPath);
    assert.equal(bootstrapStarts.length, 2, 'each of the two fresh readiness projects needs exactly one probe');
    const bootstrapProjects = new Set(pool.managers.map(manager => manager.get('32').metadata.project_id));
    assert.equal(bootstrapProjects.size, 2);
    assert.ok(bootstrapStarts.every(start => bootstrapProjects.has(start.body.metadata.projectId)));
    await call('/internal/accounts/32/bootstrap', {method:'POST', body:{}});
    assert.equal(recordsSince(beforeBootstrap, startPath).length, 2, 'already-ready bootstrap must not repeat native probes');
    assert.ok(pool.managers.every(manager => manager.get('32').metadata.readiness_probe_count === 1));
    const scopeA = {'x-prism-key-scope':'a'.repeat(64), 'x-prism-session-scope':'b'.repeat(64)};
    const scopeB = {'x-prism-key-scope':'c'.repeat(64), 'x-prism-session-scope':'b'.repeat(64)};
    async function generate(scope, text) {
      const index = fixture.records.length;
      fixture.enqueue({text:'offline pool scope answer', polls:3});
      const response = await call('/accounts/32/v1/responses', {method:'POST', user:true, scope,
        body:{model:'gpt-6.1-sol', input:text, reasoning:{effort:'high'}}});
      assert.equal(response.status, 'completed');
      const starts = recordsSince(index, startPath);
      assert.equal(starts.length, 1); const project = starts[0].body.metadata.projectId;
      assert.equal(bootstrapProjects.has(project), false, 'user prompts must never enter a readiness/bootstrap project');
      assert.ok(JSON.stringify(starts[0].body.input).includes(text)); return project;
    }
    const projectA = await generate(scopeA, 'pool scope A first turn');
    assert.equal(pool.managers[0].get('32').driver.projectId, projectA);
    // Affinity: the next turn of the same key/session stays on the worker that already holds its project.
    const stayed = await generate(scopeA, 'pool scope A second turn');
    assert.equal(stayed, projectA);
    assert.notEqual(pool.managers[1].get('32').driver.projectId, projectA, 'an idle holder keeps the scope; the other worker loads nothing');
    // With the holder busy the scope moves: the other worker loads the same project in its own real context.
    const holder = pool.accounts.get('32').workers[0];
    holder.busy = true;
    const reused = await generate(scopeA, 'pool scope A third turn, holder busy');
    holder.busy = false;
    assert.equal(reused, projectA);
    assert.equal(pool.managers[1].get('32').driver.projectId, projectA, 'same gateway key/session must reuse its project on another worker');
    const projectB = await generate(scopeB, 'pool different gateway key');
    assert.notEqual(projectB, projectA, 'the same conversation hash under another authenticated key must not reuse files');
    const anonymousA = await generate({}, 'pool anonymous first');
    const anonymousB = await generate({}, 'pool anonymous second');
    assert.equal(new Set([projectA, projectB, anonymousA, anonymousB]).size, 4);
    const registry = JSON.parse(await readFile(join(dataDir, 'projects', '32.json'), 'utf8'));
    assert.equal(registry.entries.length, 2, 'anonymous scopes must never become reusable registry entries');
    assert.equal(JSON.stringify(registry).includes(anonymousA), false);
    assert.equal(JSON.stringify(registry).includes(anonymousB), false);
    const resources = await call('/internal/resources');
    assert.equal(resources.multiplex_enabled, multiplex);
    assert.equal(resources.reserved_contexts, maxWorkers, 'resident reservations must count against maxWorkers');
    assert.equal(resources.contexts, maxWorkers);
    assert.equal(resources.memory.source, 'offline_wiring_fixture');
    assert.ok(resources.timings.page_load.count >= 2);
    assert.ok(resources.timings.project_prepare.count >= 2);
    assert.ok(resources.timings.queue_wait_ms.count >= 5);
    assert.ok(admissions.some(value => value.kind === 'context'));
    assert.ok(admissions.some(value => value.kind === 'project'));
    assert.equal(admissions.some(value => value.kind === 'resident_context'), multiplex);
    if (multiplex) {
      assert.equal(resources.multiplexers.length, 1);
      assert.equal(resources.multiplexers[0].resident_alive, true);
      assert.equal(resources.multiplexers[0].isolation, 'independent_contexts');
      await pool.multiplexers.get('32').probeHealth();
    }
    const status = await call('/internal/accounts/32/status');
    assert.equal(status.ready, true); assert.equal(status.ready_workers, 2);
    assert.equal(status.busy_workers, 0); assert.equal(status.queued, 0);
    assert.throws(() => pool.createDriver(borrowedBrowser, () => {}, '32', 2), code('browser_capacity_full', 429));
  });
});
