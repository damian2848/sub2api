import test from 'node:test';
import assert from 'node:assert/strict';
import { request as httpRequest } from 'node:http';
import { createPrismServer } from '../src/server.mjs';
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
async function fixture(t, overrides, settings = {}) {
  const server = createPrismServer({ manager: mockManager(overrides), managementKey, ...settings });
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
  assert.deepEqual((await listing.json()).data.map(model => model.id), models);
  assert.equal((await post('/internal/accounts/32/bootstrap', { retry_probe: true }, managementKey)).status, 200);
});

test('HTTP body bounds apply and requests Prism cannot serve never reach the browser', async t => {
  let calls = 0;
  const { post } = await fixture(t, { async generate() { calls += 1; return 'unexpected'; } }, { bodyLimit: 4096 });
  assert.equal((await post('/accounts/32/v1/responses', { ...body, input: 'x'.repeat(8192) })).status, 413);
  const previous = await post('/accounts/32/v1/responses', { ...body, previous_response_id: 'resp_1' });
  assert.equal(previous.status, 400);
  assert.equal((await previous.json()).error.code, 'previous_response_not_supported');
  const image = await post('/accounts/32/v1/chat/completions', { model: models[0], messages: [{ role: 'user',
    content: [{ type: 'image_url', image_url: { url: 'data:image/png;base64,AAAA' } }] }] });
  assert.equal(image.status, 400);
  assert.equal((await image.json()).error.code, 'image_input_not_supported');
  assert.equal((await post('/accounts/32/v1/responses', { ...body, model: 'unknown' })).status, 400);
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
