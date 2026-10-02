import test from 'node:test';
import assert from 'node:assert/strict';
import { request as httpRequest } from 'node:http';
import { createPrismServer, streamReasoningEnabled } from '../src/server.mjs';
import { PrismError } from '../src/errors.mjs';

const managementKey = 'm'.repeat(40);
const userKey = 'u'.repeat(40);
const models = ['gpt-6.1-sol'];
const body = { model: models[0], input: 'Caller text' };
const execTool = { type: 'function', name: 'exec_command', parameters: { type: 'object', properties: { cmd: { type: 'string' } } } };
const toolReply = '{"tool_call":{"name":"exec_command","arguments":{"cmd":"ls"}}}';
function mockManager(overrides = {}) {
  return {
    status() { return { phase: 'ready', ready: true, models }; },
    authenticateKey(source, key) { if (source !== '32' || key !== userKey) throw new PrismError('invalid_api_key', 401); },
    async provision() { return { phase: 'authenticated', ready: false, models: [] }; },
    async bootstrap(_, __, options) { return { phase: 'ready', ready: true, models, retry: options.retry_probe === true }; },
    async revoke() { return { phase: 'authentication_required', ready: false, models }; },
    async generate() { return 'Only the actual browser result'; }, ...overrides,
  };
}
// Like the browser driver, the mock reports that Prism accepted the start before it generates;
// `{ accept: false }` leaves that to the overriding generate.
async function fixture(t, overrides, settings = {}, { accept = true } = {}) {
  const manager = mockManager(overrides);
  const generate = manager.generate;
  manager.generate = (source, request, ...rest) => {
    if (accept) request.onAccepted?.();
    return generate.call(manager, source, request, ...rest);
  };
  const server = createPrismServer({ manager, managementKey, ...settings });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(async () => { server.closeAllConnections(); await new Promise(resolve => server.close(resolve)); });
  const base = `http://127.0.0.1:${server.address().port}`;
  const post = (path, value, key = userKey) => fetch(base + path, { method: 'POST', headers: {
    Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' }, body: JSON.stringify(value) });
  return { base, post };
}

test('management and user credentials have separate authority', async t => {
  const { base, post } = await fixture(t);
  const unauthorized = await fetch(base + '/internal/accounts/32/status', { headers: { Authorization: `Bearer ${userKey}` } });
  assert.equal(unauthorized.status, 401);
  assert.equal((await unauthorized.json()).error.code, 'invalid_management_key');
  assert.equal((await post('/accounts/32/v1/responses', body, managementKey)).status, 401);
  assert.equal((await fetch(base + '/health')).status, 200);
  const listing = await fetch(base + '/accounts/32/v1/models', { headers: { Authorization: `Bearer ${userKey}` } });
  const modelList = (await listing.json()).data;
  assert.deepEqual(modelList.map(model => model.id), models);
  assert.deepEqual(modelList[0].input_modalities, ['text', 'image']);
  assert.deepEqual(modelList[0].output_modalities, ['text']);
  assert.equal((await post('/internal/accounts/32/bootstrap', { retry_probe: true }, managementKey)).status, 200);
});

test('HTTP body bounds apply and requests Prism cannot serve never reach the browser', async t => {
  let calls = 0;
  const { post } = await fixture(t, { async generate() { calls += 1; return 'unexpected'; } }, { bodyLimit: 4096 });
  assert.equal((await post('/accounts/32/v1/responses', { ...body, input: 'x'.repeat(8192) })).status, 413);
  const previous = await post('/accounts/32/v1/responses', { ...body, previous_response_id: 'resp_1' });
  assert.equal(previous.status, 400);
  assert.equal((await previous.json()).error.code, 'previous_response_not_supported');
  const audio = await post('/accounts/32/v1/chat/completions', { model: models[0], messages: [{ role: 'user',
    content: [{ type: 'input_audio', input_audio: { data: 'AAAA', format: 'wav' } }] }] });
  assert.equal(audio.status, 400);
  assert.equal((await audio.json()).error.code, 'image_input_not_supported');
  assert.equal((await post('/accounts/32/v1/responses', { ...body, model: 'unknown' })).status, 400);
  assert.equal(calls, 0);
});

test('Responses and Chat resolve attachment bytes before the browser starts', async t => {
  const seen = [];
  const { post } = await fixture(t, { async generate(_, request) { seen.push(request); return 'attachment read'; } });
  const text = Buffer.from('attachment content');
  for (const family of ['responses', 'chat/completions']) {
    const part = family === 'responses'
      ? { type: 'input_file', filename: 'notes.txt', file_data: text.toString('base64') }
      : { type: 'file', file: { filename: 'notes.txt', file_data: text.toString('base64') } };
    const value = { model: models[0], stream: true, [family === 'responses' ? 'input' : 'messages']:
      [{ role: 'user', content: [part] }] };
    const response = await post('/accounts/32/v1/' + family, value);
    assert.equal(response.status, 200);
    assert.match(response.headers.get('content-type'), /text\/event-stream/);
    assert.match(await response.text(), /attachment read/);
    const request = seen.at(-1);
    assert.equal(request.attachments.length, 1);
    assert.deepEqual(request.attachments[0].data, text);
    assert.match(request.attachments[0].filename, /^notes-[a-f0-9]{16}\.txt$/);
    assert.match(request.input[0].content[0].text, /\[Attachment 1\]/);
  }
});

test('attachment validation stays before SSE commitment and browser admission', async t => {
  let calls = 0;
  const { post } = await fixture(t, { async generate() { calls++; return 'unexpected'; } },
    { maxAttachmentBytes: 8, maxAttachments: 1 });
  const input = parts => ({ model: models[0], stream: true, input: [{ role: 'user', content: parts }] });
  const file = value => ({ type: 'input_file', filename: 'note.txt', file_data: Buffer.from(value).toString('base64') });
  for (const [value, code] of [
    [input([file('larger than limit')]), 'attachment_too_large'],
    [input([file('a'), file('b')]), 'too_many_attachments'],
    [input([{ type: 'input_image', image_url: 'http://127.0.0.1/private.png' }]), 'attachment_url_not_allowed'],
  ]) {
    const response = await post('/accounts/32/v1/responses', value);
    assert.equal(response.status, 400);
    assert.match(response.headers.get('content-type'), /application\/json/);
    assert.equal((await response.json()).error.code, code);
  }
  assert.equal(calls, 0);
});

test('large Codex-sized bodies are accepted by default and reach the browser as one user message', async t => {
  const seen = [];
  const { post } = await fixture(t, { async generate(_, request) { seen.push(request); return 'ok'; } });
  const instructions = 'Codex instructions. '.repeat(60000);
  assert.ok(instructions.length > 1024 * 1024);
  const response = await post('/accounts/32/v1/responses', { ...body, instructions, tools: [execTool], temperature: 0.1,
    tool_choice: 'auto', parallel_tool_calls: true, store: true, include: ['reasoning.encrypted_content'] });
  assert.equal(response.status, 200);
  assert.equal((await response.json()).output[0].content[0].text, 'ok');
  assert.equal(seen[0].input.length, 1);
  assert.equal(seen[0].input[0].role, 'user');
  assert.ok(!seen[0].input[0].content[0].text.includes('Codex instructions.'));
  // Without tools the instructions travel with the prompt, so only the final flattened text is bounded.
  const huge = await post('/accounts/32/v1/responses', { ...body, instructions });
  assert.equal(huge.status, 400);
  assert.equal((await huge.json()).error.code, 'input_too_large');
  assert.equal((await post('/accounts/32/v1/responses', { ...body, input: 'x'.repeat(300 * 1024) })).status, 400);
  assert.equal(seen.length, 1);
});

test('management bodies keep a small limit and configured text limits are honoured', async t => {
  const { base, post } = await fixture(t, undefined, { maxTextBytes: 64, maxTranscriptChars: 2000 });
  const session = await fetch(base + '/internal/accounts/32/session', { method: 'PUT', headers: {
    Authorization: `Bearer ${managementKey}`, 'Content-Type': 'application/json' },
  body: JSON.stringify({ access_token: 'a'.repeat(200 * 1024) }) });
  assert.equal(session.status, 413);
  assert.equal((await session.json()).error.code, 'request_body_too_large');
  const tooLarge = await post('/accounts/32/v1/responses', { ...body, input: 'x'.repeat(65) });
  assert.equal(tooLarge.status, 400);
  assert.equal((await tooLarge.json()).error.code, 'input_too_large');
  assert.equal((await post('/accounts/32/v1/responses', { ...body, input: 'x'.repeat(64) })).status, 200);
});

test('tool calls come back as function_call items and tool_calls over HTTP, streamed or not', async t => {
  const { post } = await fixture(t, { async generate() { return toolReply; } });
  const responses = await (await post('/accounts/32/v1/responses', { ...body, tools: [execTool] })).json();
  assert.equal(responses.output[0].type, 'function_call');
  assert.equal(responses.output[0].arguments, '{"cmd":"ls"}');
  const chat = await (await post('/accounts/32/v1/chat/completions', { model: models[0], messages: [{ role: 'user', content: 'ls' }],
    tools: [{ type: 'function', function: execTool }] })).json();
  assert.equal(chat.choices[0].finish_reason, 'tool_calls');
  assert.equal(chat.choices[0].message.tool_calls[0].function.name, 'exec_command');
  const sse = await (await post('/accounts/32/v1/responses', { ...body, tools: [execTool], stream: true })).text();
  assert.match(sse, /event: response\.function_call_arguments\.done/);
  assert.ok(sse.indexOf('response.output_item.added') < sse.indexOf('response.function_call_arguments.delta'));
  assert.ok(sse.indexOf('response.output_item.done') < sse.indexOf('response.completed'));
  const chatSse = await (await post('/accounts/32/v1/chat/completions', { model: models[0], messages: [{ role: 'user', content: 'ls' }],
    tools: [{ type: 'function', function: execTool }], stream: true })).text();
  assert.match(chatSse, /"tool_calls":\[\{"index":0,/);
  assert.match(chatSse, /"finish_reason":"tool_calls"/);
  assert.ok(chatSse.endsWith('data: [DONE]\n\n'));
});

test('nonstream output and SSE contain actual completed output and estimated usage', async t => {
  const { post } = await fixture(t, undefined, { keepaliveMs: 2 });
  const json = await post('/accounts/32/v1/responses', body);
  assert.equal(json.headers.get('x-prism-usage'), 'estimated');
  assert.equal((await json.json()).output_text, 'Only the actual browser result');
  const stream = await post('/accounts/32/v1/responses', { ...body, stream: true });
  const text = await stream.text();
  assert.match(text, /^: waiting for Prism\n\n/);
  assert.match(text, /event: response.completed/);
  assert.match(text, /Only the actual browser result/);
});

function deferred() {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return { promise, resolve };
}
const responseEvents = text => text.split('\n\n').filter(frame => frame.startsWith('event: '))
  .map(frame => JSON.parse(frame.split('\n')[1].slice(6)));

test('HTTP lifecycle reaches the client before generation completes and tool output appears only once afterward', async t => {
  const started = deferred();
  const completion = deferred();
  const { post } = await fixture(t, { generate() { started.resolve(); return completion.promise; } }, { keepaliveMs: 2 });
  const response = await post('/accounts/32/v1/responses', { ...body, tools: [execTool], stream: true });
  const reader = response.body.getReader();
  let wire = '';
  while (responseEvents(wire).length < 2) wire += new TextDecoder().decode((await reader.read()).value);
  await started.promise;
  const early = responseEvents(wire);
  assert.deepEqual(early.map(event => event.type), ['response.created', 'response.in_progress']);
  assert.ok(!wire.includes('tool_call') && !wire.includes('output_text.delta'));
  completion.resolve(toolReply);
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    wire += new TextDecoder().decode(value);
  }
  const events = responseEvents(wire);
  assert.deepEqual(events.map(event => event.sequence_number), events.map((_, index) => index));
  assert.equal(events.filter(event => event.type === 'response.created').length, 1);
  assert.equal(events.filter(event => event.type === 'response.function_call_arguments.done').length, 1);
  assert.equal(events[0].response.id, events.at(-1).response.id);
  assert.equal(events[0].response.created_at, events.at(-1).response.created_at);
  assert.equal(events.at(-1).response.output[0].arguments, '{"cmd":"ls"}');
});

test('stream timeout returns one safe top-level Responses error following the lifecycle', async t => {
  let calls = 0;
  const { post } = await fixture(t, { generate(_, __, signal) {
    calls++;
    return new Promise((_, reject) => signal.addEventListener('abort', () => reject(signal.reason), { once: true }));
  } }, { requestTimeout: 20, keepaliveMs: 2 });
  const response = await post('/accounts/32/v1/responses', { ...body, stream: true });
  const events = responseEvents(await response.text());
  // The short keepalive interval adds in_progress heartbeats between the lifecycle and the error.
  const types = events.map(event => event.type);
  assert.deepEqual(types.slice(0, 2), ['response.created', 'response.in_progress']);
  assert.ok(types.slice(2, -1).every(type => type === 'response.in_progress'));
  assert.equal(types.at(-1), 'error');
  assert.deepEqual(events.at(-1), { type: 'error', sequence_number: events.length - 1,
    code: 'request_timeout', message: 'Prism did not answer in time', param: null });
  assert.deepEqual(events.map(event => event.sequence_number), events.map((_, index) => index));
  assert.equal(calls, 1);
});

test('streaming unexpected errors never expose the browser message or nested error envelopes', async t => {
  const { post } = await fixture(t, { async generate() { throw new Error('secret-access-token'); } });
  const events = responseEvents(await (await post('/accounts/32/v1/responses', { ...body, stream: true })).text());
  assert.deepEqual(events.at(-1), { type: 'error', sequence_number: 2,
    code: 'browser_operation_failed', message: 'browser_operation_failed', param: null });
  assert.ok(!JSON.stringify(events).includes('secret-access-token'));
});

test('disconnect after early lifecycle cancels the sole streaming generation', async t => {
  const started = deferred();
  const cancelled = deferred();
  let calls = 0;
  const { post } = await fixture(t, { generate(_, __, signal) {
    calls++;
    started.resolve();
    return new Promise((_, reject) => signal.addEventListener('abort', () => {
      cancelled.resolve(); reject(signal.reason);
    }, { once: true }));
  } });
  const response = await post('/accounts/32/v1/responses', { ...body, stream: true });
  await started.promise;
  await response.body.cancel();
  await cancelled.promise;
  assert.equal(calls, 1);
});

test('request timeout aborts a single running generation without retry', async t => {
  let calls = 0;
  let aborts = 0;
  const { post } = await fixture(t, { generate(_, __, signal) {
    calls += 1;
    return new Promise((_, reject) => signal.addEventListener('abort', () => {
      aborts += 1; reject(signal.reason);
    }, { once: true }));
  } }, { requestTimeout: 20, keepaliveMs: 2 });
  const response = await post('/accounts/32/v1/responses', body);
  assert.equal(response.status, 504);
  assert.equal((await response.json()).error.code, 'request_timeout');
  assert.equal(calls, 1);
  assert.equal(aborts, 1);
});

test('disconnect cancels the browser operation and does not replay it', async t => {
  let started;
  let cancelled;
  const didStart = new Promise(resolve => { started = resolve; });
  const didCancel = new Promise(resolve => { cancelled = resolve; });
  let calls = 0;
  const { base } = await fixture(t, { generate(_, __, signal) {
    calls += 1; started();
    return new Promise((_, reject) => signal.addEventListener('abort', () => {
      cancelled(); reject(signal.reason);
    }, { once: true }));
  } });
  const request = httpRequest(base + '/accounts/32/v1/responses', { method: 'POST', headers: {
    Authorization: `Bearer ${userKey}`, 'Content-Type': 'application/json' } });
  request.on('error', () => {});
  request.end(JSON.stringify(body));
  await didStart;
  request.destroy();
  await didCancel;
  assert.equal(calls, 1);
});

test('unexpected errors do not expose browser messages or session material', async t => {
  const secret = 'do-not-expose-access-token';
  const { post } = await fixture(t, { async generate() { throw new Error(`Browser failed with ${secret}`); } });
  const response = await post('/accounts/32/v1/responses', body);
  assert.equal(response.status, 502);
  const wire = await response.text();
  assert.ok(!wire.includes(secret));
  assert.match(wire, /browser_operation_failed/);
});

test('a follow-up turn reports the shared prompt prefix as estimated cached tokens, failures are not cached', async t => {
  let fail = false;
  const { post } = await fixture(t, { async generate() { if (fail) throw new PrismError('prism_generation_failed', 502); return 'done'; } });
  const history = 'Earlier step and its tool output. '.repeat(400);
  const first = await (await post('/accounts/32/v1/responses', { ...body, input: history })).json();
  assert.equal(first.usage.input_tokens_details.cached_tokens, 0);
  const second = await (await post('/accounts/32/v1/responses', { ...body, input: `${history} New tool result.` })).json();
  const cached = second.usage.input_tokens_details.cached_tokens;
  assert.ok(cached >= 1024 && cached % 128 === 0 && cached <= second.usage.input_tokens, String(cached));
  assert.equal(second.usage.estimation, 'character_based_estimate');
  // A failed request is not recorded: the next different prompt finds nothing new to share with it.
  fail = true;
  assert.equal((await post('/accounts/32/v1/responses', { ...body, input: `Other ${history}` })).status, 502);
  fail = false;
  const after = await (await post('/accounts/32/v1/responses', { ...body, input: `Other ${history}` })).json();
  assert.equal(after.usage.input_tokens_details.cached_tokens, 0);
  // Turned off, nothing is reported as cached.
  const off = await fixture(t, {}, { promptCache: { observe: () => 0 } });
  const plain = await (await off.post('/accounts/32/v1/chat/completions', { model: models[0], stream: false,
    messages: [{ role: 'user', content: history }] })).json();
  assert.equal(plain.usage.prompt_tokens_details.cached_tokens, 0);
});

test('a start refused before Prism accepted it answers a stream with a plain 429 the gateway can fail over', async t => {
  const refused = Object.assign(new PrismError('prism_start_limited', 429), { retryAfterSeconds: 42 });
  const { post } = await fixture(t, { async generate() { throw refused; } }, {}, { accept: false });
  const response = await post('/accounts/32/v1/responses', { ...body, stream: true });
  assert.equal(response.status, 429);
  assert.equal(response.headers.get('retry-after'), '42');
  assert.match(response.headers.get('content-type'), /application\/json/);
  assert.deepEqual(await response.json(), { error: { type: 'rate_limit_exceeded', code: 'prism_start_limited',
    resets_in_seconds: 42, message: "Prism refused to start another generation on this account for now; try again shortly" } });
  // Accepted first, the same failure arrives inside the opened stream.
  const opened = await fixture(t, { async generate(_, request) { request.onAccepted(); throw refused; } }, {}, { accept: false });
  const stream = await opened.post('/accounts/32/v1/responses', { ...body, stream: true });
  assert.equal(stream.status, 200);
  assert.equal(responseEvents(await stream.text()).at(-1).code, 'prism_start_limited');
});

test('a stream opens only when Prism accepts the start, not when the request arrives', async t => {
  const accepted = deferred();
  const finish = deferred();
  let request;
  const { post } = await fixture(t, { async generate(_, value) { request = value; accepted.resolve(); return finish.promise; } }, {},
    { accept: false });
  const pending = post('/accounts/32/v1/responses', { ...body, stream: true });
  await accepted.promise;
  let headersArrived = false;
  pending.then(() => { headersArrived = true; });
  await new Promise(resolve => setTimeout(resolve, 50));
  assert.equal(headersArrived, false, 'nothing is sent before the start is accepted');
  request.onAccepted();
  const response = await pending;
  assert.equal(response.status, 200);
  finish.resolve('done');
  assert.match(await response.text(), /event: response.completed/);
});

test('failover headers default to available and only the exact none value permits waiting', async t => {
  const seen = [];
  const { base } = await fixture(t, { async generate(_, request) { seen.push(request.failover); return 'done'; } });
  for (const value of [undefined, 'none', 'available', 'unknown', 'NONE']) {
    const response = await fetch(base + '/accounts/32/v1/responses', { method: 'POST', headers: {
      Authorization: `Bearer ${userKey}`, 'Content-Type': 'application/json',
      ...(value === undefined ? {} : { 'X-Prism-Failover': value }) }, body: JSON.stringify(body) });
    assert.equal(response.status, 200);
    await response.text();
  }
  assert.deepEqual(seen, ['available', 'none', 'available', 'available', 'available']);
});

test('a no-failover stream sends no headers or heartbeat while queued or retrying', async t => {
  const entered = deferred();
  const release = deferred();
  const { base } = await fixture(t, { async generate(_, request) {
    assert.equal(request.failover, 'none');
    entered.resolve();
    await release.promise;
    request.onAccepted();
    return 'retried successfully';
  } }, { keepaliveMs: 5 }, { accept: false });
  const pending = fetch(base + '/accounts/32/v1/responses', { method: 'POST', headers: {
    Authorization: `Bearer ${userKey}`, 'Content-Type': 'application/json', 'X-Prism-Failover': 'none' },
    body: JSON.stringify({ ...body, stream: true }) });
  await entered.promise;
  let opened = false;
  pending.then(() => { opened = true; });
  await new Promise(resolve => setTimeout(resolve, 30));
  assert.equal(opened, false);
  release.resolve();
  const response = await pending;
  assert.equal(response.status, 200);
  assert.match(await response.text(), /retried successfully/);
});

const reasoningStreamEvents = text => responseEvents(text).map(event => event.type);

test('Prism progress reaches a streaming Responses client as a reasoning item before the answer', async t => {
  const first = deferred();
  const release = deferred();
  t.after(() => release.resolve());
  const { post } = await fixture(t, { async generate(_, request) {
    request.onReasoning('**Planning the page**\nI will start with the layout.');
    first.resolve();
    await release.promise;
    request.onReasoning('**Planning the page**\nI will start with the layout.');
    request.onReasoning('**Running a command**');
    return 'The final answer';
  } });
  const response = await post('/accounts/32/v1/responses', { ...body, stream: true });
  assert.equal(response.status, 200);
  const reader = response.body.getReader();
  let wire = '';
  await first.promise;
  while (!wire.includes('reasoning_summary_part.done')) wire += new TextDecoder().decode((await reader.read()).value);
  assert.ok(!wire.includes('response.completed'), 'the reasoning arrives while Prism is still generating');
  assert.ok(!wire.includes('event: response.output_text'), 'the answer is not part of it');
  release.resolve();
  for (;;) {
    const chunk = await reader.read();
    if (chunk.done) break;
    wire += new TextDecoder().decode(chunk.value);
  }
  const events = responseEvents(wire);
  assert.deepEqual(events.map(event => event.type), [
    'response.created', 'response.in_progress',
    'response.output_item.added',
    'response.reasoning_summary_part.added', 'response.reasoning_summary_text.delta', 'response.reasoning_summary_text.done',
    'response.reasoning_summary_part.done',
    'response.reasoning_summary_part.added', 'response.reasoning_summary_text.delta', 'response.reasoning_summary_text.done',
    'response.reasoning_summary_part.done',
    'response.output_item.done',
    'response.output_item.added', 'response.content_part.added', 'response.output_text.delta', 'response.output_text.done',
    'response.content_part.done', 'response.output_item.done', 'response.completed']);
  assert.deepEqual(events.map(event => event.sequence_number), events.map((_, index) => index), 'one gapless sequence');
  const reasoningItem = events[2].item;
  assert.deepEqual([reasoningItem.type, reasoningItem.status, reasoningItem.summary, reasoningItem.id.startsWith('rs_prism_')],
    ['reasoning', 'in_progress', [], true]);
  const summaryEvents = events.filter(event => event.type.startsWith('response.reasoning_summary'));
  assert.ok(summaryEvents.every(event => event.item_id === reasoningItem.id && event.output_index === 0));
  assert.deepEqual([...new Set(summaryEvents.map(event => event.summary_index))], [0, 1], 'a repeated note is sent once');
  assert.equal(events[4].delta, '**Planning the page**\nI will start with the layout.');
  assert.equal(events[5].text, events[4].delta);
  const done = events.find(event => event.type === 'response.output_item.done' && event.item.type === 'reasoning');
  assert.equal(done.item.status, 'completed');
  assert.deepEqual(done.item.summary.map(part => part.text), ['**Planning the page**\nI will start with the layout.', '**Running a command**']);
  const message = events.find(event => event.type === 'response.output_item.added' && event.item.type === 'message');
  assert.equal(message.output_index, 1, 'the answer follows the reasoning item');
  const answerEvents = events.slice(12, 18);
  assert.ok(answerEvents.every(event => event.output_index === 1), 'every answer event is at output 1');
  const completed = events.at(-1).response;
  assert.deepEqual(completed.output.map(item => item.type), ['reasoning', 'message']);
  assert.equal(completed.output_text, 'The final answer');
  assert.equal(completed.output[0].summary.length, 2);
});

test('without Prism progress the stream is exactly as before, and tool calls keep their order after the reasoning', async t => {
  const plain = await fixture(t, { async generate() { return 'Just an answer'; } });
  const plainEvents = reasoningStreamEvents(await (await plain.post('/accounts/32/v1/responses', { ...body, stream: true })).text());
  assert.ok(!plainEvents.some(type => type.includes('reasoning')));
  assert.equal(plainEvents.filter(type => type === 'response.output_item.added').length, 1);

  const tools = await fixture(t, { async generate(_, request) { request.onReasoning('**Checking files**'); return toolReply; } });
  const events = responseEvents(await (await tools.post('/accounts/32/v1/responses',
    { ...body, tools: [execTool], stream: true })).text());
  const added = events.filter(event => event.type === 'response.output_item.added').map(event => [event.output_index, event.item.type]);
  assert.deepEqual(added, [[0, 'reasoning'], [1, 'function_call']]);
  assert.deepEqual(events.at(-1).response.output.map(item => item.type), ['reasoning', 'function_call']);
});

test('reasoning is not offered to non-streaming or Chat requests, and PRISM_STREAM_REASONING=false turns it off', async t => {
  const offered = [];
  const note = async (_, request) => { offered.push(typeof request.onReasoning); request.onReasoning?.('**Note**'); return 'answer'; };
  const on = await fixture(t, { generate: note });
  assert.equal((await on.post('/accounts/32/v1/responses', body)).status, 200);
  const chat = await on.post('/accounts/32/v1/chat/completions', { model: models[0], stream: true,
    messages: [{ role: 'user', content: 'hi' }] });
  assert.ok(!(await chat.text()).includes('reasoning_content'));
  assert.deepEqual(offered, ['undefined', 'undefined']);

  offered.length = 0;
  const off = await fixture(t, { generate: note }, { streamReasoning: false });
  const wire = await (await off.post('/accounts/32/v1/responses', { ...body, stream: true })).text();
  assert.deepEqual(offered, ['undefined']);
  assert.ok(!wire.includes('reasoning_summary') && !wire.includes('"type":"reasoning"'));
  for (const [value, expected] of [[undefined, true], ['', true], ['true', true], ['false', false], ['0', false], [' OFF ', false]]) {
    assert.equal(streamReasoningEnabled(value), expected, String(value));
  }
});

test('a note that arrives after the answer writes nothing and does not break the next request', async t => {
  let late;
  const { post } = await fixture(t, { async generate(_, request) { late = request.onReasoning; return 'answer'; } });
  const wire = await (await post('/accounts/32/v1/responses', { ...body, stream: true })).text();
  assert.equal(responseEvents(wire).at(-1).type, 'response.completed');
  late('**Too late**');
  await new Promise(resolve => setTimeout(resolve, 20));
  const after = await post('/accounts/32/v1/responses', { ...body, stream: true });
  assert.equal(after.status, 200);
  await after.text();
});
