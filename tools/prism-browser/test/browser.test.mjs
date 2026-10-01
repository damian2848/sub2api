import test from 'node:test';
import assert from 'node:assert/strict';
import { BrowserSession, FALLBACK_MODEL, catalogCollapsed, catalogFromConfig, modelFromLabel, probeModel } from '../src/browser.mjs';
import { PrismError, publicError } from '../src/errors.mjs';

function route(body, path = '/api/llm/response_with_tools_start') {
  const state = { aborted: false, body: null };
  const request = { url: () => `https://prism.openai.com${path}`, method: () => 'POST', postDataJSON: () => body };
  return { state, request: () => request, async abort() { state.aborted = true; },
  async continue(options) { state.body = options ? JSON.parse(options.postData) : body; } };
}

function response(request, data, status = 200) {
  return { url: request.url, request: () => request, ok: () => status >= 200 && status < 300,
    status: () => status, json: async () => data };
}

async function activeTurn() {
  const driver = new BrowserSession({}, () => {});
  driver.projectId = '01234567-89ab-4cde-8123-0123456789ab';
  const outcomes = [];
  const turn = { started: false, submitAllowed: true, request: { model: 'gpt-5.6-sol', effort: 'medium', input: [] },
    resolve(text) { outcomes.push({ text }); }, reject(error) { outcomes.push({ error: error.code }); } };
  driver.turn = turn;
  const start = route({ conversationId: 'current-conversation', metadata: {
    projectId: driver.projectId, model: turn.request.model, reasoning_effort: turn.request.effort } });
  await driver.route(start);
  return { driver, turn, start, outcomes };
}

const running = { request_id: 'current-request', turn_state: 'current-state', status: 'running' };
const completed = { ...running, status: 'completed', response: { status: 'success', payload: {
  output: [{ type: 'message', content: [{ type: 'output_text', text: 'READY' }] }] } } };

for (const arrival of ['during_press', 'after_press']) test(`pre-submit starts cannot claim a turn and legal starts ${arrival} remain accepted`, async () => {
  const driver = new BrowserSession({}, () => {}, '32', 1);
  driver.projectId = '01234567-89ab-4cde-8123-0123456789ab';
  const input = [{ role: 'user', content: [{ type: 'input_text', text: 'Current request' }] }];
  const request = { model: 'gpt-5.6-sol', effort: 'low', input };
  const events = [];
  const phases = [];
  driver.audit = (event, details) => events.push({ event, ...details });
  const body = conversationId => ({ conversationId, input: [{ role: 'user', content: 'Old UI input' }],
    metadata: { projectId: driver.projectId, model: request.model, reasoning_effort: request.effort } });
  const staleStart = async phase => {
    phases.push(phase);
    const turn = driver.turn;
    assert.equal(turn.submitAllowed, false);
    const stale = route(body('stale-conversation'));
    await driver.route(stale);
    assert.equal(stale.state.aborted, true);
    assert.equal(stale.state.body, null, 'blocked starts must not replace the UI input');
    assert.equal(turn.started, false);
    assert.equal(turn.startRequest, undefined);
    assert.equal(turn.conversationId, undefined);
    await driver.observe(response(stale.request(), completed));
    assert.equal(driver.turn, turn);
  };
  let accepted;
  const submit = async () => {
    assert.equal(driver.turn.submitAllowed, true);
    accepted = route(body('current-conversation'));
    await driver.route(accepted);
    assert.equal(accepted.state.aborted, false);
    assert.deepEqual(accepted.state.body.input, input);
    assert.equal(driver.turn.startRequest, accepted.request());
    await driver.observe(response(accepted.request(), completed));
  };
  driver.page = { isClosed: () => false, getByRole: () => ({ click: () => staleStart('new_chat') }) };
  driver.composer = async () => {
    await staleStart('composer');
    return { fill: () => staleStart('fill'), async press() {
      if (arrival === 'during_press') await submit();
      else setImmediate(() => submit().catch(error => driver.turn?.reject(error)));
    } };
  };
  driver.select = () => staleStart('selection');
  assert.equal(await driver.generate(request), 'READY');
  assert.deepEqual(phases, ['new_chat', 'composer', 'selection', 'fill']);
  assert.equal(events.filter(event => event.event === 'upstream_start_blocked').length, 4);
  assert.deepEqual(events.filter(event => event.event === 'upstream_start_blocked'), Array(4).fill({
    event: 'upstream_start_blocked', code: 'browser_start_before_submit' }));
  assert.equal(driver.turn, null);
});

test('browser audit includes the worker slot and defaults legacy sessions to worker zero', () => {
  const previous = process.env.PRISM_AUDIT_REQUESTS;
  const originalLog = console.log;
  const events = [];
  try {
    process.env.PRISM_AUDIT_REQUESTS = 'true';
    console.log = value => events.push(JSON.parse(value));
    new BrowserSession({}, () => {}, '32', 1).audit('upstream_result', { status: 'error' });
    new BrowserSession({}, () => {}, '32').audit('upstream_start_blocked', { code: 'browser_start_before_submit' });
  } finally {
    console.log = originalLog;
    if (previous === undefined) delete process.env.PRISM_AUDIT_REQUESTS;
    else process.env.PRISM_AUDIT_REQUESTS = previous;
  }
  assert.deepEqual(events, [
    { event: 'upstream_result', source: '32', worker: 1, status: 'error' },
    { event: 'upstream_start_blocked', source: '32', worker: 0, code: 'browser_start_before_submit' },
  ]);
});

test('old chat responses and HTTP errors cannot reject or overwrite the active turn', async () => {
  const { driver, turn, start, outcomes } = await activeTurn();
  await driver.observe(response(start.request(), running));
  const staleStart = route({ conversationId: 'previous-conversation' });
  const unacceptedStart = route({ conversationId: 'current-conversation' });
  const staleStatus = route({ request_id: 'previous-request', turn_state: 'previous-state',
    conversation_id: 'previous-conversation' }, '/api/llm/response_with_tools_status');
  const failed = { request_id: 'previous-request', turn_state: 'previous-state',
    conversation_id: 'previous-conversation', status: 'error', response: { status: 'error' } };
  for (const request of [staleStart.request(), unacceptedStart.request(), staleStatus.request()]) {
    await driver.observe(response(request, failed));
    await driver.observe(response(request, null, 403));
  }
  assert.deepEqual(outcomes, []);
  assert.equal(turn.requestId, running.request_id);
  assert.equal(turn.turnState, running.turn_state);
  assert.equal(turn.conversationId, 'current-conversation');
  assert.equal(turn.completed, undefined);
});

test('unbound status and mismatched request or response identifiers are ignored', async () => {
  const { driver, turn, start, outcomes } = await activeTurn();
  const status = route({ request_id: running.request_id, turn_state: running.turn_state },
    '/api/llm/response_with_tools_status');
  await driver.observe(response(status.request(), completed));
  assert.deepEqual(outcomes, []);
  await driver.observe(response(start.request(), running));
  await driver.observe(response(status.request(), { ...completed, request_id: 'other-request' }));
  await driver.observe(response(status.request(), { ...completed, conversation_id: 'other-conversation' }));
  const wrongConversation = route({ request_id: running.request_id, conversation_id: 'other-conversation' },
    '/api/llm/response_with_tools_status');
  await driver.observe(response(wrongConversation.request(), completed));
  await driver.observe(response(wrongConversation.request(), null, 502));
  assert.deepEqual(outcomes, []);
  assert.equal(turn.requestId, running.request_id);
  assert.equal(turn.turnState, running.turn_state);
});

test('an accepted start may complete immediately and current polls may carry opaque rotating state', async () => {
  const immediate = await activeTurn();
  await immediate.driver.observe(response(immediate.start.request(), completed));
  assert.deepEqual(immediate.outcomes, [{ text: 'READY' }]);
  const polled = await activeTurn();
  await polled.driver.observe(response(polled.start.request(), { ...running, turn_state: { generation: 2 } }));
  const status = route({ request_id: running.request_id, turn_state: { generation: 1 } },
    '/api/llm/response_with_tools_status');
  await polled.driver.observe(response(status.request(), { ...completed, turn_state: { generation: 3 } }));
  assert.deepEqual(polled.outcomes, [{ text: 'READY' }]);
  assert.deepEqual(polled.turn.turnState, { generation: 3 });
});

test('a response that finishes parsing after its turn is replaced cannot settle the replacement', async () => {
  const { driver, start, outcomes } = await activeTurn();
  let finishJSON;
  const pending = response(start.request(), completed);
  pending.json = () => new Promise(resolve => { finishJSON = resolve; });
  const observation = driver.observe(pending);
  const replacement = { started: true, request: { model: 'gpt-5.6-sol' }, conversationId: 'replacement',
    resolve() { assert.fail('replacement resolved'); }, reject() { assert.fail('replacement rejected'); } };
  driver.turn = replacement;
  finishJSON(completed);
  await observation;
  assert.deepEqual(outcomes, []);
  assert.equal(replacement.requestId, undefined);
});

test('cancelled turns ignore current pending progress and terminal responses', async () => {
  const { driver, turn, start, outcomes } = await activeTurn();
  const controller = new AbortController();
  turn.signal = controller.signal;
  controller.abort();
  await driver.observe(response(start.request(), { ...running,
    codex_live_progress: { reasoningSummaries: [{ text: 'not assistant output' }] } }));
  await driver.observe(response(start.request(), completed));
  assert.deepEqual(outcomes, []);
  assert.equal(turn.requestId, undefined);
});

function deferred() {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return { promise, resolve };
}

test('authentication cannot resurrect a late context or page after the session is closed', async () => {
  for (const blockedStage of ['context', 'page']) {
    const reached = deferred();
    const resume = deferred();
    let closes = 0;
    let navigations = 0;
    const page = { async route() {}, on() {}, isClosed: () => false,
      async goto() { navigations++; }, async evaluate() { assert.fail('authentication after close'); } };
    const context = { async addCookies() {}, async close() { closes++; }, async newPage() {
      if (blockedStage === 'page') { reached.resolve(); await resume.promise; }
      return page;
    } };
    const driver = new BrowserSession({ async newContext() {
      if (blockedStage === 'context') { reached.resolve(); await resume.promise; }
      return context;
    } }, () => {});
    const authentication = driver.authenticate({ access_token: 'fixture', expected_user_id: 'fixture' });
    await reached.promise;
    await driver.close();
    resume.resolve();
    await assert.rejects(authentication, error => error.code === 'browser_session_closed');
    assert.equal(driver.isAlive(), false, blockedStage);
    assert.equal(driver.context, null);
    assert.equal(driver.page, null);
    assert.equal(closes, 1);
    assert.equal(navigations, 0);
  }
});

test('abort while creating an authentication context returns promptly and cleans up its late arrival', async () => {
  const reached = deferred();
  const resume = deferred();
  let closes = 0;
  const driver = new BrowserSession({ async newContext() {
    reached.resolve(); await resume.promise;
    return { async close() { closes++; }, async addCookies() { assert.fail('cookies after cancellation'); } };
  } }, () => {});
  const controller = new AbortController();
  const authentication = driver.authenticate({ access_token: 'fixture', expected_user_id: 'fixture' }, controller.signal);
  await reached.promise;
  controller.abort();
  await assert.rejects(authentication, error => error.code === 'request_cancelled');
  resume.resolve();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(closes, 1);
  assert.equal(driver.isAlive(), false);
});

test('closed initialization cannot reserve or continue a project mutation when its delayed navigation finishes', async () => {
  const reached = deferred();
  const resume = deferred();
  const driver = new BrowserSession({}, () => {});
  driver.context = { async close() {} };
  driver.page = { isClosed: () => false, waitForResponse() { return Promise.resolve({ ok: () => true,
    async json() { return { uuid: 'unused' }; } }); }, async goto() { reached.resolve(); await resume.promise; } };
  let reserved = 0;
  driver.composer = async () => { assert.fail('composer after close'); };
  const initialization = driver.initialize(null, () => { reserved++; });
  await reached.promise;
  await driver.close();
  resume.resolve();
  await assert.rejects(initialization, error => error.code === 'browser_session_closed');
  const create = route({ project_uuid: '01234567-89ab-4cde-8123-0123456789ab' }, '/api/projects');
  await driver.route(create);
  assert.equal(create.state.aborted, true);
  assert.equal(reserved, 0);
  assert.equal(driver.creating, false);
  assert.equal(driver.bootstrapping, false);
  assert.equal(driver.reserveProject, null);
});

test('late catalog data cannot restore labels after initialization is closed', async () => {
  const reached = deferred();
  const resume = deferred();
  const driver = new BrowserSession({}, () => {});
  driver.context = { async close() {} };
  driver.page = { isClosed: () => false, async goto() { driver.syncSeen = true; driver.lastHeartbeat = 1; } };
  driver.composer = async () => ({});
  driver.catalog = async () => { reached.resolve(); await resume.promise; return new Map([['gpt-5.6-sol', '5.6 Sol']]); };
  const initialization = driver.initialize('01234567-89ab-4cde-8123-0123456789ab', () => {});
  await reached.promise;
  await driver.close();
  resume.resolve();
  await assert.rejects(initialization, error => error.code === 'browser_session_closed');
  assert.equal(driver.labels.size, 0);
  assert.equal(driver.isAlive(), false);
});

test('cancellation destroys the context and stale UI work cannot submit or clear a replacement turn', async () => {
  for (const blockedStage of ['new_chat', 'composer', 'selection', 'fill']) {
    const driver = new BrowserSession({}, () => {});
    const reached = deferred();
    const resume = deferred();
    const calls = [];
    const step = async stage => {
      calls.push(stage);
      if (stage === blockedStage) { reached.resolve(); await resume.promise; }
    };
    let closed = 0;
    driver.context = { async close() { closed++; } };
    driver.page = { isClosed: () => false, getByRole: () => ({ click: () => step('new_chat') }) };
    const composer = { fill: () => step('fill'), press: () => step('submit') };
    driver.composer = async () => { await step('composer'); return composer; };
    driver.select = () => step('selection');
    const controller = new AbortController();
    let textCallbacks = 0;
    const generation = driver.generate({ model: 'gpt-5.6-sol', effort: 'medium', input: [] },
      controller.signal, () => { textCallbacks++; });
    await reached.promise;
    controller.abort();
    await assert.rejects(generation, error => error.code === 'request_cancelled');
    assert.equal(closed, 1, blockedStage);
    assert.equal(driver.page, null);
    assert.equal(driver.context, null);
    const replacement = { started: false };
    driver.turn = replacement;
    driver.page = { isClosed: () => false };
    const count = calls.length;
    resume.resolve();
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(calls.length, count, blockedStage);
    assert.ok(!calls.includes('submit'), blockedStage);
    assert.equal(driver.turn, replacement);
    assert.equal(textCallbacks, 0);
  }
});

test('cancellation inside a new-chat await prevents the later fill and submit', async () => {
  const driver = new BrowserSession({}, () => {});
  driver.labels.set('gpt-5.6-sol', '5.6 Sol');
  const reached = deferred();
  const resume = deferred();
  let closed = false;
  driver.context = { async close() { closed = true; } };
  driver.page = { isClosed: () => false, getByRole(role) {
    if (role === 'button') return { async click() { reached.resolve(); await resume.promise; } };
    assert.fail('the model menu must not be used');
  } };
  driver.composer = async () => ({ async fill() { assert.fail('cancelled fill'); }, async press() { assert.fail('cancelled submit'); } });
  const controller = new AbortController();
  const generation = driver.generate({ model: 'gpt-5.6-sol', effort: 'medium', input: [] }, controller.signal);
  await reached.promise;
  controller.abort();
  await assert.rejects(generation, error => error.code === 'request_cancelled');
  resume.resolve();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(closed, true);
});

test('HTTP errors from the accepted current start and current status still fail the turn', async () => {
  const startFailure = await activeTurn();
  await startFailure.driver.observe(response(startFailure.start.request(), null, 403));
  assert.deepEqual(startFailure.outcomes, [{ error: 'session_expired' }]);
  const statusFailure = await activeTurn();
  await statusFailure.driver.observe(response(statusFailure.start.request(), running));
  const status = route({ request_id: running.request_id, turn_state: running.turn_state },
    '/api/llm/response_with_tools_status');
  await statusFailure.driver.observe(response(status.request(), null, 502));
  assert.deepEqual(statusFailure.outcomes, [{ error: 'prism_upstream_http_error' }]);
});

test('only the current native resubmission terminal error is internally retryable', async () => {
  const payload = { httpStatus: 403, reason: 'unknown',
    message: 'Error while processing conversation (403 Forbidden). Please submit prompt again.' };
  const failure = { ...running, status: 'completed', response: { status: 'error', payload } };
  const cases = [
    [failure, true],
    [{ ...failure, status: 'failed' }, false],
    [{ ...failure, response: { ...failure.response, status: 'failed' } }, false],
    [{ ...failure, response: { status: 'error', payload: { ...payload, httpStatus: 401 } } }, false],
    [{ ...failure, response: { status: 'error', payload: { ...payload, httpStatus: '403' } } }, false],
    [{ ...failure, response: { status: 'error', payload: { ...payload, reason: 'model_not_available' } } }, false],
    [{ ...failure, response: { status: 'error', payload: { ...payload, message: 'Forbidden: secret-session-token' } } }, false],
  ];
  for (const [data, retryable] of cases) {
    const { driver, turn, start } = await activeTurn();
    let error;
    turn.reject = value => { error = value; };
    const stale = route({ conversationId: 'previous-conversation' });
    await driver.observe(response(stale.request(), failure));
    assert.equal(error, undefined);
    await driver.observe(response(start.request(), data));
    assert.equal(error.code, 'prism_generation_failed');
    assert.equal(error.retryConversation, retryable);
    assert.deepEqual(publicError(error), { error: {
      message: 'Prism could not complete the generation; try again', type: 'prism_error', code: 'prism_generation_failed' } });
  }
});

test('public errors carry a readable fixed reason and never upstream text', () => {
  const upstream = new PrismError('prism_upstream_http_error', 502);
  assert.match(publicError(upstream).error.message, /overloaded/);
  assert.equal(publicError(upstream).error.code, 'prism_upstream_http_error');
  assert.equal(publicError(new PrismError('model_not_available', 400, 'model')).error.param, 'model');
  // A code without a dedicated reason is its own message; an unknown failure is a generic code.
  assert.equal(publicError(new PrismError('route_not_found', 404)).error.message, 'route_not_found');
  assert.equal(publicError(new Error('raw upstream <secret>')).error.message, 'browser_operation_failed');
});

test('terminal audit validates the HTTP status and never emits raw error messages or credentials', async () => {
  for (const status of [403, 401, '403', 0, 600, 403.5, null]) {
    const { driver, start } = await activeTurn();
    const events = [];
    driver.audit = (event, details) => events.push({ event, ...details });
    await driver.observe(response(start.request(), { ...running, status: 'completed', response: {
      status: 'error', payload: { httpStatus: status, reason: 'unknown',
        message: 'Forbidden: secret-session-token', access_token: 'secret-access-token',
        input: [{ content: 'secret-prompt' }] } } }));
    const result = events.find(event => event.event === 'upstream_result');
    assert.equal(result.payload_http_status, Number.isInteger(status) && status >= 100 && status <= 599 ? status : undefined);
    assert.equal(result.resubmission_requested, false);
    assert.ok(!/secret-|access_token|message|input/.test(JSON.stringify(result)));
  }
  const { driver, start } = await activeTurn();
  const events = [];
  driver.audit = (event, details) => events.push({ event, ...details });
  await driver.observe(response(start.request(), { ...running, status: 'completed', response: { status: 'error',
    payload: { httpStatus: 403, reason: 'unknown',
      message: 'Error while processing conversation (403 Forbidden). Please submit prompt again.' } } }));
  const result = events.find(event => event.event === 'upstream_result');
  assert.equal(result.payload_http_status, 403);
  assert.equal(result.resubmission_requested, true);
  assert.equal(result.message, undefined);
});

test('one UI start replaces the UI text, preserves official metadata and blocks an automatic replay', async () => {
  const driver = new BrowserSession({}, () => {});
  driver.projectId = '01234567-89ab-4cde-8123-0123456789ab';
  const input = [{ type: 'message', role: 'user', content: [{ type: 'input_text', text: 'Original question' }] }];
  driver.turn = { started: false, submitAllowed: true, request: { model: 'gpt-6.1-sol', effort: 'high', input },
    reject(error) { assert.fail(error.code); } };
  const body = { input: [{ role: 'user', content: 'UI placeholder' }], metadata: {
    projectId: driver.projectId, model: 'gpt-6.1-sol', reasoning_effort: 'high', sandbox_token: 'official-token',
    sandbox_url: 'official-url', userId: 'official-identity' }, conversationId: 'official-conversation' };
  const first = route(body);
  await driver.route(first);
  assert.deepEqual(first.state.body.input, input);
  assert.deepEqual(first.state.body.metadata, body.metadata);
  assert.equal(first.state.body.conversationId, body.conversationId);
  const replay = route(body);
  await driver.route(replay);
  assert.equal(replay.state.aborted, true);
});

test('the requested model and effort replace the UI defaults, and unrelated project mutations are rejected', async () => {
  const driver = new BrowserSession({}, () => {});
  driver.projectId = '01234567-89ab-4cde-8123-0123456789ab';
  driver.turn = { started: false, submitAllowed: true, request: { model: 'gpt-6.1-sol', effort: 'low', input: [] },
    reject(error) { assert.fail(error.code); } };
  // The UI still sits on its loading defaults (Sol, medium); the exact requested values go upstream.
  const ui = route({ metadata: { projectId: driver.projectId, model: 'gpt-5.6-sol', reasoning_effort: 'medium', sandbox_token: 'official-token' } });
  await driver.route(ui);
  assert.equal(ui.state.aborted, false);
  assert.equal(ui.state.body.metadata.model, 'gpt-6.1-sol');
  assert.equal(ui.state.body.metadata.reasoning_effort, 'low');
  assert.equal(ui.state.body.metadata.sandbox_token, 'official-token');
  const unrelated = route({}, '/api/projects/other-project/delete');
  await driver.route(unrelated);
  assert.equal(unrelated.state.aborted, true);
});

test('select() only checks the catalog and the effort, and never touches the menu', async () => {
  const driver = new BrowserSession({}, () => {});
  driver.labels = catalogFromConfig([{ id: 'gpt-6.1-sol', label: '6.1 Sol' }, { id: 'gpt-6-luna', label: '6 Luna' }]);
  driver.page = { getByRole() { assert.fail('the model menu must not be used'); } };
  await driver.select({ model: 'gpt-6-luna', effort: 'high' });
  await assert.rejects(driver.select({ model: 'gpt-6-astra', effort: 'low' }), error => error.code === 'model_not_available');
  for (const effort of ['low', 'medium', 'high', 'xhigh']) await driver.select({ model: 'gpt-6-luna', effort });
  await assert.rejects(driver.select({ model: 'gpt-6-luna', effort: 'ultra' }), error => error.code === 'unsupported_reasoning_effort');
});

test('the catalog is Prism\'s own model config, validated, and the probe prefers the known-good model', () => {
  const config = [{ id: 'gpt-6.1-sol', label: '6.1 Sol' }, { id: 'gpt-5.6-sol', label: '5.6 Sol' },
    { id: 'gpt-5.6-terra', label: '5.6 Terra' }, { id: 'gpt-6-luna', label: '6 Luna' },
    { id: '../../etc', label: 'x' }, { id: 'gpt-6-astra' }, { label: 'no id' }, null, 'gpt-6-sol'];
  const catalog = catalogFromConfig(config);
  assert.deepEqual([...catalog.keys()], ['gpt-6.1-sol', 'gpt-5.6-sol', 'gpt-5.6-terra', 'gpt-6-luna', 'gpt-6-astra']);
  assert.equal(catalog.get('gpt-6-astra'), 'gpt-6-astra');
  assert.equal(catalogFromConfig(null).size, 0);
  assert.equal(catalogFromConfig('x').size, 0);
  assert.equal(probeModel([...catalog.keys()]), 'gpt-5.6-sol');
  assert.equal(probeModel(['gpt-6.1-sol', 'gpt-6-luna']), 'gpt-6.1-sol');
});

test('a Ready Statsig config decides the catalog without opening the model menu', async () => {
  const configured = [{ id: 'gpt-6.1-sol', label: '6.1 Sol' }, { id: 'gpt-5.6-sol', label: '5.6 Sol' },
    { id: 'gpt-5.6-terra', label: '5.6 Terra' }, { id: 'gpt-6-luna', label: '6 Luna' }];
  const loading = catalogDriver([['gpt-5.6-sol']], ['Ready'], configured);
  const labels = await loading.driver.catalog(['gpt-5.6-sol']);
  assert.deepEqual([...labels.keys()], ['gpt-6.1-sol', 'gpt-5.6-sol', 'gpt-5.6-terra', 'gpt-6-luna']);
  assert.deepEqual(loading.calls, ['statsig', 'config']);
});

test('catalog advertises only actual model menu labels', () => {
  assert.equal(modelFromLabel('6.1 Sol'), 'gpt-6.1-sol');
  assert.equal(modelFromLabel('5.6 Terra'), 'gpt-5.6-terra');
  assert.equal(modelFromLabel('6 Luna'), 'gpt-6-luna');
  assert.equal(modelFromLabel('Model6 Astra'), null);
  assert.equal(modelFromLabel('EffortMedium'), null);
});

test('native project UUID is durably reserved before the create is sent', async () => {
  const driver = new BrowserSession({}, () => {});
  driver.creating = true;
  let persisted;
  driver.reserveProject = async id => { persisted = id; };
  const id = '01234567-89ab-4cde-8123-0123456789ab';
  const create = route({ project_uuid: id, title: 'Blank project' }, '/api/projects');
  create.continue = async () => { assert.equal(persisted, id); create.state.body = 'sent'; };
  await driver.route(create);
  assert.equal(create.state.body, 'sent');
  assert.equal(driver.projectId, id);
  const second = route({ project_uuid: '11234567-89ab-4cde-8123-0123456789ab' }, '/api/projects');
  await driver.route(second);
  assert.equal(second.state.aborted, true);
});

const full = ['gpt-6-astra', 'gpt-5.6-sol', 'gpt-5.6-terra', 'gpt-5.6-luna'];

test('catalog decision flags only the one-model fallback list against a larger persisted catalog', () => {
  assert.equal(FALLBACK_MODEL, 'gpt-5.6-sol');
  assert.equal(catalogCollapsed(['gpt-5.6-sol'], full), true);
  assert.equal(catalogCollapsed(['gpt-5.6-sol'], ['gpt-5.6-sol', 'gpt-5.6-terra']), true);
  assert.equal(catalogCollapsed(['gpt-5.6-sol'], ['gpt-5.6-sol']), false);
  assert.equal(catalogCollapsed(['gpt-5.6-sol'], []), false);
  assert.equal(catalogCollapsed(['gpt-5.6-sol']), false);
  assert.equal(catalogCollapsed(['gpt-5.6-terra'], full), false);
  assert.equal(catalogCollapsed(['gpt-5.6-sol', 'gpt-5.6-terra'], full), false);
  assert.equal(catalogCollapsed([], full), false);
});

function catalogDriver(reads, statuses = ['Ready'], config = null) {
  const calls = [];
  const driver = new BrowserSession({}, () => {});
  driver.page = { async evaluate() { calls.push('statsig'); return statuses.length > 1 ? statuses.shift() : statuses[0]; },
    async waitForTimeout(ms) { calls.push(`wait ${ms}`); } };
  driver.readConfigModels = async () => { calls.push('config'); return config; };
  driver.readModelMenu = async () => { calls.push('read'); return new Map(reads.shift().map(id => [id, `label ${id}`])); };
  return { driver, calls };
}

test('a collapsed catalog is re-read once after three seconds and the second result is accepted', async () => {
  const recovered = catalogDriver([['gpt-5.6-sol'], full]);
  assert.deepEqual([...(await recovered.driver.catalog(full)).keys()], full);
  assert.deepEqual(recovered.calls, ['statsig', 'config', 'read', 'wait 3000', 'read']);
  // Still the fallback list on the second read: accepted, never a third read.
  const still = catalogDriver([['gpt-5.6-sol'], ['gpt-5.6-sol'], full]);
  assert.deepEqual([...(await still.driver.catalog(full)).keys()], ['gpt-5.6-sol']);
  assert.equal(still.calls.filter(call => call === 'read').length, 2);
  // A genuinely small catalog (nothing larger persisted) or a full read needs no retry.
  const first = catalogDriver([['gpt-5.6-sol']]);
  assert.deepEqual([...(await first.driver.catalog([])).keys()], ['gpt-5.6-sol']);
  assert.deepEqual(first.calls, ['statsig', 'config', 'read']);
  const healthy = catalogDriver([full]);
  assert.deepEqual([...(await healthy.driver.catalog(full)).keys()], full);
  assert.deepEqual(healthy.calls, ['statsig', 'config', 'read']);
});

test('the menu is not read until Statsig reports Ready, and a missing Statsig only delays by the timeout', async () => {
  const loading = catalogDriver([full], ['Loading', null, 'Loading', 'Ready']);
  await loading.driver.catalog(full);
  assert.deepEqual(loading.calls, ['statsig', 'wait 250', 'statsig', 'wait 250', 'statsig', 'wait 250', 'statsig', 'config', 'read']);
  const absent = catalogDriver([full], [null]);
  const started = Date.now();
  assert.equal(await absent.driver.waitForStatsig(undefined, 30, 5), false);
  assert.ok(Date.now() - started >= 30);
  const failing = new BrowserSession({}, () => {});
  failing.page = { evaluate: () => Promise.reject(new Error('navigation')), async waitForTimeout() {} };
  assert.equal(await failing.waitForStatsig(undefined, 5, 1), false);
  const ready = catalogDriver([full]);
  assert.equal(await ready.driver.waitForStatsig(), true);
  const controller = new AbortController();
  controller.abort();
  await assert.rejects(ready.driver.waitForStatsig(controller.signal), error => error.code === 'request_cancelled');
});
