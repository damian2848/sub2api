import test from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { createStreamWriter, estimatedUsage, mapEffort, parseRequest, resultBody, writeCompletedStream } from '../src/protocol.mjs';
import { PrismError } from '../src/errors.mjs';
import { TOOL_INSTRUCTIONS_LIMIT } from '../src/emulation.mjs';

const models = ['gpt-6.1-sol'];
const model = models[0];
const parse = (body, family = 'responses', limits) => parseRequest({ model, ...body }, family, models, limits);
const text = request => request.input[0].content[0].text;
const rejects = (body, code, family = 'responses') => assert.throws(() => parse(body, family),
  error => error.status === 400 && error.code === code, `expected ${code}`);

const message = (role, value, type = 'input_text') => ({ type: 'message', role, content: [{ type, text: value }] });
const execTool = { type: 'function', name: 'exec_command', description: 'Runs a command in a PTY.\nMore detail here.',
  parameters: { type: 'object', properties: { cmd: { type: 'string' } }, required: ['cmd'] } };

// A realistic Codex CLI request: huge instructions, developer message, scaffolding user message with the
// environment, the real task, a prior tool round trip and Codex's additional_tools input item.
function codexBody(extra = {}) {
  return { instructions: 'You are Codex, a coding agent based on GPT-5. '.repeat(1500), stream: true, store: false,
    parallel_tool_calls: true, tool_choice: 'auto', include: ['reasoning.encrypted_content'], prompt_cache_key: 'abc',
    text: { verbosity: 'low' }, reasoning: { effort: 'high', summary: 'auto' }, input: [
      message('developer', '<permissions instructions>Sandbox: workspace-write</permissions instructions>'),
      { type: 'additional_tools', role: 'developer', tools: [
        { type: 'namespace', name: 'functions', tools: [execTool,
          { type: 'custom', name: 'apply_patch', description: 'freeform patch' }] },
        { type: 'namespace', name: 'browser', tools: [{ type: 'function', name: 'open_url', description: 'Open a URL',
          parameters: { type: 'object', properties: { url: { type: 'string' } } } }] },
        { type: 'namespace', name: 'mcp__github', tools: [{ type: 'function', name: 'create_issue', parameters: {} }] }] },
      message('user', '<recommended_plugins>Plugin list</recommended_plugins>\n<environment_context>\n  <cwd>/srv/app</cwd>\n  <shell>zsh</shell>\n</environment_context>'),
      message('user', 'List the files here and tell me how many there are.'),
      { type: 'reasoning', id: 'rs_1', summary: [], encrypted_content: 'gAAA' },
      { type: 'function_call', call_id: 'call_1', name: 'exec_command', arguments: '{"cmd":"ls"}' },
      { type: 'function_call_output', call_id: 'call_1', output: 'Chunk ID: 1\nOutput:\na.txt\nb.txt\n' },
    ], ...extra };
}

test('single user text is passed through byte for byte, whatever the surrounding parameters', () => {
  const exact = '  Hello\n\n\tagain  \n';
  assert.equal(text(parse({ input: [{ role: 'user', content: [{ type: 'input_text', text: 'Hello' },
    { type: 'text', text: ' again' }] }], reasoning: { effort: 'high' }, store: false })), 'Hello again');
  assert.equal(text(parse({ input: exact })), exact);
  assert.equal(text(parse({ messages: [{ role: 'user', content: exact }] }, 'chat')), exact);
  assert.equal(text(parse({ input: [{ role: 'user', content: 'é 漢字 😀' }] })), 'é 漢字 😀');
  const request = parse({ input: 'Hello', tools: [], instructions: '', temperature: 0.2 });
  assert.equal(text(request), 'Hello');
  assert.equal(request.tools, null);
  assert.deepEqual(request.input.map(item => [item.type, item.role, item.content.map(part => part.type)]),
    [['message', 'user', ['input_text']]]);
  // The readiness probe text of accounts.mjs must survive untouched.
  const probe = 'Reply with only the uppercase spelling of ready. Do not edit files or use tools.';
  assert.equal(text(parse({ input: probe })), probe);
});

test('Codex scaffolding is dropped and a lone real user message is still passed through unchanged', () => {
  const request = parse({ input: [message('user', '<skills_instructions>Skills</skills_instructions>'),
    message('user', '  <apps_instructions>Apps</apps_instructions>'), message('user', 'Real question')] });
  assert.equal(text(request), 'Real question');
});

test('Codex Responses body folds into exactly one user message with task, transcript, actions and environment', () => {
  const request = parse(codexBody());
  assert.equal(request.input.length, 1);
  assert.deepEqual([request.input[0].type, request.input[0].role, request.input[0].content.length, request.input[0].content[0].type],
    ['message', 'user', 1, 'input_text']);
  const prompt = text(request);
  assert.match(prompt, /^<role>action emitter<\/role>/);
  assert.match(prompt, /TASK:\nList the files here and tell me how many there are\.\n/);
  assert.match(prompt, /TRANSCRIPT SO FAR:\n\[executor ran\]\nexec_command \{"cmd":"ls"\}\n\n\[result\]\nChunk ID: 1/);
  assert.match(prompt, /Available actions:\n- exec_command\n {4}params: \{"type":"object"/);
  assert.match(prompt, /- open_url\n/);
  assert.match(prompt, /Actions: exec_command, open_url\n/);
  assert.match(prompt, /<environment_context>\n {2}<cwd>\/srv\/app<\/cwd>\n {2}<shell>zsh<\/shell>\n<\/environment_context>/);
  assert.ok(!prompt.includes('You are Codex'), 'client instructions must not be forwarded in tool mode');
  assert.ok(!prompt.includes('Plugin list') && !prompt.includes('recommended_plugins'));
  assert.ok(!prompt.includes('apply_patch\n') && !prompt.includes('create_issue') && !prompt.includes('mcp__'));
  assert.ok(!prompt.includes('encrypted_content') && !prompt.includes('gAAA'));
  assert.equal(request.effort, 'high');
  assert.equal(request.stream, true);
  assert.deepEqual([...request.tools], [['exec_command', 'functions'], ['open_url', 'browser']]);
});

test('Codex tool call replies become function_call items; the functions namespace is omitted', () => {
  const request = parse(codexBody());
  const plain = resultBody(request, '{"tool_call":{"name":"exec_command","arguments":{"cmd":"ls"}}}');
  const item = plain.output[0];
  assert.equal(item.type, 'function_call');
  assert.equal(item.name, 'exec_command');
  assert.equal(item.arguments, '{"cmd":"ls"}');
  assert.equal(item.status, 'completed');
  assert.match(item.id, /^fc_[0-9a-f]+$/);
  assert.match(item.call_id, /^call_[0-9a-f]+$/);
  assert.ok(!('namespace' in item));
  assert.equal(plain.output.length, 1);
  assert.equal(plain.output_text, '');
  const namespaced = resultBody(request, '```json\n{"tool_call":{"name":"open_url","arguments":{"url":"https://example.com"}}}\n```');
  assert.equal(namespaced.output[0].namespace, 'browser');
  // The same call from a tool that lives in a non-functions namespace carries that namespace.
  const shell = parse({ input: [message('user', 'go'), { type: 'additional_tools', tools: [
    { type: 'namespace', name: 'terminal', tools: [execTool] }] }] });
  assert.equal(resultBody(shell, '{"tool_call":{"name":"exec_command","arguments":{"cmd":"ls"}}}').output[0].namespace, 'terminal');
});

test('tool call parsing tolerates fences, short leading prose, missing braces and string arguments', () => {
  const request = parse({ input: 'run', tools: [execTool] });
  const call = reply => resultBody(request, reply).output[0];
  assert.equal(call('Sure thing:\n{"tool_call":{"name":"exec_command","arguments":{"cmd":"ls"}}}').type, 'function_call');
  const missing = call('{"tool_call":{"name":"exec_command","arguments":{"cmd":"ls","max_output_tokens":1000}}');
  assert.equal(missing.type, 'function_call');
  assert.deepEqual(JSON.parse(missing.arguments), { cmd: 'ls', max_output_tokens: 1000 });
  assert.deepEqual(JSON.parse(call('{"tool_call":{"name":"exec_command","arguments":"{\\"cmd\\":\\"pwd\\"}"}}').arguments), { cmd: 'pwd' });
  assert.equal(call('{"tool_call":{"name":"exec_command","arguments":{"cmd":"ls"}}} and then I will explain.').type, 'function_call');
  assert.equal(call('{"tool_call":{"name":"functions.exec_command"}}').arguments, '{}');
  // Long prose before the JSON or no JSON at all is plain text; an unknown tool fails safely.
  const prose = 'I will now carefully run the listing command for you: {"tool_call":{"name":"exec_command","arguments":{}}}';
  assert.equal(call(prose).type, 'message');
  assert.equal(call(prose).content[0].text, prose);
  const unknown = '{"tool_call":{"name":"rm_rf","arguments":{}}}';
  assert.throws(() => call(unknown), error => error.status === 502 && error.code === 'prism_unknown_tool');
  assert.equal(call('The directory has two files.').content[0].text, 'The directory has two files.');
  assert.equal(call('{"done":"Listed two files."}').content[0].text, 'Listed two files.');
  assert.equal(call('{"done": 3}').content[0].text, '{"done": 3}');
});

test('without tools a done-shaped or tool-shaped reply is returned verbatim', () => {
  const request = parse({ input: 'x' });
  const reply = '{"done":"literal"}';
  assert.equal(resultBody(request, reply).output[0].content[0].text, reply);
  const call = '{"tool_call":{"name":"exec_command","arguments":{}}}';
  assert.equal(resultBody(request, call).output[0].type, 'message');
});

test('Chat with tools produces tool_calls with finish_reason tool_calls', () => {
  const request = parse({ messages: [{ role: 'system', content: 'You are a shell helper.' },
    { role: 'user', content: 'List files' },
    { role: 'assistant', content: 'Running it.', tool_calls: [{ id: 'call_a', type: 'function',
      function: { name: 'exec_command', arguments: '{"cmd":"ls"}' } }] },
    { role: 'tool', tool_call_id: 'call_a', content: [{ type: 'text', text: 'a.txt' }] },
    { role: 'user', content: 'How many?' }],
  tools: [{ type: 'function', function: { name: 'exec_command', description: 'Run a command',
    parameters: { type: 'object', properties: { cmd: { type: 'string' } } } } }] }, 'chat');
  const prompt = text(request);
  assert.match(prompt, /^<role>action emitter<\/role>/);
  assert.match(prompt, /TASK:\nHow many\?\n/);
  assert.match(prompt, /\[user\]\nList files\n\n\[assistant\]\nRunning it\.\n\n\[executor ran\]\nexec_command \{"cmd":"ls"\}\n\n\[result\]\na\.txt/);
  // The short system prompt is forwarded as a delimited block after the protocol.
  assert.match(prompt, /Available actions:\n- [\s\S]*?\n\nCaller instructions \(follow them, but always reply in the tag format defined above\):\n<system_instructions>\nYou are a shell helper\.\n<\/system_instructions>\n\nTASK:/);
  const result = resultBody(request, '{"tool_call":{"name":"exec_command","arguments":{"cmd":"ls | wc -l"}}}');
  const choice = result.choices[0];
  assert.equal(choice.finish_reason, 'tool_calls');
  assert.equal(choice.message.content, null);
  assert.equal(choice.message.role, 'assistant');
  assert.equal(choice.message.tool_calls.length, 1);
  assert.match(choice.message.tool_calls[0].id, /^call_/);
  assert.equal(choice.message.tool_calls[0].type, 'function');
  assert.deepEqual(choice.message.tool_calls[0].function, { name: 'exec_command', arguments: '{"cmd":"ls | wc -l"}' });
  const answer = resultBody(request, '{"done":"Two files."}').choices[0];
  assert.deepEqual([answer.finish_reason, answer.message.content, 'tool_calls' in answer.message], ['stop', 'Two files.', false]);
});

test('plain multi-turn chat with a system message becomes a framed prompt', () => {
  const request = parse({ messages: [{ role: 'developer', content: 'Answer in French.' }, { role: 'system', content: 'Be terse.' },
    { role: 'user', content: 'hi' }, { role: 'assistant', content: 'salut' }, { role: 'user', content: 'bye' }] }, 'chat');
  assert.equal(request.tools, null);
  assert.equal(text(request), [
    'You are answering through an API bridge. Reply directly with the answer text. Do not create, edit or delete project files and do not use your own sandbox tools.',
    'Follow these system instructions from the caller:\n<system_instructions>\nAnswer in French.\n\nBe terse.\n</system_instructions>',
    'Conversation so far. Respond to the FINAL user message.\n\n[user]\nhi\n\n[assistant]\nsalut\n\n[user]\nbye'].join('\n\n'));
  // Responses: instructions alone frame a single user message too.
  const single = parse({ instructions: 'Reply in JSON.', input: 'Weather?' });
  assert.match(text(single), /<system_instructions>\nReply in JSON\.\n<\/system_instructions>\n\nConversation so far\. Respond to the FINAL user message\.\n\n\[user\]\nWeather\?$/);
  // History without instructions is framed as well.
  const history = parse({ input: [message('user', 'one'), message('assistant', 'two', 'output_text'), message('user', 'three')] });
  assert.ok(!text(history).includes('system_instructions'));
  assert.match(text(history), /\[user\]\none\n\n\[assistant\]\ntwo\n\n\[user\]\nthree$/);
  // System only: still answerable.
  assert.match(text(parse({ messages: [{ role: 'system', content: 'Say hi.' }] }, 'chat')), /\(no user message\)$/);
});

test('accepted input shapes: string, object, items, tool outputs, custom tools and skipped items', () => {
  assert.equal(text(parse({ input: { role: 'user', content: 'object form' } })), 'object form');
  assert.equal(text(parse({ input: ['bare string item'] })), 'bare string item');
  const request = parse({ tools: [execTool], input: [message('system', 'sys'), message('user', 'task'),
    { type: 'web_search_call', id: 'ws_1', status: 'completed' }, { type: 'reasoning', id: 'rs' },
    { type: 'custom_tool_call', call_id: 'c1', name: 'apply_patch', input: '*** Begin Patch' },
    { type: 'custom_tool_call_output', call_id: 'c1', output: 'Done!' },
    { type: 'function_call_output', call_id: 'c2', output: [{ type: 'input_text', text: 'part1' }, { type: 'input_text', text: 'part2' }] },
    { type: 'function_call_output', call_id: 'c3', output: { exit_code: 1 } }, message('assistant', 'Hmm.', 'output_text')] });
  const prompt = text(request);
  assert.match(prompt, /\[executor ran\]\napply_patch \*\*\* Begin Patch\n\n\[result\]\nDone!\n\n\[result\]\npart1part2\n\n\[result\]\n\{"exit_code":1\}\n\n\[assistant\]\nHmm\./);
  assert.ok(!prompt.includes('web_search') && !prompt.includes('rs'.repeat(2)));
  rejects({ input: [{ type: 'item_reference', id: 'msg_1' }] }, 'item_reference_not_supported');
  rejects({ input: [{ type: 'computer_call_output' }] }, 'unsupported_input_item');
  rejects({ input: [message('narrator', 'x')] }, 'unsupported_message_role');
  rejects({ messages: [{ role: 'narrator', content: 'x' }] }, 'unsupported_message_role', 'chat');
  rejects({ input: [{ role: 'user', content: [{ type: 'video', url: 'x' }] }] }, 'unsupported_content_type');
  rejects({ input: '' }, 'empty_input');
  rejects({ input: [] }, 'input_required');
  rejects({ messages: [] }, 'messages_required', 'chat');
  rejects({ messages: ['x'] }, 'expected_object', 'chat');
  rejects({ messages: [{ role: 'user', content: '  ' }] }, 'empty_input', 'chat');
  rejects({ input: 'x', stream: 'yes' }, 'expected_boolean');
  assert.throws(() => parseRequest(null, 'responses', models), error => error.code === 'expected_object');
});

test('audio, malformed images and unresolved file IDs are rejected wherever they appear', () => {
  const parts = [
    [{ type: 'image_url', image_url: { url: 'data:image/png;base64,AAAA' } }, 'invalid_attachment_data'],
    [{ type: 'input_file', file_id: 'file_1' }, 'attachment_file_id_not_supported'],
    [{ type: 'input_audio', input_audio: { data: 'AAAA', format: 'wav' } }, 'image_input_not_supported'],
    [{ type: 'file', file: { file_id: 'file_2' } }, 'attachment_file_id_not_supported'],
  ];
  for (const [part, code] of parts) {
    rejects({ input: [{ role: 'user', content: [{ type: 'input_text', text: 'look' }, part] }] }, code);
    rejects({ messages: [{ role: 'user', content: [{ type: 'text', text: 'look' }, part] }] }, code, 'chat');
    rejects({ input: [message('user', 'x'), { type: 'function_call_output', call_id: 'c', output: [part] }] }, code);
    rejects({ tools: [execTool], input: [{ role: 'user', content: [part] }] }, code);
  }
  assert.throws(() => parse({ input: [{ role: 'user', content: [parts[0][0]] }] }),
    error => error.status === 400 && error.param === 'input.0.content.0');
});

test('image and file parts in history and tool outputs keep ordered markers and exact bytes', () => {
  const png = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+j4e8AAAAASUVORK5CYII=';
  const image = { type: 'input_image', image_url: 'data:image/png;base64,' + png };
  const file = { type: 'input_file', filename: 'code.txt', file_data: Buffer.from('test code').toString('base64') };
  const request = parse({ tools: [execTool], input: [
    { role: 'user', content: [{ type: 'input_text', text: 'Earlier image:' }, image] },
    message('assistant', 'I will inspect it.'),
    { type: 'function_call_output', call_id: 'c', output: [{ type: 'input_text', text: 'Fetched file:' }, file] },
    message('user', 'Read both attachments.'),
  ] });
  assert.equal(request.attachments.length, 2);
  assert.deepEqual(request.attachments.map(item => item.marker), ['[Attachment 1]', '[Attachment 2]']);
  assert.deepEqual(request.attachments[0].data, Buffer.from(png, 'base64'));
  assert.equal(request.attachments[1].data.toString(), 'test code');
  assert.match(text(request), /Earlier image:\n\[Attachment 1\]/);
  assert.match(text(request), /Fetched file:\n\[Attachment 2\]/);
  assert.ok(!text(request).includes(png));
  const chat = parse({ messages: [{ role: 'user', content: [
    { type: 'image_url', image_url: { url: image.image_url } }, { type: 'file', file },
  ] }] }, 'chat');
  assert.equal(chat.attachments.length, 2);
  assert.equal(text(chat).trim(), '[Attachment 1]\n\n[Attachment 2]');
  const remote = parse({ input: [{ role: 'user', content: [{ type: 'input_image', image_url: 'https://example.com/a.png' }] }] });
  assert.equal(remote.attachments[0].url, 'https://example.com/a.png');
  assert.equal(text(remote).trim(), '[Attachment 1]');
});

test('previous_response_id is rejected unless null; model must be in the catalog', () => {
  rejects({ input: 'x', previous_response_id: 'resp_previous' }, 'previous_response_not_supported');
  rejects({ input: 'x', previous_response_id: '' }, 'previous_response_not_supported');
  assert.equal(text(parse({ input: 'x', previous_response_id: null })), 'x');
  assert.throws(() => parseRequest({ model: 'gpt-6-astra', input: 'Hello' }, 'responses', models), error => error.code === 'model_not_available');
  assert.throws(() => parseRequest({ input: 'Hello' }, 'responses', models), error => error.code === 'model_not_available');
});

test('all other parameters are ignored, never rejected', () => {
  const ignored = { temperature: 0.2, top_p: 0.9, include: ['reasoning.encrypted_content'],
    prompt_cache_key: 'k', text: { format: { type: 'json_object' } }, store: true, metadata: { a: 'b' }, user: 'u', service_tier: 'flex',
    max_output_tokens: 20, max_tokens: 20, max_completion_tokens: 20, truncation: 'auto', stream_options: { include_usage: true, extra: 1 },
    seed: 1, n: 1, stop: ['x'], response_format: { type: 'text' }, frequency_penalty: 0, logit_bias: {}, some_future_param: { nested: true } };
  for (const family of ['responses', 'chat']) {
    const body = family === 'chat' ? { messages: [{ role: 'user', content: 'Hello' }], ...ignored } : { input: 'Hello', ...ignored };
    const request = parse(body, family);
    assert.equal(text(request), 'Hello');
    assert.equal(request.stream, false);
    assert.equal(request.includeUsage, true);
  }
  assert.equal(parse({ input: 'x', stream: null }).stream, false);
});

test('reasoning effort maps to Prism levels and never rejects', () => {
  const table = { none: 'low', minimal: 'low', low: 'low', medium: 'medium', high: 'high', xhigh: 'xhigh', HIGH: 'high', XHIGH: 'xhigh',
    max: 'xhigh', ultra: 'xhigh', ULTRA: 'xhigh', extrahigh: 'xhigh', 'extra-high': 'xhigh', extra_high: 'xhigh', 'Extra-High': 'xhigh' };
  for (const [given, expected] of Object.entries(table)) {
    assert.equal(mapEffort(given), expected, given);
    assert.equal(parse({ input: 'x', reasoning: { effort: given } }).effort, expected);
    assert.equal(parse({ messages: [{ role: 'user', content: 'x' }], reasoning_effort: given }, 'chat').effort, expected);
  }
  for (const odd of [undefined, null, '', 'turbo', 5, {}, 'constructor', '__proto__']) assert.equal(mapEffort(odd), 'medium');
  assert.equal(parse({ input: 'x' }).effort, 'medium');
  assert.equal(parse({ input: 'x', reasoning: { effort: 'turbo' } }).effort, 'medium');
  assert.equal(parse({ input: 'x', reasoning: 'high' }).effort, 'medium');
  assert.equal(parse({ messages: [{ role: 'user', content: 'x' }] }, 'chat').effort, 'medium');
});

test('long histories are clamped, never rejected; only an oversized final text is', () => {
  const messages = [{ role: 'system', content: 'Be brief.' }];
  for (let i = 0; i < 400; i += 1) {
    messages.push({ role: 'user', content: `question ${i} ${'q'.repeat(2000)}` }, { role: 'assistant', content: `answer ${i} ${'a'.repeat(2000)}` });
  }
  messages.push({ role: 'user', content: 'the final question' });
  const request = parse({ messages }, 'chat', { maxTranscriptChars: 6000 });
  const prompt = text(request);
  assert.match(prompt, /\.\.\.\(\d+ earlier steps omitted\)\.\.\.\n\n\[user\]\nquestion 399 /);
  assert.ok(prompt.endsWith('[user]\nthe final question'));
  assert.ok(prompt.includes(`answer 399 ${'a'.repeat(50)}`) && !prompt.includes('question 0 '));
  assert.ok(prompt.length < 6000 + 1500, `prompt is ${prompt.length} chars`);
  // The default budget comes from PRISM_MAX_TRANSCRIPT_CHARS (32000).
  assert.ok(text(parse({ messages }, 'chat')).length < 32000 + 1500);
  const before = process.env.PRISM_MAX_TRANSCRIPT_CHARS;
  process.env.PRISM_MAX_TRANSCRIPT_CHARS = '3000';
  try { assert.ok(text(parse({ messages }, 'chat')).length < 3000 + 1500); } finally {
    if (before === undefined) delete process.env.PRISM_MAX_TRANSCRIPT_CHARS; else process.env.PRISM_MAX_TRANSCRIPT_CHARS = before;
  }
  // Tool mode clamps the transcript but keeps the task whole.
  const tooled = parse({ tools: [execTool], messages: messages.slice(0, -1).concat({ role: 'user', content: 'x'.repeat(20000) }) }, 'chat',
    { maxTranscriptChars: 5000 });
  assert.ok(text(tooled).includes('x'.repeat(20000)));
  assert.match(text(tooled), /earlier steps omitted/);
  // A single gigantic tool result is truncated, not rejected.
  const huge = parse({ tools: [execTool], input: [message('user', 'task'), { type: 'function_call_output', call_id: 'c', output: 'y'.repeat(900000) }] });
  assert.ok(text(huge).length < 30000);
  assert.match(text(huge), /chars omitted/);
  // Only an oversized final text fails, and it is the final user message that decides.
  assert.throws(() => parse({ input: 'z'.repeat(300 * 1024) }), error => error.code === 'input_too_large' && error.param === 'input');
  assert.throws(() => parse({ messages: [{ role: 'user', content: 'old' }, { role: 'user', content: 'z'.repeat(300 * 1024) }] }, 'chat'),
    error => error.code === 'input_too_large' && error.param === 'messages');
  assert.throws(() => parse({ input: 'z'.repeat(2000) }, 'responses', 1000), error => error.code === 'input_too_large');
  assert.equal(text(parse({ input: 'z'.repeat(2000) }, 'responses', { maxTextBytes: 2000 })).length, 2000);
  // Multibyte text is measured in bytes.
  assert.throws(() => parse({ input: '漢'.repeat(400) }, 'responses', { maxTextBytes: 1000 }), error => error.code === 'input_too_large');
});

test('a very large tool list is shrunk instead of rejected', () => {
  const tools = Array.from({ length: 600 }, (_, i) => ({ type: 'function', name: `tool_${i}`, description: 'd'.repeat(300),
    parameters: { type: 'object', properties: Object.fromEntries(Array.from({ length: 40 }, (_, k) => [`p${k}`, { type: 'string', description: 'x'.repeat(30) }])) } }));
  const prompt = text(parse({ input: 'go', tools }));
  assert.ok(prompt.length < 256 * 1024);
  assert.ok(prompt.includes('- tool_599'));
});

test('usage is estimated from the whole flattened prompt and the emitted output', () => {
  const request = parse(codexBody());
  const prompt = text(request);
  const call = resultBody(request, '{"tool_call":{"name":"exec_command","arguments":{"cmd":"ls"}}}');
  assert.equal(call.usage.input_tokens, Math.ceil(prompt.length / 3));
  assert.equal(call.usage.output_tokens, Math.ceil('exec_command {"cmd":"ls"}'.length / 3));
  assert.equal(call.usage.total_tokens, call.usage.input_tokens + call.usage.output_tokens);
  assert.equal(call.usage.estimation, 'character_based_estimate');
  const plain = parse({ input: 'Hello' });
  const answer = resultBody(plain, 'Actual completed output');
  assert.equal(answer.usage.estimation, 'character_based_estimate');
  assert.equal(answer.usage.output_tokens, Math.ceil('Actual completed output'.length / 3));
  const chat = resultBody(parse({ messages: [{ role: 'user', content: 'Hello there' }] }, 'chat'), 'Hi');
  assert.deepEqual(chat.usage, { prompt_tokens: 4, completion_tokens: 1, total_tokens: 5, prompt_tokens_details: { cached_tokens: 0 },
    estimation: 'character_based_estimate' });
  // An estimated cache read is reported in both formats and never exceeds the prompt.
  const cachedCall = resultBody(request, '{"tool_call":{"name":"exec_command","arguments":{"cmd":"ls"}}}', undefined, { cachedTokens: 256 });
  assert.equal(cachedCall.usage.input_tokens_details.cached_tokens, Math.min(256, cachedCall.usage.input_tokens));
  assert.equal(cachedCall.usage.input_tokens, call.usage.input_tokens, 'input_tokens stays the whole prompt');
  const cachedChat = resultBody(parse({ messages: [{ role: 'user', content: 'x'.repeat(3000) }] }, 'chat'), 'Hi', undefined, { cachedTokens: 512 });
  assert.equal(cachedChat.usage.prompt_tokens_details.cached_tokens, 512);
  assert.equal(estimatedUsage([{ content: [{ text: 'abc' }] }], 'a', 999).input_tokens_details.cached_tokens, 1);
  assert.equal(estimatedUsage([{ content: [{ text: 'abc' }] }], 'a', -5).input_tokens_details.cached_tokens, 0);
  assert.equal(estimatedUsage([{ content: [{ text: 'abc' }] }], 'abcd').total_tokens, 3);
});

function capture() {
  let wire = '';
  return { res: { write(value) { wire += value; }, end() { wire += '<end>'; } }, wire: () => wire };
}
function responsesEvents(wire) {
  assert.ok(wire.endsWith('\n\n<end>'));
  return wire.slice(0, -'<end>'.length).trim().split('\n\n').map(block => {
    const [event, data, extra] = block.split('\n');
    assert.equal(extra, undefined);
    assert.match(event, /^event: [a-z_.]+$/);
    assert.match(data, /^data: \{/);
    const parsed = JSON.parse(data.slice(6));
    assert.equal(event.slice(7), parsed.type);
    return parsed;
  });
}
const chatChunks = wire => wire.slice(0, -'<end>'.length).trim().split('\n\n').map(block => {
  assert.match(block, /^data: /);
  return block === 'data: [DONE]' ? '[DONE]' : JSON.parse(block.slice(6));
});

test('Responses text SSE has the full event order and chunked deltas', () => {
  const long = 'héllo 😀 '.repeat(400);
  const request = parse({ input: 'Hello', stream: true });
  const result = resultBody(request, long);
  const { res, wire } = capture();
  writeCompletedStream(res, request, result);
  const events = responsesEvents(wire());
  const types = events.map(event => event.type);
  const deltas = events.filter(event => event.type === 'response.output_text.delta');
  assert.ok(deltas.length > 1 && deltas.every(delta => delta.delta.length <= 600));
  assert.equal(deltas.map(delta => delta.delta).join(''), long);
  assert.ok(deltas.every(delta => !/[\ud800-\udbff]$/.test(delta.delta) && !/^[\udc00-\udfff]/.test(delta.delta)));
  assert.deepEqual([...new Set(types)], ['response.created', 'response.in_progress', 'response.output_item.added',
    'response.content_part.added', 'response.output_text.delta', 'response.output_text.done', 'response.content_part.done',
    'response.output_item.done', 'response.completed']);
  assert.equal(types.at(-1), 'response.completed');
  assert.deepEqual(events.map(event => event.sequence_number), events.map((_, index) => index));
  assert.equal(events.find(event => event.type === 'response.output_text.done').text, long);
  assert.equal(events.find(event => event.type === 'response.content_part.done').part.text, long);
  assert.equal(events.find(event => event.type === 'response.output_item.added').item.status, 'in_progress');
  assert.equal(events.at(-1).response.output[0].content[0].text, long);
  assert.equal(events[0].response.status, 'in_progress');
  assert.equal(events.at(-1).response.usage.estimation, 'character_based_estimate');
  assert.ok(wire().endsWith('<end>'));
});

function writableResponse() {
  const res = new EventEmitter();
  res.frames = [];
  res.write = frame => { res.frames.push(frame); return !res.blocked; };
  res.end = () => { res.writableEnded = true; };
  return res;
}
const asyncEvents = res => res.frames.filter(frame => frame.startsWith('event: '))
  .map(frame => JSON.parse(frame.split('\n')[1].slice(6)));

test('async Responses lifecycle is early and keeps one identity and continuous sequences through final tools', async () => {
  const request = parse({ input: 'run', tools: [execTool], stream: true });
  const res = writableResponse();
  const writer = createStreamWriter(res, request);
  await writer.begin();
  await writer.begin();
  const early = asyncEvents(res);
  assert.deepEqual(early.map(event => event.type), ['response.created', 'response.in_progress']);
  assert.equal(res.writableEnded, undefined);
  assert.ok(early.every(event => event.response.output.length === 0 && event.response.usage === null));
  const result = resultBody(request, '{"tool_call":{"name":"exec_command","arguments":{"cmd":"ls"}}}', writer.identity);
  await writer.finish(result);
  await writer.finish(result);
  await writer.heartbeat();
  const events = asyncEvents(res);
  assert.deepEqual(events.map(event => event.sequence_number), events.map((_, index) => index));
  assert.equal(events.filter(event => event.type === 'response.output_item.added').length, 1);
  assert.equal(events.filter(event => event.type === 'response.completed').length, 1);
  assert.equal(events[0].response.id, events.at(-1).response.id);
  assert.equal(events[0].response.created_at, events.at(-1).response.created_at);
  assert.equal(events.at(-1).response.output[0].arguments, '{"cmd":"ls"}');
  assert.equal(res.writableEnded, true);
});

test('while Prism generates, a Responses stream sends real in_progress events as heartbeats and Chat only comments', async () => {
  const responses = writableResponse();
  const writer = createStreamWriter(responses, parse({ input: 'x', stream: true }));
  const tick = () => new Promise(resolve => setImmediate(resolve));
  await writer.begin();
  await tick();
  await writer.heartbeat();
  await tick();
  await writer.heartbeat();
  const events = asyncEvents(responses);
  assert.deepEqual(events.map(event => event.type), ['response.created', 'response.in_progress', 'response.in_progress', 'response.in_progress']);
  assert.deepEqual(events.map(event => event.sequence_number), [0, 1, 2, 3], 'sequence numbers keep counting');
  assert.ok(events.every(event => event.response.id === writer.identity.id && event.response.status === 'in_progress' &&
    event.response.output.length === 0 && event.response.usage === null), 'a heartbeat carries no content');
  // begin() and each heartbeat also send the comment, which keeps proxies open.
  assert.equal(responses.frames.filter(frame => frame === ': waiting for Prism\n\n').length, 3);
  await writer.finish(resultBody(parse({ input: 'x' }), 'done', writer.identity));
  assert.equal(asyncEvents(responses).filter(event => event.type === 'response.completed').length, 1);

  const chat = writableResponse();
  const chatWriter = createStreamWriter(chat, parse({ messages: [{ role: 'user', content: 'x' }], stream: true }, 'chat'));
  await chatWriter.begin();
  await tick();
  await chatWriter.heartbeat();
  assert.equal(chat.frames.filter(frame => frame === ': waiting for Prism\n\n').length, 2, 'begin and one heartbeat');
  assert.ok(!chat.frames.some(frame => String(frame).includes('response.in_progress')), 'Chat gets no Responses events');
});

test('async writes wait for drain and skip heartbeats while backpressured', async () => {
  const res = writableResponse();
  const writer = createStreamWriter(res, parse({ input: 'x', stream: true }));
  res.blocked = true;
  const beginning = writer.begin();
  await writer.heartbeat();
  assert.equal(res.frames.length, 1);
  assert.equal(asyncEvents(res).length, 0);
  assert.equal(res.listenerCount('drain'), 1);
  res.blocked = false;
  res.emit('drain');
  await beginning;
  assert.equal(asyncEvents(res).length, 2);
  assert.equal(res.listenerCount('drain'), 0);
  await writer.finish(resultBody(parse({ input: 'x' }), 'x', writer.identity));
});

test('backpressured close aborts writes, removes listeners and never emits remaining content', async () => {
  const res = writableResponse();
  const controller = new AbortController();
  const writer = createStreamWriter(res, parse({ input: 'x', stream: true }), {
    signal: controller.signal, onDisconnect: () => controller.abort(),
  });
  res.blocked = true;
  const beginning = writer.begin();
  res.destroyed = true;
  res.emit('close');
  await assert.rejects(beginning, error => error.code === 'request_cancelled');
  assert.equal(controller.signal.aborted, true);
  assert.equal(res.frames.length, 1);
  assert.equal(res.listenerCount('drain'), 0);
  assert.equal(res.listenerCount('close'), 0);
});

test('Responses errors use safe top-level fields and the next sequence even after timeout', async () => {
  const res = writableResponse();
  const controller = new AbortController();
  const writer = createStreamWriter(res, parse({ input: 'x', stream: true }), { signal: controller.signal });
  await writer.begin();
  controller.abort(new PrismError('request_timeout', 504));
  res.blocked = true;
  await writer.error(controller.signal.reason);
  await writer.error(new Error('do-not-expose-access-token'));
  const events = asyncEvents(res);
  assert.deepEqual(events.at(-1), { type: 'error', sequence_number: 2,
    code: 'request_timeout', message: 'Prism did not answer in time', param: null });
  assert.equal(events.filter(event => event.type === 'error').length, 1);
  assert.equal(res.writableEnded, true);
  assert.equal(res.listenerCount('drain'), 0);
});

test('an error frame stalled on drain ends when the request deadline arrives', async () => {
  const res = writableResponse();
  const controller = new AbortController();
  const writer = createStreamWriter(res, parse({ input: 'x', stream: true }), { signal: controller.signal });
  await writer.begin();
  res.blocked = true;
  const ending = writer.error(new Error('secret-access-token'));
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(res.listenerCount('drain'), 1);
  controller.abort(new PrismError('request_timeout', 504));
  await ending;
  assert.equal(res.writableEnded, true);
  assert.equal(res.listenerCount('drain'), 0);
  assert.deepEqual(asyncEvents(res).at(-1), { type: 'error', sequence_number: 2,
    code: 'browser_operation_failed', message: 'browser_operation_failed', param: null });
});

test('async Chat emits the role once before content and preserves its initial identity', async () => {
  const request = parse({ messages: [{ role: 'user', content: 'Hello' }], stream: true }, 'chat');
  const res = writableResponse();
  const writer = createStreamWriter(res, request);
  await writer.begin();
  const initial = JSON.parse(res.frames[1].slice(6));
  assert.deepEqual(initial.choices[0].delta, { role: 'assistant', content: '' });
  await writer.finish(resultBody(request, 'answer', writer.identity));
  const chunks = res.frames.filter(frame => frame.startsWith('data: {')).map(frame => JSON.parse(frame.slice(6)));
  assert.equal(chunks.filter(chunk => chunk.choices[0]?.delta.role === 'assistant').length, 1);
  assert.ok(chunks.every(chunk => chunk.id === initial.id && chunk.created === initial.created));
  assert.equal(chunks.filter(chunk => chunk.choices[0]?.delta.content).map(chunk => chunk.choices[0].delta.content).join(''), 'answer');
  assert.equal(res.frames.at(-1), 'data: [DONE]\n\n');
});

test('Responses function call SSE streams the arguments and ends with the completed response', () => {
  const request = parse(codexBody());
  const args = { cmd: 'echo ' + 'x'.repeat(1500) };
  const result = resultBody(request, JSON.stringify({ tool_call: { name: 'open_url', arguments: args } }));
  const { res, wire } = capture();
  writeCompletedStream(res, request, result);
  const events = responsesEvents(wire());
  assert.deepEqual([...new Set(events.map(event => event.type))], ['response.created', 'response.in_progress',
    'response.output_item.added', 'response.function_call_arguments.delta', 'response.function_call_arguments.done',
    'response.output_item.done', 'response.completed']);
  const added = events.find(event => event.type === 'response.output_item.added').item;
  assert.deepEqual([added.type, added.status, added.arguments, added.name, added.namespace], ['function_call', 'in_progress', '', 'open_url', 'browser']);
  const deltas = events.filter(event => event.type === 'response.function_call_arguments.delta');
  assert.ok(deltas.length > 1);
  assert.equal(deltas.map(event => event.delta).join(''), JSON.stringify(args));
  assert.ok(deltas.every(event => event.item_id === added.id && event.output_index === 0));
  const done = events.find(event => event.type === 'response.function_call_arguments.done');
  assert.equal(done.arguments, JSON.stringify(args));
  const finished = events.find(event => event.type === 'response.output_item.done').item;
  assert.deepEqual([finished.status, finished.arguments, finished.call_id], ['completed', JSON.stringify(args), result.output[0].call_id]);
  assert.equal(events.at(-1).response.output[0].type, 'function_call');
  assert.deepEqual(events.map(event => event.sequence_number), events.map((_, index) => index));
});

test('Chat text SSE: role chunk, content chunks, finish chunk, optional usage chunk, then DONE', () => {
  const long = 'word '.repeat(500);
  const request = parse({ messages: [{ role: 'user', content: 'Hello' }], stream: true, stream_options: { include_usage: true } }, 'chat');
  const { res, wire } = capture();
  writeCompletedStream(res, request, resultBody(request, long));
  const chunks = chatChunks(wire());
  assert.equal(chunks.at(-1), '[DONE]');
  assert.deepEqual(chunks[0].choices[0].delta, { role: 'assistant', content: '' });
  const content = chunks.slice(1).filter(chunk => chunk !== '[DONE]' && chunk.choices[0]?.delta.content !== undefined);
  assert.ok(content.length > 1);
  assert.equal(content.map(chunk => chunk.choices[0].delta.content).join(''), long);
  const finish = chunks.find(chunk => chunk !== '[DONE]' && chunk.choices[0]?.finish_reason);
  assert.equal(finish.choices[0].finish_reason, 'stop');
  assert.deepEqual(finish.choices[0].delta, {});
  const usage = chunks.at(-2);
  assert.deepEqual(usage.choices, []);
  assert.equal(usage.usage.estimation, 'character_based_estimate');
  assert.ok(chunks.slice(0, -1).every(chunk => chunk.object === 'chat.completion.chunk' && chunk.model === model));
  assert.equal(new Set(chunks.slice(0, -1).map(chunk => chunk.id)).size, 1);
  // Without include_usage there is no usage chunk.
  const plain = parse({ messages: [{ role: 'user', content: 'Hello' }], stream: true }, 'chat');
  const second = capture();
  writeCompletedStream(second.res, plain, resultBody(plain, 'Hello back'));
  assert.ok(!second.wire().includes('"usage"'));
  assert.match(second.wire(), /"finish_reason":"stop"/);
  assert.ok(second.wire().endsWith('data: [DONE]\n\n<end>'));
});

test('Chat tool call SSE sends one tool_calls chunk with index 0 and finish_reason tool_calls', () => {
  const request = parse({ messages: [{ role: 'user', content: 'ls' }], tools: [execTool], stream: true,
    stream_options: { include_usage: true } }, 'chat');
  const { res, wire } = capture();
  writeCompletedStream(res, request, resultBody(request, '{"tool_call":{"name":"exec_command","arguments":{"cmd":"ls"}}}'));
  const chunks = chatChunks(wire());
  const calls = chunks.filter(chunk => chunk !== '[DONE]' && chunk.choices[0]?.delta.tool_calls);
  assert.equal(calls.length, 1);
  const [call] = calls[0].choices[0].delta.tool_calls;
  assert.deepEqual([call.index, call.type, call.function], [0, 'function', { name: 'exec_command', arguments: '{"cmd":"ls"}' }]);
  assert.match(call.id, /^call_/);
  assert.ok(!chunks.some(chunk => chunk !== '[DONE]' && typeof chunk.choices[0]?.delta.content === 'string' && chunk.choices[0].delta.content !== ''));
  const finish = chunks.find(chunk => chunk !== '[DONE]' && chunk.choices[0]?.finish_reason);
  assert.equal(finish.choices[0].finish_reason, 'tool_calls');
  assert.deepEqual(chunks.map(chunk => chunk === '[DONE]' ? 'done' : chunk.choices[0]?.finish_reason ? 'finish' : chunk.choices[0]?.delta.tool_calls ? 'tool' : chunk.choices.length ? 'role' : 'usage'),
    ['role', 'tool', 'finish', 'usage', 'done']);
});

test('blank system text alone is not an input, and a blank system message does not frame a plain prompt', () => {
  rejects({ messages: [{ role: 'system', content: '  ' }] }, 'empty_input', 'chat');
  assert.equal(text(parse({ messages: [{ role: 'system', content: '' }, { role: 'user', content: 'hi' }] }, 'chat')), 'hi');
  rejects({ tools: [execTool], input: [] }, 'input_required');
  rejects({ tools: [execTool], input: [message('user', '')] }, 'empty_input');
});

test('odd request shapes fail with a 400 or parse, never an unhandled exception', () => {
  const odd = [null, true, 7, 'x', [], {}, [null], [[]], [{}], { type: 'namespace', tools: 'x' }, { type: 'function', function: null },
    { type: 'message', role: ['user'], content: { type: 'text' } }, { type: 'function_call' }, { type: 'function_call_output', output: 5 },
    { role: 'assistant', tool_calls: [null, 1, { function: null }, { function: { name: 5 } }] }, { role: 'user', content: [null] }];
  const deep = { type: 'namespace', name: 'n', tools: [] };
  let nest = deep;
  for (let i = 0; i < 5000; i += 1) { const next = { type: 'namespace', name: 'n', tools: [] }; nest.tools.push(next); nest = next; }
  for (const value of [...odd, deep]) {
    for (const body of [{ input: value }, { input: [value] }, { input: 'x', tools: value }, { input: 'x', tools: [value] },
      { input: [message('user', 'x'), { type: 'additional_tools', tools: value }] }, { input: 'x', reasoning: value, instructions: value },
      { input: 'x', stream_options: value }]) {
      try { parse(body); } catch (error) { assert.equal(error.status, 400, String(error.stack).slice(0, 300)); }
    }
    for (const body of [{ messages: value }, { messages: [value] }, { messages: [{ role: 'user', content: 'x' }], tools: value },
      { messages: [{ role: 'user', content: 'x' }], tools: [value], reasoning_effort: value }]) {
      try { parse(body, 'chat'); } catch (error) { assert.equal(error.status, 400, String(error.stack).slice(0, 300)); }
    }
  }
});

test('Claude Code style Chat request keeps the environment lines, drops the system prompt and replays tool use', () => {
  const system = 'You are Claude Code, an interactive CLI tool. '.repeat(500) +
    '\n<env>\nWorking directory: /Users/dev/project\nIs directory a git repo: Yes\nPlatform: darwin\n</env>\nPrimary working directory: /Users/dev/project';
  const request = parse({ messages: [{ role: 'system', content: system }, { role: 'user', content: 'Fix the failing test in utils.' },
    { role: 'assistant', content: 'I will read the file first.', tool_calls: [{ id: 'toolu_1', type: 'function',
      function: { name: 'Read', arguments: '{"file_path":"/Users/dev/project/utils.ts"}' } }] },
    { role: 'tool', tool_call_id: 'toolu_1', content: 'export const add = (a, b) => a - b;' }],
  tools: [{ type: 'function', function: { name: 'Read', description: 'Reads a file from the local filesystem.',
    parameters: { type: 'object', properties: { file_path: { type: 'string' } }, required: ['file_path'] } } },
  { type: 'function', function: { name: 'Edit', description: 'Performs exact string replacements in files.',
    parameters: { type: 'object', properties: { file_path: { type: 'string' }, old_string: { type: 'string' }, new_string: { type: 'string' } } } } }],
  tool_choice: 'auto', parallel_tool_calls: true, max_tokens: 8192, stream: true }, 'chat');
  const prompt = text(request);
  assert.match(prompt, /^<role>action emitter<\/role>/);
  assert.ok(!prompt.includes('You are Claude Code'));
  assert.match(prompt, /<env>\nWorking directory: \/Users\/dev\/project\nIs directory a git repo: Yes\nPlatform: darwin\n<\/env>/);
  assert.match(prompt, /TASK:\nFix the failing test in utils\.\n/);
  assert.match(prompt, /\[assistant\]\nI will read the file first\.\n\n\[executor ran\]\nRead \{"file_path":"\/Users\/dev\/project\/utils\.ts"\}\n\n\[result\]\nexport const add/);
  assert.match(prompt, /Actions: Read, Edit\n/);
  const edit = resultBody(request, '{"tool_call":{"name":"Edit","arguments":{"file_path":"/Users/dev/project/utils.ts","old_string":"a - b","new_string":"a + b"}}}');
  assert.equal(edit.choices[0].finish_reason, 'tool_calls');
  assert.deepEqual(JSON.parse(edit.choices[0].message.tool_calls[0].function.arguments).new_string, 'a + b');
});

test('in tool mode a done reply carries the complete multi-line final answer', () => {
  const request = parse({ input: 'explain this', tools: [execTool] });
  const answer = reply => resultBody(request, reply).output[0];
  const multi = answer('{"done":"Line 1\\n\\n- point\\n- other **bold**"}');
  assert.equal(multi.type, 'message');
  assert.equal(multi.content[0].text, 'Line 1\n\n- point\n- other **bold**');
  assert.equal(resultBody(request, '{"done":"Line 1\\n\\n- point"}').output_text, 'Line 1\n\n- point');
  // Literal newlines inside the JSON string (invalid JSON) are repaired, fenced or with trailing prose.
  assert.equal(answer('{"done":"Line 1\n\n- point\n\ttabbed"}').content[0].text, 'Line 1\n\n- point\n\ttabbed');
  assert.equal(answer('```json\n{"done":"Line 1\n\n- point"}\n```').content[0].text, 'Line 1\n\n- point');
  assert.equal(answer('{"done":"Line 1\n- point"} Hope that helps!').content[0].text, 'Line 1\n- point');
  assert.equal(answer('{"done":"quote \\" and \\\\ and unicode \\u00e9"}').content[0].text, 'quote " and \\ and unicode é');
  // The same over Chat: a normal assistant message, not a tool call.
  const chat = parse({ messages: [{ role: 'user', content: 'hi' }], tools: [execTool] }, 'chat');
  const choice = resultBody(chat, '{"done":"Hello!\\n\\nHow can I help?"}').choices[0];
  assert.deepEqual([choice.finish_reason, choice.message.content, 'tool_calls' in choice.message], ['stop', 'Hello!\n\nHow can I help?', false]);
  // Streamed, the text is delivered whole and in order.
  const { res, wire } = capture();
  writeCompletedStream(res, request, resultBody(request, '{"done":"Line 1\\n\\n- point"}'));
  const events = responsesEvents(wire());
  assert.equal(events.filter(event => event.type === 'response.output_text.delta').map(event => event.delta).join(''), 'Line 1\n\n- point');
  assert.equal(events.find(event => event.type === 'response.output_text.done').text, 'Line 1\n\n- point');
});

test('in tool mode a raw prose reply that is not JSON becomes the message text', () => {
  const request = parse({ input: 'hi', tools: [execTool] });
  const prose = 'Hello! I can help with that.\n\n## Plan\n- one\n- two {not json}';
  const item = resultBody(request, prose).output[0];
  assert.equal(item.type, 'message');
  assert.equal(item.content[0].text, prose);
  const chat = parse({ messages: [{ role: 'user', content: 'hi' }], tools: [execTool] }, 'chat');
  const choice = resultBody(chat, prose).choices[0];
  assert.deepEqual([choice.finish_reason, choice.message.content], ['stop', prose]);
  // JSON that is neither a tool call nor a done string stays verbatim too.
  assert.equal(resultBody(request, '{"answer":"x"}').output[0].content[0].text, '{"answer":"x"}');
});

test('short caller instructions are forwarded in tool mode, large ones are dropped, at a fixed threshold', () => {
  assert.equal(TOOL_INSTRUCTIONS_LIMIT, 6000);
  const chat = system => text(parse({ messages: [{ role: 'system', content: system }, { role: 'user', content: 'go' }], tools: [execTool] }, 'chat'));
  const at = chat(`BEGIN${'i'.repeat(TOOL_INSTRUCTIONS_LIMIT - 5)}`);
  assert.ok(at.includes('<system_instructions>\nBEGIN') && at.includes('Caller instructions (follow them'));
  const over = chat(`BEGIN${'i'.repeat(TOOL_INSTRUCTIONS_LIMIT - 4)}`);
  assert.ok(!over.includes('BEGIN') && !over.includes('Caller instructions') && !over.includes('system_instructions'));
  // Whitespace around the text does not count; the combined text of every source does.
  assert.ok(chat(`  \n BEGIN${'i'.repeat(TOOL_INSTRUCTIONS_LIMIT - 5)} \n `).includes('BEGIN'));
  const combined = (a, b) => text(parse({ instructions: a, tools: [execTool], input: [message('developer', b), message('user', 'go')] }));
  const half = TOOL_INSTRUCTIONS_LIMIT / 2;
  const fits = combined(`A${'a'.repeat(half - 1)}`, `D${'d'.repeat(half - 3)}`);
  assert.ok(fits.includes('<system_instructions>\nA') && fits.includes('\n\nD'));
  const tooMuch = combined(`A${'a'.repeat(half - 1)}`, `D${'d'.repeat(half)}`);
  assert.ok(!tooMuch.includes('system_instructions') && !tooMuch.includes('aaaa') && !tooMuch.includes('dddd'));
  // Order: protocol, caller instructions, executor environment, task.
  const ordered = text(parse({ instructions: 'Always answer in French.', tools: [execTool], input: [
    message('user', '<environment_context><cwd>/w</cwd></environment_context>'), message('user', 'go')] }));
  const positions = ['Available actions:', 'Caller instructions', 'Always answer in French.', 'The executor runs here', '<cwd>/w</cwd>', 'TASK:']
    .map(marker => ordered.indexOf(marker));
  assert.ok(positions.every(position => position >= 0) && positions.every((position, index) => !index || position > positions[index - 1]), String(positions));
  // No instructions, no block. The huge Codex-style prompt stays out.
  assert.ok(!text(parse({ tools: [execTool], input: 'go' })).includes('Caller instructions'));
  assert.ok(!text(parse(codexBody())).includes('Caller instructions'));
});

// ---- several actions per reply, progress notes and plain final answers ----
const freeformExec = { type: 'function', name: 'exec', description: 'Run JavaScript in the executor.',
  parameters: { type: 'object', properties: { input: { type: 'string' } }, required: ['input'] } };
const twoCalls = 'Writing the file and checking it.\n<tool_call name="exec_command">{"cmd":"printf a > a.txt"}</tool_call>\n' +
  '<tool_call name="exec_command">{"cmd":"cat a.txt"}</tool_call>';

test('Responses: a progress note then one function_call per tag, in order, with usage that counts the calls', () => {
  const request = parse({ input: 'go', tools: [execTool] });
  const result = resultBody(request, twoCalls);
  assert.deepEqual(result.output.map(item => item.type), ['message', 'function_call', 'function_call']);
  assert.equal(result.output[0].content[0].text, 'Writing the file and checking it.');
  assert.equal(result.output_text, 'Writing the file and checking it.');
  assert.deepEqual(result.output.slice(1).map(item => JSON.parse(item.arguments).cmd), ['printf a > a.txt', 'cat a.txt']);
  assert.ok(result.output.slice(1).every(item => item.status === 'completed' && /^call_/.test(item.call_id) && /^fc_/.test(item.id) && !('namespace' in item)));
  assert.equal(new Set(result.output.map(item => item.id)).size, 3);
  assert.ok(result.usage.output_tokens >= Math.ceil(twoCalls.length / 3) - 25, 'the calls are part of the estimated output');
  // Without a note there is no empty message item in front of the calls.
  const bare = resultBody(request, '<tool_call name="exec_command">{"cmd":"ls"}</tool_call>');
  assert.deepEqual(bare.output.map(item => item.type), ['function_call']);
  assert.equal(bare.output_text, '');
});

test('Chat: the note is the content next to tool_calls, and finish_reason follows the calls', () => {
  const request = parse({ messages: [{ role: 'user', content: 'go' }], tools: [{ type: 'function', function: execTool }] }, 'chat');
  const result = resultBody(request, twoCalls);
  const choice = result.choices[0];
  assert.equal(choice.message.content, 'Writing the file and checking it.');
  assert.deepEqual(choice.message.tool_calls.map(call => JSON.parse(call.function.arguments).cmd), ['printf a > a.txt', 'cat a.txt']);
  assert.equal(choice.finish_reason, 'tool_calls');
  const answer = resultBody(request, 'All done. The file holds `a`.');
  assert.deepEqual([answer.choices[0].finish_reason, answer.choices[0].message.content, 'tool_calls' in answer.choices[0].message],
    ['stop', 'All done. The file holds `a`.', false]);
  const noNote = resultBody(request, '<tool_call name="exec_command">{"cmd":"ls"}</tool_call>');
  assert.equal(noNote.choices[0].message.content, null);
});

test('tool mode with a plain-text reply is a normal assistant message, not a wrapped answer', () => {
  const request = parse({ input: 'hi', tools: [execTool] });
  const result = resultBody(request, '你好！有什么可以帮你的吗？');
  assert.deepEqual(result.output.map(item => item.type), ['message']);
  assert.equal(result.output[0].content[0].text, '你好！有什么可以帮你的吗？');
});

test('a very large HTML file reaches a freeform exec byte for byte', () => {
  const rows = Array.from({ length: 600 }, (_, index) => `<g id="r${index}"><circle cx="${index}" cy="5" r="2" fill="#${(index * 977 % 0xffffff).toString(16).padStart(6, '0')}"/><text>"q" \\n ${index}</text></g>`);
  const html = `<!DOCTYPE html>\n<html><body>\n<svg viewBox="0 0 700 20">\n${rows.join('\n')}\n</svg>\n</body></html>\n`;
  const program = `await tools.exec_command({cmd: "printf '%s' " + ${JSON.stringify(JSON.stringify(html))} + " > pelican.html"});`;
  assert.ok(program.length > 40000);
  const request = parse({ input: 'write pelican.html', tools: [freeformExec] });
  const call = resultBody(request, `<tool_call name="exec">\n${program}\n</tool_call>`).output[0];
  assert.equal(JSON.parse(call.arguments).input, program);
});

test('Responses SSE with a note and several calls streams every item with its own output_index and one sequence', () => {
  const request = parse({ input: 'go', tools: [execTool], stream: true });
  const res = writableResponse();
  writeCompletedStream(res, request, resultBody(request, twoCalls));
  const events = res.frames.filter(frame => frame.startsWith('event: ')).map(frame => JSON.parse(frame.split('\n')[1].slice(6)));
  assert.deepEqual(events.map(event => event.sequence_number), events.map((_, index) => index));
  const added = events.filter(event => event.type === 'response.output_item.added');
  assert.deepEqual(added.map(event => [event.output_index, event.item.type]), [[0, 'message'], [1, 'function_call'], [2, 'function_call']]);
  assert.deepEqual(events.filter(event => event.type === 'response.output_item.done').map(event => event.output_index), [0, 1, 2]);
  assert.deepEqual(events.filter(event => event.type === 'response.function_call_arguments.done').map(event => JSON.parse(event.arguments).cmd),
    ['printf a > a.txt', 'cat a.txt']);
  assert.equal(events.at(-1).type, 'response.completed');
  assert.deepEqual(events.at(-1).response.output.map(item => item.type), ['message', 'function_call', 'function_call']);
});

test('Responses message items carry the channel: commentary next to calls, final_answer otherwise', () => {
  const request = parse({ input: 'go', tools: [execTool], stream: true });
  const withCalls = resultBody(request, twoCalls);
  assert.equal(withCalls.output[0].phase, 'commentary');
  assert.ok(withCalls.output.slice(1).every(item => !('phase' in item)));
  assert.equal(resultBody(request, 'All done.').output[0].phase, 'final_answer');
  assert.equal(resultBody(parse({ input: 'hi' }), 'Hello').output[0].phase, 'final_answer');
  // Streamed items keep the channel on added and done, so the client records it.
  for (const [reply, phase] of [[twoCalls, 'commentary'], ['All done.', 'final_answer']]) {
    const res = writableResponse();
    writeCompletedStream(res, request, resultBody(request, reply));
    const events = res.frames.filter(frame => frame.startsWith('event: ')).map(frame => JSON.parse(frame.split('\n')[1].slice(6)));
    const messages = events.filter(event => event.item?.type === 'message');
    assert.deepEqual(messages.map(event => [event.type, event.item.phase]),
      [['response.output_item.added', phase], ['response.output_item.done', phase]]);
    assert.equal(events.at(-1).response.output[0].phase, phase);
  }
  // Chat completions have no channel.
  const chat = parse({ messages: [{ role: 'user', content: 'go' }], tools: [{ type: 'function', function: execTool }] }, 'chat');
  assert.ok(!JSON.stringify(resultBody(chat, twoCalls)).includes('phase'));
});

test('Codex final answers deterministically link file paths, but commentary and Chat never change', () => {
  const input = [message('developer', 'Use a clickable markdown link for files.'),
    message('user', '<environment_context><cwd>/old/project</cwd></environment_context>'),
    message('user', '<environment_context><cwd>/Users/dev/project</cwd></environment_context>\nPlease edit the file.')];
  const request = parse({ input, tools: [execTool] });
  assert.deepEqual(request.fileLinks, { cwd: '/Users/dev/project' });
  const result = resultBody(request, 'Updated `src/app.py:12:3`.');
  assert.equal(result.output[0].phase, 'final_answer');
  assert.equal(result.output_text, 'Updated [app.py](/Users/dev/project/src/app.py:12:3).');
  assert.equal(result.output[0].content[0].text, result.output_text);
  const { res, wire } = capture();
  writeCompletedStream(res, request, result);
  const events = responsesEvents(wire());
  assert.equal(events.filter(event => event.type === 'response.output_text.delta').map(event => event.delta).join(''), result.output_text);
  assert.equal(events.at(-1).response.output_text, result.output_text);
  assert.equal(result.usage.output_tokens, estimatedUsage(request.input, 'Updated `src/app.py:12:3`.').output_tokens);
  const note = 'Checking `src/app.py`.';
  const commentary = resultBody(request, note + '\n<tool_call name="exec_command">{"cmd":"ls"}</tool_call>');
  assert.equal(commentary.output[0].phase, 'commentary');
  assert.equal(commentary.output_text, note);
  const chat = parse({ messages: input }, 'chat');
  assert.equal(chat.fileLinks, undefined);
  assert.equal(resultBody(chat, note).choices[0].message.content, note);
  for (const disabled of [parse({ input: '<cwd>/project</cwd>' }),
    parse({ instructions: 'clickable markdown link', input: '<cwd>/project</cwd>' }),
    parse({ instructions: 'clickable markdown link', input: 'no cwd' }),
    parse({ instructions: 'clickable markdown link', input: '<cwd>relative</cwd>' }),
    parse({ instructions: 'clickable markdown link <cwd>/project</cwd>', input: 'no cwd in input' })]) {
    assert.equal(disabled.fileLinks, undefined);
    assert.equal(resultBody(disabled, note).output_text, note);
  }
  const system = parse({ instructions: 'clickable markdown link', input: '<environment_context><cwd>/project</cwd></environment_context>' });
  assert.deepEqual(system.fileLinks, { cwd: '/project' });
  const developer = parse({ input: [message('developer',
    'Use a clickable markdown link. <environment_context><cwd>/developer</cwd></environment_context>'), message('user', 'go')] });
  assert.deepEqual(developer.fileLinks, { cwd: '/developer' });
});

test('file links use the real environment cwd despite assistant messages and tool output cwd tags', () => {
  for (const type of ['function_call_output', 'custom_tool_call_output']) {
    for (const output of ['<cwd>/w</cwd>', '<environment_context><cwd>/w</cwd></environment_context>']) {
      const request = parse({ instructions: 'Use a clickable markdown link for files.', input: [
        message('user', '<environment_context><cwd>/real</cwd></environment_context>'),
        message('assistant', '<environment_context><cwd>/assistant</cwd></environment_context>'),
        message('user', 'Quoted output: <cwd>/user-quote</cwd>'),
        { type, call_id: 'call_1', output },
      ] });
      assert.deepEqual(request.fileLinks, { cwd: '/real' });
      assert.equal(resultBody(request, 'Updated `src/app.py`.').output_text, 'Updated [app.py](/real/src/app.py).');
    }
  }
});

test('Chat SSE streams the note, then one tool_calls chunk carrying every call with its index', () => {
  const request = parse({ messages: [{ role: 'user', content: 'go' }], tools: [{ type: 'function', function: execTool }], stream: true }, 'chat');
  const res = writableResponse();
  writeCompletedStream(res, request, resultBody(request, twoCalls));
  const chunks = res.frames.filter(frame => frame.startsWith('data: {')).map(frame => JSON.parse(frame.slice(6)));
  const content = chunks.flatMap(chunk => chunk.choices[0]?.delta?.content ?? []).join('');
  assert.equal(content, 'Writing the file and checking it.');
  const calls = chunks.flatMap(chunk => chunk.choices[0]?.delta?.tool_calls ?? []);
  assert.deepEqual(calls.map(call => call.index), [0, 1]);
  assert.equal(chunks.at(-1).choices[0].finish_reason, 'tool_calls');
  assert.ok(res.frames.at(-1).endsWith('data: [DONE]\n\n'));
});


test('Chat and Responses normalize supported tool policy shapes and expose one shared policy', () => {
  for (const family of ['chat', 'responses']) {
    const base = family === 'chat' ? { messages: [{ role: 'user', content: 'Run ls.' }] } : { input: 'Run ls.' };
    for (const choice of ['auto', 'none', 'required']) {
      const request = parse({ ...base, tools: [execTool], tool_choice: choice, parallel_tool_calls: false }, family);
      assert.deepEqual(request.toolPolicy, { choice, parallel: false });
      // Even none retains the original specs to identify and reject forbidden calls.
      assert.ok(request.toolSpecs.has('exec_command'));
    }
    const forced = family === 'chat' ? { type: 'function', function: { name: 'exec_command' } }
      : { type: 'function', name: 'exec_command' };
    const request = parse({ ...base, tools: [execTool], tool_choice: forced }, family);
    assert.deepEqual(request.toolPolicy, { choice: 'function', name: 'exec_command', parallel: true });
    assert.match(text(request), /call only the action "exec_command"/);
    assert.deepEqual(parse(base, family).toolPolicy, { choice: 'auto', parallel: true });
    assert.deepEqual(parse({ ...base, tool_choice: null }, family).toolPolicy, { choice: 'auto', parallel: true });
  }
  const extra = parse({ input: [{ type: 'message', role: 'user', content: 'Use the extra tool.' },
    { type: 'additional_tools', tools: [{ type: 'namespace', name: 'terminal', tools: [execTool] }] }],
    tool_choice: { type: 'function', name: 'exec_command' } });
  assert.equal(extra.toolPolicy.name, 'exec_command');
  assert.equal(resultBody(extra, '<tool_call name="exec_command">{"cmd":"pwd"}</tool_call>').output[0].namespace, 'terminal');
});

test('unusable tool policies fall back to auto and never reject the request', () => {
  for (const family of ['chat', 'responses']) {
    const base = family === 'chat' ? { messages: [{ role: 'user', content: 'run' }] } : { input: 'run' };
    const policyOf = value => parse({ ...base, tools: [execTool], ...value }, family).toolPolicy;
    for (const choice of ['', 'sometimes', 'any', 7, true, [], {}, { type: 'custom', name: 'exec_command' },
      { type: 'allowed_tools', mode: 'auto', tools: [] }, { type: 'function' }]) {
      assert.deepEqual(policyOf({ tool_choice: choice }), { choice: 'auto', parallel: true }, JSON.stringify(choice));
    }
    // Only an explicit false turns parallel calls off; anything else keeps the default.
    for (const value of [null, 'false', 0, 1, [], {}, true]) assert.equal(policyOf({ parallel_tool_calls: value }).parallel, true);
    assert.equal(policyOf({ parallel_tool_calls: false }).parallel, false);
    const forced = name => family === 'chat' ? { type: 'function', function: { name } } : { type: 'function', name };
    for (const name of [null, '', 3, [], 'not_offered']) assert.equal(policyOf({ tool_choice: forced(name) }).choice, 'auto');
    assert.deepEqual(policyOf({ tool_choice: forced('exec_command') }), { choice: 'function', name: 'exec_command', parallel: true });
    // Either family's forced-function shape names the tool.
    assert.equal(policyOf({ tool_choice: family === 'chat' ? { type: 'function', name: 'exec_command' }
      : { type: 'function', function: { name: 'exec_command' } } }).choice, 'function');
    assert.equal(parse({ ...base, tool_choice: 'required' }, family).toolPolicy.choice, 'auto', 'required without tools is not enforceable');
    assert.equal(parse({ ...base, tools: [{ type: 'web_search' }], tool_choice: 'required' }, family).toolPolicy.choice, 'auto');
    assert.equal(policyOf({ tool_choice: 'required' }).choice, 'required');
    assert.equal(policyOf({ tool_choice: 'none' }).choice, 'none');
  }
});

test('both API result families enforce policy before shaping any partial result', () => {
  const one = '<tool_call name="exec_command">{"cmd":"ls"}</tool_call>';
  for (const family of ['chat', 'responses']) {
    const base = family === 'chat' ? { messages: [{ role: 'user', content: 'run' }] } : { input: 'run' };
    const request = policy => parse({ ...base, tools: [execTool], ...policy }, family);
    for (const [policy, reply] of [
      [{ tool_choice: 'required' }, 'All done.'],
      [{ parallel_tool_calls: false }, one + one],
      [{ tool_choice: family === 'chat' ? { type: 'function', function: { name: 'exec_command' } }
        : { type: 'function', name: 'exec_command' } }, 'All done.'],
    ]) {
      assert.throws(() => resultBody(request(policy), reply),
        error => error.status === 502 && error.code === 'prism_tool_policy_violation');
    }
    const forced = family === 'chat' ? { type: 'function', function: { name: 'exec_command' } }
      : { type: 'function', name: 'exec_command' };
    assert.throws(() => resultBody(request({ tool_choice: forced, parallel_tool_calls: true }), one + one),
      error => error.status === 502 && error.code === 'prism_tool_policy_violation');
    const accepted = resultBody(request({ tool_choice: 'required', parallel_tool_calls: false }), one);
    assert.equal(family === 'chat' ? accepted.choices[0].message.tool_calls.length : accepted.output.length, 1);
    const none = resultBody(request({ tool_choice: 'none' }), 'No action taken.');
    assert.equal(family === 'chat' ? none.choices[0].message.content : none.output_text, 'No action taken.');
    // tool_choice=none never emits a call; text that still holds tool tags goes back as text.
    const ignored = resultBody(request({ tool_choice: 'none' }), one);
    assert.equal(family === 'chat' ? ignored.choices[0].message.tool_calls : ignored.output.some(item => item.type === 'function_call'),
      family === 'chat' ? undefined : false);
    assert.equal(family === 'chat' ? ignored.choices[0].message.content : ignored.output_text, one);
    // A request that offered no tools is never parsed for calls: text that mentions the tags is the answer.
    const plain = resultBody(parse(base, family), one);
    assert.equal(family === 'chat' ? plain.choices[0].message.content : plain.output_text, one);
  }
});

// ---- Attachments beyond the per-request limit: newest are kept, older ones become a visible placeholder ----
const tinyPng = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+j4e8AAAAASUVORK5CYII=';
const pic = n => ({ type: 'input_image', image_url: `data:image/png;base64,${tinyPng}`, detail: `pic-${n}` });
const userWith = (text, ...parts) => ({ role: 'user', content: [{ type: 'input_text', text }, ...parts] });
const limited = (body, maxAttachments, family = 'responses') => parseRequest({ model: models[0], ...body }, family, models, { maxAttachments });

test('a history with more images than the limit keeps the newest and marks the older ones, never failing', () => {
  const input = [];
  for (let n = 1; n <= 5; n += 1) { input.push(userWith(`turn ${n}`, pic(n))); input.push(message('assistant', `answer ${n}`)); }
  input.push(message('user', 'what did you see?'));
  const request = limited({ input }, 3);
  assert.equal(request.attachments.length, 3, 'only the newest three are uploaded');
  assert.deepEqual(request.attachments.map(item => item.marker), ['[Attachment 1]', '[Attachment 2]', '[Attachment 3]']);
  const prompt = text(request);
  assert.equal((prompt.match(/\[Earlier image omitted\]/g) || []).length, 2, 'the two oldest become placeholders');
  assert.match(prompt, /turn 1:?\s*\n?\[Earlier image omitted\]/);
  assert.match(prompt, /turn 3:?\s*\n?\[Attachment 1\]/);
  assert.match(prompt, /turn 5:?\s*\n?\[Attachment 3\]/);
  assert.ok(prompt.indexOf('turn 1') < prompt.indexOf('[Attachment 1]'), 'order is preserved');
});

test('exactly at the limit nothing is folded', () => {
  const input = [userWith('a', pic(1)), message('assistant', 'ok'), userWith('b', pic(2), pic(3))];
  const request = limited({ input }, 3);
  assert.equal(request.attachments.length, 3);
  assert.ok(!text(request).includes('omitted'));
});

test('the current request still fails when its own attachments exceed the limit', () => {
  const input = [userWith('old', pic(1)), message('assistant', 'ok'), userWith('now', pic(2), pic(3), pic(4))];
  assert.throws(() => limited({ input }, 2), error => error.code === 'too_many_attachments' && error.status === 400);
  // without any history the behaviour is unchanged
  assert.throws(() => limited({ input: [userWith('only', pic(1), pic(2), pic(3))] }, 2), error => error.code === 'too_many_attachments');
});

test('an old attachment that is no longer valid cannot break a request once it is folded away', () => {
  const broken = { type: 'input_image', image_url: 'data:image/png;base64,AAAA' };
  const input = [userWith('stale', broken), message('assistant', 'ok'), userWith('b', pic(2)), message('assistant', 'ok'), userWith('c', pic(3))];
  const request = limited({ input }, 2);
  assert.equal(request.attachments.length, 2);
  assert.match(text(request), /\[Earlier image omitted\]/);
  // but a broken attachment that is still within the kept window is rejected as before
  assert.throws(() => limited({ input: [userWith('x', broken)] }, 2), error => error.code === 'invalid_attachment_data');
});

test('files and tool-output images count too, and the placeholder names the kind', () => {
  const file = { type: 'input_file', filename: 'a.txt', file_data: Buffer.from('hello').toString('base64') };
  const input = [
    userWith('first file', file),
    { type: 'function_call_output', call_id: 'c', output: [{ type: 'input_text', text: 'shot' }, pic(1)] },
    userWith('second', pic(2)),
  ];
  const request = limited({ tools: [execTool], input }, 2);
  assert.equal(request.attachments.length, 2);
  assert.match(text(request), /\[Earlier file omitted\]/);
  assert.ok(!text(request).includes('[Earlier image omitted]'));
});

test('the chat-completions family folds old images the same way', () => {
  const messages = [];
  for (let n = 1; n <= 4; n += 1) messages.push({ role: 'user', content: [{ type: 'text', text: `m${n}` }, { type: 'image_url', image_url: { url: `data:image/png;base64,${tinyPng}` } }] });
  const request = limited({ messages }, 2, 'chat');
  assert.equal(request.attachments.length, 2);
  assert.equal((text(request).match(/\[Earlier image omitted\]/g) || []).length, 2);
});

test('audio is still rejected even when it sits in a part that would be folded', () => {
  const input = [userWith('a', { type: 'input_audio', input_audio: { data: 'AAAA', format: 'wav' } }), userWith('b', pic(1)), userWith('c', pic(2))];
  assert.throws(() => limited({ input }, 1), error => error.code === 'image_input_not_supported');
});
