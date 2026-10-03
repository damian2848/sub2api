import test from 'node:test';
import assert from 'node:assert/strict';
import { BrowserSession } from '../src/browser.mjs';
import { AccountPageMultiplexer, multiplexEnabled } from '../src/page-multiplexer.mjs';

const tick = () => new Promise(resolve => setImmediate(resolve));
const request = { model: 'gpt-6.1-sol', effort: 'high', input: [] };
const project = '01234567-89ab-4cde-8123-0123456789ab';
const running = id => ({ status: 'running', request_id: id, turn_state: `state-${id}` });
const complete = id => ({ ...running(id), status: 'completed', response: { status: 'success', payload: {
  output: [{ type: 'message', content: [{ type: 'output_text', text: `RESULT ${id}` }] }] } } });

test('page multiplex is opt-in and leaves default workers untouched', () => {
  assert.equal(multiplexEnabled(undefined), false); assert.equal(multiplexEnabled('false'), false);
  assert.equal(multiplexEnabled('true'), true); assert.equal(multiplexEnabled('ON'), true);
  assert.equal(new BrowserSession({}, () => {}).multiplex, false);
});

test('account submission admission is serial, cancelled waiters do not hold its lease', async () => {
  const mux = new AccountPageMultiplexer(); const release = await mux.acquireSubmission();
  const controller = new AbortController(); const pending = mux.acquireSubmission(controller.signal);
  controller.abort(); await assert.rejects(pending, error => error.code === 'request_cancelled');
  let admitted = false; const next = mux.acquireSubmission().then(value => { admitted = true; return value; });
  await tick(); assert.equal(admitted, false); release(); (await next)();
  assert.equal(mux.status().submission_busy, false); await mux.close();
});

test('shared resident registration is single-flight and rejects other account identity', async () => {
  let contexts = 0; let closes = 0;
  const page = { goto: async () => {}, addInitScript: async () => {}, waitForFunction: async () => {}, isClosed: () => false, on() {} };
  const browser = { newContext: async () => { contexts += 1; return { addCookies: async () => {}, newPage: async () => page, close: async () => { closes += 1; } }; } };
  const mux = new AccountPageMultiplexer();
  await Promise.all([mux.register(browser, { access_token: 'token' }, 'account-a'), mux.register(browser, { access_token: 'token' }, 'account-a')]);
  assert.equal(contexts, 1);
  await assert.rejects(mux.register(browser, { access_token: 'other' }, 'account-b'), error => error.code === 'source_identity_changed');
  await mux.close(); assert.equal(closes, 1);
});

test('resident polling multiplexes distinct in-flight turns without mixing their output or state', async () => {
  const mux = new AccountPageMultiplexer({ pollMs: 250 });
  const observed = [];
  mux.context = {}; mux.page = { isClosed: () => false, evaluate: async (_, { body }) => {
    const input = JSON.parse(body); observed.push(input); return { status: 200, text: JSON.stringify(complete(input.request_id)) }; } };
  const turns = ['one', 'two'].map(id => {
    const driver = new BrowserSession({}, () => {}, 'account', id === 'one' ? 0 : 1, { multiplex: true, multiplexer: mux });
    const turn = { started: true, requestId: id, conversationId: `conversation-${id}`, turnState: `state-${id}`,
      statusTemplate: { diff_format: 'dense', conversation_id: `conversation-${id}` }, request,
      ownBodies: new Set(), ownPolls: 0, ownPollErrors: 0, ownPollErrorTotal: 0, reject: error => assert.fail(error.code) };
    driver.turn = turn;
    const result = new Promise(resolve => { turn.resolve = resolve; });
    mux.startPolling(driver, turn); return { turn, result };
  });
  assert.deepEqual(await Promise.all(turns.map(item => item.result)), ['RESULT one', 'RESULT two']);
  assert.deepEqual(observed.map(item => item.turn_state), ['state-one', 'state-two']);
  await tick(); assert.equal(mux.status().in_flight, 0);
});

test('first accepted native status closes UI only after Enter returns and keeps context alive', async () => {
  let closed = 0; let polled = 0; let released = 0;
  const mux = { acquireSubmission: async () => () => { released += 1; }, startPolling() { polled += 1; },
    isAlive: () => true, cancel() {} };
  const driver = new BrowserSession({}, () => {}, 'account', 0, { multiplex: true, multiplexer: mux });
  driver.context = {}; driver.projectId = project; driver.labels.set(request.model, '6.1 Sol');
  const page = { isClosed: () => closed > 0, close: async () => { closed += 1; }, getByRole: () => ({ click: async () => {} }) };
  driver.page = page;
  let accepted;
  driver.composer = async () => ({ fill: async () => {}, press: async () => {
    const body = { conversationId: 'conversation', metadata: { projectId: project } };
    const nativeRequest = { url: () => 'https://prism.openai.com/api/llm/response_with_tools_start', method: () => 'POST', postDataJSON: () => body };
    await driver.route({ request: () => nativeRequest, continue: async () => {}, abort: () => assert.fail('accepted') });
    await driver.observe({ url: nativeRequest.url, request: () => nativeRequest, ok: () => true, json: async () => running('one') });
    const statusBody = { request_id: 'one', conversation_id: 'conversation', diff_format: 'dense' };
    const statusRequest = { url: () => 'https://prism.openai.com/api/llm/response_with_tools_status', postDataJSON: () => statusBody, postData: () => JSON.stringify(statusBody) };
    await driver.observe({ url: statusRequest.url, request: () => statusRequest, ok: () => true, json: async () => running('one') });
    assert.equal(closed, 0, 'observe must not race the Enter completion'); accepted = driver.turn;
  } });
  const result = driver.generate(request);
  await tick();
  assert.equal(closed, 1); assert.equal(polled, 1); assert.equal(released, 1); assert.equal(driver.page, null); assert.equal(driver.isAlive(), true);
  accepted.resolve('DONE'); assert.equal(await result, 'DONE'); assert.equal(released, 1);
});

test('cancelling a detached turn uses resident stop without terminating other turns', async () => {
  const calls = []; const mux = { stop: async turn => { calls.push(turn.requestId); return true; }, cancel: turn => calls.push(`cancel-${turn.requestId}`), isAlive: () => true };
  const driver = new BrowserSession({}, () => {}, 'account', 0, { multiplex: true, multiplexer: mux });
  driver.context = {}; driver.turn = { started: true, detached: true, requestId: 'one' };
  await driver.stop(); assert.deepEqual(calls, ['one', 'cancel-one']); assert.equal(driver.context !== null, true);
});

test('idle detached workers survive the sixty-second heartbeat window only after authenticated resident health', async () => {
  let now = 100000; let healthy = true;
  const mux = new AccountPageMultiplexer({ now: () => now }); mux.identity = 'same-account'; mux.context = {};
  mux.page = { isClosed: () => false, evaluate: async () => healthy
    ? { ok: true, status: 200, id: 'same-account', anonymous: false } : { ok: false, status: 0 } };
  const beats = []; const driver = new BrowserSession({}, stamp => beats.push(stamp), 'source', 0, { multiplex: true, multiplexer: mux });
  driver.context = {}; driver.page = null; driver.lastHeartbeat = 100; mux.sessions.add(driver);
  now += 75000; await mux.probeHealth();
  assert.equal(driver.lastHeartbeat, 175); assert.equal(now / 1000 - driver.lastHeartbeat < 60, true);
  healthy = false; now += 75000; await mux.probeHealth();
  assert.equal(driver.lastHeartbeat, 175, 'failed network health must not manufacture a heartbeat');
  assert.equal(now / 1000 - driver.lastHeartbeat > 60, true); assert.deepEqual(beats, [175]);
});

test('resident health identity change revokes detached workers and cannot refresh their heartbeat', async () => {
  const mux = new AccountPageMultiplexer({ now: () => 999000 }); mux.identity = 'same-account'; mux.context = {};
  mux.page = { isClosed: () => false, evaluate: async () => ({ ok: true, status: 200, id: 'different-account' }) };
  const beats = []; const driver = { context: {}, page: null, lastHeartbeat: 100, onHeartbeat: (...args) => beats.push(args) };
  mux.sessions.add(driver); let code;
  mux.jobs.set({ reject: error => { code = error.code; } }, {});
  await mux.probeHealth(); assert.equal(driver.lastHeartbeat, 100); assert.deepEqual(beats, [[0, 'session_expired']]); assert.equal(code, 'session_expired');
});

test('resident stop failure closes only the affected submission context', async () => {
  let contextsClosed = 0; let pollerClosed = 0;
  const mux = { stop: async () => false, cancel() {}, unregister() {}, close() { pollerClosed += 1; }, isAlive: () => true };
  const driver = new BrowserSession({}, () => {}, 'source', 0, { multiplex: true, multiplexer: mux });
  driver.context = { close: async () => { contextsClosed += 1; } };
  driver.turn = { started: true, detached: true, requestId: 'one', reject() {} };
  await driver.stop(); assert.equal(contextsClosed, 1); assert.equal(pollerClosed, 0); assert.equal(driver.context, null);
});

function authenticationBrowser({ identity = 'fixture-user', evaluate } = {}) {
  let contexts = 0;
  const page = { route: async () => {}, on() {}, goto: async () => {}, addInitScript: async () => {}, waitForFunction: async () => {}, isClosed: () => false,
    evaluate: evaluate || (async () => ({ signedIn: true, actualUserId: identity, emailMatches: true, idMatches: true })) };
  return { get contexts() { return contexts; }, on() {}, close: async () => {},
    newContext: async () => { contexts += 1; return { addCookies: async () => {}, newPage: async () => page, close: async () => {} }; } };
}

test('a provision rejected by persisted source identity never creates or poisons a resident poller', async t => {
  const { AccountManager } = await import('../src/accounts.mjs');
  const { createHash } = await import('node:crypto');
  const { mkdtemp, rm } = await import('node:fs/promises');
  const { join } = await import('node:path');
  const { tmpdir } = await import('node:os');
  const directory = await mkdtemp(join(tmpdir(), 'prism-mux-identity-'));
  const browser = authenticationBrowser({ identity: 'rejected-account-b' });
  const mux = new AccountPageMultiplexer();
  const manager = new AccountManager({ dataDir: directory, browserFactory: async () => browser,
    sessionFactory: (browser, heartbeat, source) => new BrowserSession(browser, heartbeat, source, 0, { multiplex: true, multiplexer: mux }) });
  t.after(async () => { await manager.close(); await mux.close(); await rm(directory, { recursive: true, force: true }); });
  await manager.init();
  manager.get('12', true).metadata.identity_hash = createHash('sha256').update('original-account-a').digest('hex');
  await assert.rejects(manager.provision('12', { access_token: 'fixture-token-'.repeat(3), api_key: 'fixture-key-'.repeat(4),
    expected_user_id: 'rejected-account-b', expires_at: Math.floor(Date.now() / 1000) + 600 }), error => error.code === 'source_identity_changed');
  assert.equal(browser.contexts, 1, 'only the short-lived authentication submission context exists');
  assert.equal(mux.identity, null); assert.equal(mux.isAlive(), false); assert.equal(mux.sessions.size, 0);
});

test('cancelled late authentication cannot register a resident or restore the rejected identity', async () => {
  let reach; let finish;
  const reached = new Promise(resolve => { reach = resolve; });
  const gate = new Promise(resolve => { finish = resolve; });
  const browser = authenticationBrowser({ evaluate: async () => { reach(); await gate;
    return { signedIn: true, actualUserId: 'late-user', emailMatches: true, idMatches: true }; } });
  const mux = new AccountPageMultiplexer();
  const driver = new BrowserSession(browser, () => {}, '12', 0, { multiplex: true, multiplexer: mux });
  const controller = new AbortController(); const authentication = driver.authenticate({ access_token: 'fake-token' }, controller.signal);
  await reached; controller.abort(); await assert.rejects(authentication, error => error.code === 'request_cancelled');
  finish(); await tick();
  assert.equal(browser.contexts, 1); assert.equal(mux.identity, null); assert.equal(mux.sessions.size, 0); assert.equal(driver.authIdentity, null);
  await mux.close();
});

test('cancelling a late resident registration releases preparation and removes its stale session', async () => {
  let reach; let finish; let releases = 0;
  const reached = new Promise(resolve => { reach = resolve; });
  const gate = new Promise(resolve => { finish = resolve; });
  const members = new Set();
  const mux = { acquireSubmission: async () => () => { releases += 1; },
    register: async (_, __, ___, driver) => { reach(); await gate; members.add(driver); },
    unregister: driver => members.delete(driver), isAlive: () => true };
  const browser = authenticationBrowser();
  const driver = new BrowserSession(browser, () => {}, '12', 0, { multiplex: true, multiplexer: mux });
  await driver.authenticate({ access_token: 'fake-token' });
  const controller = new AbortController(); const initialization = driver.initialize(null, async () => {}, controller.signal);
  await reached; controller.abort();
  await assert.rejects(initialization, error => error.code === 'request_cancelled');
  assert.equal(releases, 1); assert.equal(driver.context, null);
  finish(); await tick(); assert.equal(members.size, 0); assert.equal(driver.context, null);
});

test('a refused resident poll records only status, error code and cookie names, once per turn', async () => {
  const audits = [];
  const mux = new AccountPageMultiplexer({ pollMs: 250, onAudit: (event, fields) => audits.push({ event, ...fields }) });
  mux.context = { cookies: async () => [{ name: 'prism_oai_access_token', value: 'SECRET-TOKEN-VALUE' }, { name: '_dd_s', value: 'x' }] };
  let calls = 0;
  mux.page = { isClosed: () => false, url: () => 'https://prism.openai.com/auth/session',
    evaluate: async () => { calls += 1; return { status: 401, text: JSON.stringify({ error: { code: 'unauthorized', message: 'SECRET message' } }) }; } };
  const driver = new BrowserSession({}, () => {}, 'account', 0, { multiplex: true, multiplexer: mux });
  let rejected;
  const turn = { started: true, requestId: 'one', conversationId: 'c', turnState: 's', statusTemplate: { diff_format: 'dense' }, request,
    ownBodies: new Set(), ownPolls: 0, ownPollErrors: 0, ownPollErrorTotal: 0, reject: error => { rejected = error; } };
  driver.turn = turn;
  mux.startPolling(driver, turn);
  await new Promise(resolve => setTimeout(resolve, 50));
  assert.equal(rejected.code, 'session_expired');
  const refused = audits.filter(item => item.event === 'resident_poll_refused');
  assert.equal(refused.length, 1);
  assert.deepEqual(refused[0], { event: 'resident_poll_refused', path: '/api/llm/response_with_tools_status', status: 401,
    code: 'unauthorized', resident_cookie_names: ['_dd_s', 'prism_oai_access_token'], resident_url_origin: 'same_origin',
    editor_poll_header_names: undefined, editor_only_header_names: undefined, resident_set_header_names: ['content-type'] });
  assert.equal(JSON.stringify(audits).includes('SECRET'), false, 'neither a token value nor an upstream message may be logged');
});

test('a resident refusal diagnosis never breaks the poll when cookies cannot be read', async () => {
  const audits = [];
  const mux = new AccountPageMultiplexer({ onAudit: (event, fields) => audits.push({ event, ...fields }) });
  mux.context = { cookies: async () => { throw new Error('closed'); } };
  mux.page = { isClosed: () => false, url: () => '' };
  await mux.diagnoseRefusal('/api/llm/response_with_tools_status?x=1', { status: 403, text: 'not json' });
  assert.equal(audits.length, 1);
  assert.equal(audits[0].path, '/api/llm/response_with_tools_status');
  assert.equal(audits[0].code, undefined);
  assert.equal(audits[0].resident_cookie_names, undefined);
  assert.equal(audits[0].resident_url_origin, 'other');
});

test('headerNames keeps only validated lower-case names and never a value', async () => {
  const { headerNames } = await import('../src/browser.mjs');
  const request = { headers: () => ({ 'OpenAI-Sentinel-Token': 'SECRET-PROOF', Authorization: 'Bearer SECRET', 'x-prism-device': 'dev', 'bad name!': 'x', '': 'y' }) };
  const names = headerNames(request);
  assert.deepEqual(names, ['authorization', 'openai-sentinel-token', 'x-prism-device']);
  assert.equal(JSON.stringify(names).includes('SECRET'), false);
  assert.equal(headerNames({}), undefined);
  assert.equal(headerNames({ headers: () => { throw new Error('closed'); } }), undefined);
  assert.equal(headerNames(undefined), undefined);
});

test('a resident refusal reports which header names only the editor poll carried', async () => {
  const audits = [];
  const mux = new AccountPageMultiplexer({ pollMs: 250, onAudit: (event, fields) => audits.push({ event, ...fields }) });
  mux.context = { cookies: async () => [{ name: 'prism_session_token', value: 'SECRET' }] };
  mux.page = { isClosed: () => false, url: () => 'https://prism.openai.com/auth/session',
    evaluate: async () => ({ status: 403, text: '<html>blocked</html>' }) };
  const driver = new BrowserSession({}, () => {}, 'account', 0, { multiplex: true, multiplexer: mux });
  const turn = { started: true, requestId: 'one', conversationId: 'c', turnState: 's', statusTemplate: { diff_format: 'dense' }, request,
    // What the editor page's accepted poll carried: browser-managed headers, the one the resident also sets,
    // and two page-script headers the resident fetch does not set.
    nativeStatusHeaderNames: ['accept', 'content-type', 'cookie', 'openai-sentinel-token', 'user-agent', 'x-prism-device-id'],
    ownBodies: new Set(), ownPolls: 0, ownPollErrors: 0, ownPollErrorTotal: 0, reject() {} };
  driver.turn = turn;
  mux.startPolling(driver, turn);
  await new Promise(resolve => setTimeout(resolve, 50));
  const event = audits.find(item => item.event === 'resident_poll_refused');
  assert.equal(event.status, 403);
  assert.deepEqual(event.editor_poll_header_names, ['accept', 'content-type', 'cookie', 'openai-sentinel-token', 'user-agent', 'x-prism-device-id']);
  assert.deepEqual(event.editor_only_header_names, ['openai-sentinel-token', 'x-prism-device-id']);
  assert.deepEqual(event.resident_set_header_names, ['content-type']);
  assert.equal(JSON.stringify(audits).includes('SECRET'), false);
});

test('without a captured editor poll the refusal still records the resident side', async () => {
  const audits = [];
  const mux = new AccountPageMultiplexer({ onAudit: (event, fields) => audits.push({ event, ...fields }) });
  mux.context = { cookies: async () => [] }; mux.page = { isClosed: () => false, url: () => '' };
  await mux.diagnoseRefusal('/api/llm/response_with_tools_status', { status: 403, text: '' }, {});
  assert.equal(audits[0].editor_poll_header_names, undefined);
  assert.equal(audits[0].editor_only_header_names, undefined);
  assert.deepEqual(audits[0].resident_set_header_names, ['content-type']);
});

test('a resident page that never gets the official fetch wrapper is rejected, never used for polling', async () => {
  let closed = 0; let evaluated = 0;
  const page = { goto: async () => {}, addInitScript: async () => {}, isClosed: () => false, on() {},
    waitForFunction: async () => { throw new Error('timeout'); }, evaluate: async () => { evaluated += 1; } };
  const browser = { newContext: async () => ({ addCookies: async () => {}, newPage: async () => page, close: async () => { closed += 1; } }) };
  const mux = new AccountPageMultiplexer();
  await assert.rejects(mux.register(browser, { access_token: 'token' }, 'account-a'), error => error.code === 'poll_carrier_unavailable' && error.status === 503);
  assert.equal(closed, 1, 'the unusable context must be closed');
  assert.equal(mux.isAlive(), false);
  assert.equal(mux.identity, null);
  assert.equal(evaluated, 0);
  await mux.close();
});

test('the resident page loads the real site, not a bare JSON document', async () => {
  const visited = []; let initScript = null;
  const page = { goto: async url => { visited.push(url); }, addInitScript: async fn => { initScript = String(fn); }, waitForFunction: async () => {},
    isClosed: () => false, on() {} };
  const browser = { newContext: async () => ({ addCookies: async () => {}, newPage: async () => page, close: async () => {} }) };
  const mux = new AccountPageMultiplexer({ origin: 'https://prism.openai.com' });
  await mux.register(browser, { access_token: 'token' }, 'account-a');
  assert.deepEqual(visited, ['https://prism.openai.com/']);
  assert.match(initScript, /__prismOriginalFetch/);
  await mux.close();
});
