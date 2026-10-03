import test from 'node:test';
import assert from 'node:assert/strict';
import { runInNewContext } from 'node:vm';
import { BrowserSession, CHAT_TAB_SELECTOR, PREPARED_CHAT_MAX_AGE_MS, closeOldChatTabsInPage,
  prewarmEnabled } from '../src/browser.mjs';
import { publicError } from '../src/errors.mjs';

const projectId = '01234567-89ab-4cde-8123-0123456789ab';
const success = text => ({ request_id: 'current-request', turn_state: 'current-state', status: 'completed',
  response: { status: 'success', payload: { output: [{ type: 'message', content: [{ type: 'output_text', text }] }] } } });
const failure = payload => ({ request_id: 'current-request', turn_state: 'current-state', status: 'completed',
  response: { status: 'error', payload } });

function startRoute(conversationId) {
  const request = { url: () => 'https://prism.openai.com/api/llm/response_with_tools_start', method: () => 'POST',
    headers: () => ({}), postDataJSON: () => ({ conversationId, input: [{ role: 'user', content: 'UI text' }],
      metadata: { projectId, model: 'gpt-5.6-sol', reasoning_effort: 'low' } }) };
  return { request: () => request, async abort() { assert.fail('the start must not be aborted'); }, async continue() {} };
}

const response = (request, data) => ({ url: request.url, request: () => request, ok: () => true, status: () => 200,
  json: async () => data });

// A page whose composer submits a native start that Prism answers with reply().
function fakePage(driver, { reply = () => success('READY'), composerState = () => ({ disabled: false, value: '' }) } = {}) {
  const calls = [];
  let conversations = 0;
  const composer = {
    async fill() { calls.push('fill'); },
    async evaluate(work) {
      calls.push('check_composer');
      return runInNewContext(`(${work.toString()})`, {})(composerState());
    },
    async press() {
      calls.push('submit');
      const start = startRoute(`conversation-${++conversations}`);
      await driver.route(start);
      await driver.observe(response(start.request(), reply()));
    },
  };
  return {
    calls,
    isClosed: () => false,
    getByRole(role, options) {
      assert.deepEqual([role, options.name], ['button', 'New chat tab']);
      return { async click() { calls.push('new_chat'); } };
    },
    locator(selector) {
      assert.equal(selector, 'textarea:visible');
      return { last: () => composer, waitFor: async () => {} };
    },
    async waitForFunction() {},
    async evaluate(work, selector) {
      assert.equal(work, closeOldChatTabsInPage);
      assert.equal(selector, CHAT_TAB_SELECTOR);
      calls.push('close_tabs');
      return { before: 4, after: 1 };
    },
  };
}

function session(options) {
  const driver = new BrowserSession({}, () => {}, '32', 0);
  driver.projectId = projectId;
  driver.labels.set('gpt-5.6-sol', '5.6 Sol');
  driver.context = { async close() {} };
  driver.prewarm = true;
  const events = [];
  driver.audit = (event, details) => events.push({ event, ...details });
  driver.page = fakePage(driver, options);
  // composer() is Playwright waiting; the fake page has nothing to wait for.
  driver.composer = async () => driver.page.locator('textarea:visible').last();
  return { driver, events };
}

const request = () => ({ model: 'gpt-5.6-sol', effort: 'low',
  input: [{ type: 'message', role: 'user', content: [{ type: 'input_text', text: 'Hello there' }] }] });

test('a clean turn prepares the next chat while idle and the next request submits straight into it', async () => {
  const { driver, events } = session();
  assert.equal(await driver.generate(request()), 'READY');
  assert.ok(driver.preparing, 'a successful turn starts preparing the next chat');
  const prepared = await driver.preparing;
  assert.equal(prepared.page, driver.page);
  assert.deepEqual(driver.page.calls, ['new_chat', 'fill', 'submit', 'new_chat', 'close_tabs']);
  assert.deepEqual(events.filter(event => event.event === 'chat_prepared').map(event =>
    [event.chat_tabs_before, event.chat_tabs_after, typeof event.prepare_ms]), [[4, 1, 'number']]);

  driver.page.calls.length = 0;
  assert.equal(await driver.generate(request()), 'READY');
  assert.deepEqual(driver.page.calls.slice(0, 3), ['check_composer', 'fill', 'submit'],
    'the prepared chat is used without opening another one');
  const starts = events.filter(event => event.event === 'upstream_start');
  assert.deepEqual(starts.map(event => event.prewarmed), [false, true]);
  for (const start of starts) {
    assert.equal(start.input_chars, 'Hello there'.length);
    assert.equal(typeof start.prep_ms, 'number');
    assert.equal(typeof start.submit_ms, 'number');
  }
});

test('initialization exposes its first empty chat as the first prepared request', async () => {
  const { driver } = session();
  driver.page.waitForResponse = () => Promise.resolve({ ok: () => true,
    async json() { return { uuid: projectId }; } });
  driver.loadPage = async () => { driver.projectId = projectId; driver.syncSeen = true; driver.lastHeartbeat = 1; };
  driver.catalog = async () => new Map([['gpt-5.6-sol', '5.6 Sol']]);
  await driver.initialize(null, async () => {});
  driver.page.calls.length = 0;
  assert.equal(await driver.generate(request()), 'READY');
  assert.deepEqual(driver.page.calls.slice(0, 3), ['check_composer', 'fill', 'submit']);
  assert.equal(driver.page.calls.includes('new_chat'), true, 'only the idle follow-up preparation opens a new chat');
});

test('a prepared chat from an earlier page load, too old, or no longer empty is not used', async () => {
  const stale = [
    driver => { driver.pageGeneration += 1; },
    driver => { driver.preparing = Promise.resolve({ page: driver.page, generation: driver.pageGeneration,
      at: Date.now() - PREPARED_CHAT_MAX_AGE_MS - 1 }); },
    driver => { driver.preparing = Promise.resolve({ page: {}, generation: driver.pageGeneration, at: Date.now() }); },
  ];
  for (const [index, invalidate] of stale.entries()) {
    const { driver } = session();
    await driver.generate(request());
    await driver.preparing;
    invalidate(driver);
    driver.page.calls.length = 0;
    assert.equal(await driver.generate(request()), 'READY');
    assert.equal(driver.page.calls.filter(call => call === 'new_chat').length, 2, `case ${index}: a new chat, then the next prepare`);
    assert.equal(driver.page.calls[0], 'new_chat', `case ${index}`);
  }
  for (const state of [{ disabled: true, value: '' }, { disabled: false, value: 'left over' }]) {
    let current = { disabled: false, value: '' };
    const { driver } = session({ composerState: () => current });
    await driver.generate(request());
    await driver.preparing;
    current = state;
    driver.page.calls.length = 0;
    await driver.generate(request());
    assert.deepEqual(driver.page.calls.slice(0, 2), ['check_composer', 'new_chat'], JSON.stringify(state));
  }
});

test('a failed turn prepares nothing, and prewarming can be turned off', async () => {
  const { driver } = session({ reply: () => failure({ httpStatus: 500, reason: 'unknown' }) });
  await assert.rejects(driver.generate(request()), error => error.code === 'prism_generation_failed');
  assert.equal(driver.preparing, null);
  assert.deepEqual(driver.page.calls, ['new_chat', 'fill', 'submit']);

  const off = session();
  off.driver.prewarm = false;
  await off.driver.generate(request());
  assert.equal(off.driver.preparing, null);
  for (const value of ['false', 'FALSE', '0', 'off', ' off ']) assert.equal(prewarmEnabled(value), false, value);
  for (const value of [undefined, '', 'true', '1', 'on']) assert.equal(prewarmEnabled(value), true, String(value));
});

test('a reload while a chat is being prepared discards it', async () => {
  const { driver, events } = session();
  await driver.generate(request());
  driver.pageGeneration += 1;
  assert.equal(await driver.preparing, null);
  assert.deepEqual(events.filter(event => event.event === 'chat_prepare_failed').map(event => event.code),
    ['browser_session_closed']);
  assert.equal(events.some(event => event.event === 'chat_prepared'), false);
});

test('Prism refusing the conversation size is a 400 context_length_exceeded, not a retried 5xx', async () => {
  for (const payload of [{ httpStatus: 413, reason: 'unknown' }, { httpStatus: 400, reason: 'conversation_too_large' }]) {
    const { driver, events } = session({ reply: () => failure({ ...payload, message: 'secret upstream text' }) });
    let failure_;
    await assert.rejects(driver.generate(request()), error => { failure_ = error; return true; });
    assert.equal(failure_.code, 'context_length_exceeded');
    assert.equal(failure_.status, 400);
    assert.notEqual(failure_.transient, true);
    assert.notEqual(failure_.retryConversation, true);
    assert.deepEqual(publicError(failure_), { error: { type: 'invalid_request_error', code: 'context_length_exceeded',
      message: 'The conversation is too long for Prism; compact it or start a new conversation' } });
    assert.ok(!JSON.stringify(events).includes('secret upstream text'));
    assert.equal(driver.preparing, null);
  }
});

test('the result audit counts the tools and reasoning summaries of Prism\'s own agent, never their content', async () => {
  const driver = new BrowserSession({}, () => {}, '32', 0);
  driver.projectId = projectId;
  const events = [];
  driver.audit = (event, details) => events.push({ event, ...details });
  const outcomes = [];
  driver.turn = { started: false, submitAllowed: true, request: { model: 'gpt-5.6-sol', effort: 'low', input: [] },
    resolve: text => outcomes.push(text), reject: error => outcomes.push(error.code) };
  const start = startRoute('current-conversation');
  await driver.route(start);
  await driver.observe(response(start.request(), { request_id: 'current-request', turn_state: 'current-state', status: 'pending' }));
  const poll = progress => {
    const request = { url: () => 'https://prism.openai.com/api/llm/response_with_tools_status', method: () => 'POST',
      postDataJSON: () => ({ request_id: 'current-request', turn_state: 'current-state' }), postData: () => '{}' };
    return driver.observe(response(request, { request_id: 'current-request', turn_state: 'current-state', status: 'pending',
      codex_live_progress: progress }));
  };
  const call = (line, name) => ({ line_index: line, call_id: `call-${line}`, name, call_type: 'function_call',
    arguments_preview: 'cat secret-file.txt' });
  await poll({ toolCalls: [call(1, 'exec_command')], reasoningSummaries: [{ line_index: 2, text: 'secret thought' }] });
  await poll({ toolCalls: [call(1, 'exec_command'), call(3, 'apply_patch'), call(4, 'bad name!')],
    reasoningSummaries: [{ line_index: 2, text: 'secret thought' }, { line_index: 5, text: 'another' }] });
  const statusRequest = { url: () => 'https://prism.openai.com/api/llm/response_with_tools_status', method: () => 'POST',
    postDataJSON: () => ({ request_id: 'current-request', turn_state: 'current-state' }), postData: () => '{}' };
  const done = success('READY');
  done.response.payload.output.unshift({ type: 'reasoning', summary: [] }, { type: 'Bad Type' });
  await driver.observe(response(statusRequest, done));
  assert.deepEqual(outcomes, ['READY']);
  const result = events.find(event => event.event === 'upstream_result');
  assert.equal(result.internal_tool_calls, 3);
  assert.deepEqual(result.internal_tool_names, ['exec_command', 'apply_patch']);
  assert.equal(result.reasoning_summaries, 2);
  assert.deepEqual(result.output_types, { reasoning: 1, message: 1 });
  assert.equal(typeof result.prism_ms, 'number');
  assert.ok(!/secret/.test(JSON.stringify(events)));
});

test('closing old chat tabs middle-clicks every chat tab except the newest and the active one', async () => {
  const closed = [];
  const tabs = [];
  const tab = (id, className = 'border-transparent') => {
    const element = { className, isConnected: true, getAttribute: name => name === 'data-tab-id' ? id : null,
      dispatchEvent(event) {
        closed.push({ id, type: event.type, button: event.button, bubbles: event.bubbles });
        tabs.splice(tabs.indexOf(element), 1);
        element.isConnected = false;
        return true;
      } };
    return element;
  };
  const document = { querySelectorAll(selector) { assert.equal(selector, CHAT_TAB_SELECTOR); return [...tabs]; } };
  class MouseEvent { constructor(type, init) { Object.assign(this, { type, ...init }); } }
  const run = () => runInNewContext(`(${closeOldChatTabsInPage.toString()})`, { document, MouseEvent, setTimeout })(CHAT_TAB_SELECTOR);

  tabs.push(tab('chat:1000'), tab('chat:3000', 'border-[color:var(--tabs-active-border)] text-fg-primary'), tab('chat:2000'),
    tab('chat:5000'), tab('chat:4000'));
  const result = await run();
  assert.deepEqual(closed.map(item => item.id), ['chat:1000', 'chat:2000', 'chat:4000']);
  assert.ok(closed.every(item => item.type === 'mousedown' && item.button === 1 && item.bubbles === true));
  assert.deepEqual([result.before, result.after], [5, 2]);

  closed.length = 0;
  tabs.length = 0;
  tabs.push(tab('chat:7000'));
  const single = await run();
  assert.deepEqual([single.before, single.after, closed.length], [1, 1, 0]);
});
