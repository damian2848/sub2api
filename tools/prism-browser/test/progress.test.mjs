import test from 'node:test';
import assert from 'node:assert/strict';
import { BrowserSession, MAX_FORWARDED_CHARS, MAX_FORWARDED_PARTS, toolProgressKind,
  toolProgressLines } from '../src/browser.mjs';

const projectId = '01234567-89ab-4cde-8123-0123456789ab';
const statusUrl = 'https://prism.openai.com/api/llm/response_with_tools_status';

function startRoute() {
  const request = { url: () => 'https://prism.openai.com/api/llm/response_with_tools_start', method: () => 'POST',
    headers: () => ({}), postDataJSON: () => ({ conversationId: 'current-conversation', input: [],
      metadata: { projectId, model: 'gpt-5.6-sol', reasoning_effort: 'low' } }) };
  return { request: () => request, async abort() { assert.fail('aborted'); }, async continue() {} };
}

const reply = (request, data) => ({ url: request.url, request: () => request, ok: () => true, status: () => 200,
  json: async () => data });
const pendingBody = progress => ({ request_id: 'current-request', turn_state: 'current-state', status: 'pending',
  ...(progress ? { codex_live_progress: progress } : {}) });
const summary = (line, text) => ({ line_index: line, text });
const call = (line, name, preview = '') => ({ line_index: line, call_id: `call-${line}`, name, call_type: 'function_call',
  arguments_preview: preview, source: 'sandbox' });

async function startedTurn(onReasoning) {
  const driver = new BrowserSession({}, () => {}, '32', 0);
  driver.projectId = projectId;
  const events = [];
  driver.audit = (event, details) => events.push({ event, ...details });
  driver.turn = { started: false, submitAllowed: true, request: { model: 'gpt-5.6-sol', effort: 'low', input: [],
    ...(onReasoning ? { onReasoning } : {}) }, resolve() {}, reject(error) { assert.fail(error.code); } };
  const start = startRoute();
  await driver.route(start);
  await driver.observe(reply(start.request(), pendingBody()));
  const poll = (progress, { own = false } = {}) => {
    const request = { url: () => statusUrl, method: () => 'POST',
      postDataJSON: () => ({ request_id: 'current-request', turn_state: 'current-state' }),
      postData: () => (own ? 'own-body' : 'page-body') };
    if (own) driver.turn.ownBodies = new Set(['own-body']);
    return driver.observe(reply(request, pendingBody(progress)));
  };
  return { driver, poll, events };
}

test('each reasoning summary is forwarded once, in order, even when both pollers report it', async () => {
  const notes = [];
  const { poll } = await startedTurn(text => notes.push(text));
  await poll({ reasoningSummaries: [summary(1, '**Plan**\nFirst I look at the layout.')] });
  await poll({ reasoningSummaries: [summary(1, '**Plan**\nFirst I look at the layout.')] }, { own: true });
  await poll({ reasoningSummaries: [summary(1, '**Plan**\nFirst I look at the layout.'),
    summary(2, '  **Build**\n\nThen I write it.  '), summary(3, '   '), summary(4, 7), summary(5, '**Plan** First I look at the   layout.')] });
  assert.deepEqual(notes, ['**Plan**\nFirst I look at the layout.', '**Build**\n\nThen I write it.'],
    'blank, non-text and whitespace-only variants of a seen note are skipped');
});

test('tool progress is forwarded as one bold line per kind and per grown count, never a repeat', async () => {
  const notes = [];
  const { poll } = await startedTurn(text => notes.push(text));
  await poll({ toolCalls: [call(1, 'exec', 'ls -la')] });
  await poll({ toolCalls: [call(1, 'exec', 'ls -la')] });
  await poll({ toolCalls: [call(1, 'exec', 'ls -la'), call(2, 'exec', 'grep -rn foo .')] });
  await poll({ toolCalls: [call(1, 'exec', 'ls -la'), call(2, 'exec', 'grep -rn foo .'), call(3, 'exec', 'ls src'),
    call(4, 'exec', 'cat a.txt'), call(5, 'apply_patch', '*** Begin Patch')] });
  assert.deepEqual(notes, ['**Explored 1 location**', '**Searched**', '**Explored 2 locations**', '**Read 1 file**',
    '**Editing files**']);
});

test('tool kinds and lines match what the Prism page shows', () => {
  const kind = (name, preview = '') => toolProgressKind({ name, call_type: 'function_call', arguments_preview: preview, source: '' });
  assert.deepEqual([kind('apply_patch'), kind('shell', 'rg foo'), kind('shell', 'ls -la'), kind('shell', 'cat x'),
    kind('shell', 'bash -lc make'), kind('web.open')], ['edit', 'search', 'explore', 'read', 'command', 'other']);
  assert.deepEqual(toolProgressLines({ explore: 2, search: 1, read: 1, edit: 1, command: 1 }),
    ['Explored 2 locations', 'Searched', 'Read 1 file', 'Editing files', 'Running a command']);
  assert.deepEqual(toolProgressLines({ other: 3 }), ['Using a tool']);
  assert.deepEqual(toolProgressLines({ command: 1, other: 3 }), ['Running a command']);
  assert.deepEqual(toolProgressLines({}), []);
});

test('nothing is forwarded without a callback, and a throwing callback or odd progress never breaks the turn', async () => {
  const quiet = await startedTurn();
  await quiet.poll({ reasoningSummaries: [summary(1, '**Plan**')], toolCalls: [call(1, 'exec', 'ls')] });
  assert.equal(quiet.driver.turn.notes, undefined);

  let attempts = 0;
  const loud = await startedTurn(() => { attempts += 1; throw new Error('client gone'); });
  await loud.poll({ reasoningSummaries: [summary(1, '**Plan**')], toolCalls: [call(1, 'exec', 'ls')] });
  assert.equal(attempts, 2, 'both notes were attempted');
  for (const progress of [null, 'text', 7, [], { reasoningSummaries: 'x', toolCalls: 4 }, { reasoningSummaries: [null, 3, {}],
    toolCalls: [null, 'x'] }]) await loud.poll(progress);
  assert.equal(attempts, 2);
});

test('notes are bounded in size and number, and the result audit counts them without their text', async () => {
  const notes = [];
  const { driver, poll, events } = await startedTurn(text => notes.push(text));
  await poll({ reasoningSummaries: [summary(0, 'x'.repeat(MAX_FORWARDED_CHARS + 500))] });
  assert.equal(notes[0].length, MAX_FORWARDED_CHARS);
  assert.ok(notes[0].endsWith('…'));
  const many = Array.from({ length: 200 }, (_, index) => summary(index + 1, `**Step ${index}** secret detail ${index}`));
  await poll({ reasoningSummaries: many.slice(0, 100) });
  await poll({ reasoningSummaries: many.slice(100) });
  assert.equal(notes.length, MAX_FORWARDED_PARTS);

  const request = { url: () => statusUrl, method: () => 'POST',
    postDataJSON: () => ({ request_id: 'current-request', turn_state: 'current-state' }), postData: () => 'page-body' };
  driver.turn.resolve = () => {};
  await driver.observe(reply(request, { request_id: 'current-request', turn_state: 'current-state', status: 'completed',
    response: { status: 'success', payload: { output: [{ type: 'message', content: [{ type: 'output_text', text: 'READY' }] }] } } }));
  const result = events.find(event => event.event === 'upstream_result');
  assert.equal(result.reasoning_forwarded, MAX_FORWARDED_PARTS);
  assert.ok(!/secret detail/.test(JSON.stringify(events)));
});
