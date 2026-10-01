import test from 'node:test';
import assert from 'node:assert/strict';
import { NOISE, TOOL_INSTRUCTIONS_LIMIT, buildPrompt, clampTranscript, collectTools, elide, envContext, looseJSON, parseDone,
  parseToolCall, stripFence, toolProtocol, toolReminder } from '../src/emulation.mjs';

test('collectTools keeps plain functions, walks namespaces, skips custom, web_search and MCP tools', () => {
  const tools = collectTools([
    { type: 'function', name: 'exec_command', description: 'First paragraph.\nSecond line.\n\nSecond paragraph.', parameters: { cmd: 'string' } },
    { type: 'function', function: { name: 'nested', description: 'Chat shape', parameters: { a: 1 } } },
    { type: 'function', name: 'exec_command', description: 'duplicate' },
    { type: 'custom', name: 'apply_patch' }, { type: 'web_search' }, { type: 'function' }, null, 'junk',
    { type: 'namespace', name: 'mcp__x', tools: [{ type: 'function', name: 'mcp_tool' }] },
    { type: 'namespace', name: 'agents', tools: [{ type: 'function', name: 'spawn', input_schema: { b: 2 } }] },
  ], [{ type: 'message', role: 'user' }, { type: 'additional_tools', tools: [
    { type: 'namespace', name: 'functions', tools: [{ type: 'function', name: 'wait', description: 'Line one\nLine two', parameters: {} },
      { type: 'custom', name: 'exec' }] },
    { type: 'namespace', name: 'mcp__big', tools: [{ type: 'function', name: 'noisy' }] },
    { type: 'function', name: 'top_level_extra' }] }]);
  assert.deepEqual(tools.map(tool => [tool.ns, tool.name]), [[undefined, 'exec_command'], [undefined, 'nested'], ['agents', 'spawn'],
    ['functions', 'wait'], [undefined, 'top_level_extra']]);
  assert.equal(tools[0].desc, 'First paragraph. Second line.');
  assert.deepEqual(tools[1].params, { a: 1 });
  assert.deepEqual(tools[2].params, { b: 2 });
  assert.equal(tools[3].desc, 'Line one');
  assert.deepEqual(collectTools(undefined), []);
  assert.deepEqual(collectTools('x', 'y'.split('')), []);
});

test('envContext extracts only the executor environment', () => {
  const noise = '## Memory\n' + 'notes about another project '.repeat(400) + '\ncwd stuff';
  assert.ok(!envContext([noise]).includes('another project'));
  assert.equal(envContext(['<environment_context>cwd=/srv</environment_context>']), '<environment_context>cwd=/srv</environment_context>');
  assert.equal(envContext(['junk <cwd>/a/b</cwd> junk']), '<cwd>/a/b</cwd>');
  assert.equal(envContext(['<cwd>/a</cwd>', '<cwd>/a</cwd>']), '<cwd>/a</cwd>');
  assert.equal(envContext([undefined, '', 'text <filesystem>fs</filesystem> <env>\nWorking directory: /w\n</env>']),
    '<filesystem>fs</filesystem>\n<env>\nWorking directory: /w\n</env>');
  assert.equal(envContext(['x <environment_context>a <cwd>/q</cwd> b</environment_context> y']),
    '<environment_context>a <cwd>/q</cwd> b</environment_context>');
  // Fallback to loose lines only when there is no tagged block.
  assert.equal(envContext(['intro\nPrimary working directory: /repo\nPlatform: darwin\ncwd: /tmp/x']), 'Primary working directory: /repo\ncwd: /tmp/x');
  assert.equal(envContext(['no environment here']), '');
  assert.ok(envContext(['<cwd>' + 'x'.repeat(5000) + '</cwd>']).length <= 1500);
});

test('lowered Codex exec retains its complete executor API description', () => {
  const description = 'Run JavaScript in the executor.\n\n' + 'Runtime details. '.repeat(80) +
    '\nconst result = await tools.exec_command({cmd: "pwd"});\ntext(result);';
  const exec = { type: 'function', name: 'exec', description,
    parameters: { type: 'object', properties: { input: { type: 'string' } }, required: ['input'] } };
  for (const specs of [collectTools([exec]), collectTools([{ ...exec, name: 'functions__exec' }]), collectTools([], [{ type: 'additional_tools', tools: [
    { type: 'namespace', name: 'functions', tools: [exec] }] }])]) {
    assert.equal(specs[0].desc, description);
    const prompt = buildPrompt({ convo: [{ kind: 'user', text: 'Print the current directory.' }], tools: specs, budget: 1000 });
    assert.ok(prompt.includes(description));
    assert.ok(prompt.includes('"input":{"type":"string"}'));
    assert.match(prompt, /tools\.exec_command\(\{cmd: "pwd"\}\)/);
  }
  // An ordinary exec function with command arguments keeps the concise treatment.
  assert.equal(collectTools([{ ...exec, parameters: { properties: { cmd: { type: 'string' } } } }])[0].desc,
    'Run JavaScript in the executor.');
});

test('envContext stays linear on unclosed tags', () => {
  const hostile = '<cwd>'.repeat(40000) + '<env>'.repeat(40000);
  const started = Date.now();
  assert.equal(envContext([hostile]), '');
  assert.ok(Date.now() - started < 1000, 'environment scan must be linear');
});

test('NOISE matches Codex scaffolding only at the start of a message', () => {
  for (const tag of ['recommended_plugins', 'plugin_instructions', 'skills_instructions', 'apps_instructions']) {
    assert.ok(NOISE.test(`<${tag}>x</${tag}>`));
  }
  assert.ok(!NOISE.test('please read <recommended_plugins>'));
  assert.ok(!NOISE.test('<recommended_pluginsX>'));
});

test('clampTranscript keeps the newest entries and notes what was cut', () => {
  const big = Array.from({ length: 100 }, (_, i) => `step ${i} ${'x'.repeat(500)}`);
  const clamped = clampTranscript(big, 2000);
  assert.ok(clamped.reduce((sum, entry) => sum + entry.length, 0) < 3000);
  assert.match(clamped[0], /^\.\.\.\(\d+ earlier steps omitted\)\.\.\.$/);
  assert.equal(clamped.at(-1), big.at(-1));
  assert.equal(Number(/\((\d+) earlier/.exec(clamped[0])[1]), 100 - (clamped.length - 1));
  assert.deepEqual(clampTranscript([], 100), []);
  assert.deepEqual(clampTranscript(['a', 'b'], 100), ['a', 'b']);
  const only = clampTranscript(['y'.repeat(5000)], 1000);
  assert.equal(only.length, 1);
  assert.ok(only[0].startsWith('...(truncated)...\n') && only[0].length < 1100);
  // A newest entry bigger than the budget is truncated, older ones are reported as omitted.
  const mixed = clampTranscript(['old', 'z'.repeat(5000)], 1000);
  assert.deepEqual([mixed[0], mixed.length], ['...(1 earlier steps omitted)...', 2]);
  assert.ok(mixed[1].startsWith('...(truncated)...\n'));
});

test('clampTranscript never drops or counts the pinned entry', () => {
  const entries = ['a'.repeat(400), 'b'.repeat(400), `PIN ${'p'.repeat(5000)}`, 'c'.repeat(400)];
  const clamped = clampTranscript(entries, 500, 2);
  assert.deepEqual(clamped, ['...(2 earlier steps omitted)...', entries[2], entries[3]]);
  assert.deepEqual(clampTranscript(entries, 10000, 2), entries);
  assert.deepEqual(clampTranscript(['only pinned'], 1, 0), ['only pinned']);
  const lone = clampTranscript(['q'.repeat(3000), 'PIN'], 500, 1);
  assert.equal(lone.length, 2);
  assert.ok(lone[0].startsWith('...(truncated)...\n') && lone[1] === 'PIN');
});

test('elide keeps the head and the tail of long text', () => {
  assert.equal(elide('short', 100), 'short');
  const text = `HEAD${'m'.repeat(1000)}TAIL`;
  const cut = elide(text, 100);
  assert.ok(cut.startsWith('HEAD') && cut.endsWith('TAIL') && cut.includes('chars omitted'));
  assert.ok(cut.length < 160);
});

test('looseJSON, stripFence, parseDone and parseToolCall repair what models actually emit', () => {
  assert.deepEqual(looseJSON('{"a":1}'), { a: 1 });
  assert.deepEqual(looseJSON('  {"a":{"b":2}  '), { a: { b: 2 } });
  assert.deepEqual(looseJSON('{"a":"}"} trailing } prose'), { a: '}' });
  assert.deepEqual(looseJSON('{"a":{"b":{"c":1'.concat('}}')), { a: { b: { c: 1 } } });
  assert.equal(looseJSON('{"a":{"b":{"c":{"d":{"e":1'), null);
  for (const bad of ['prose', '[1]', '', null, undefined, '{not json}']) assert.equal(looseJSON(bad), null);
  assert.equal(stripFence('```json\n{"a":1}\n```'), '{"a":1}');
  assert.equal(stripFence('```\n{"a":1}```'), '{"a":1}');
  assert.equal(stripFence('Here you go: {"a":1}'), '{"a":1}');
  assert.equal(stripFence('x'.repeat(60) + '{"a":1}'), 'x'.repeat(60) + '{"a":1}');
  assert.equal(parseDone('{"done":"ok"} trailing prose'), 'ok');
  assert.equal(parseDone('{"done":"all good"}'), 'all good');
  assert.equal(parseDone('{"tool_call":{"name":"x"}}'), null);
  assert.equal(parseDone('{"done":{"a":1}}'), null);
  const tools = new Map([['shell', undefined], ['spawn', 'agents']]);
  const call = parseToolCall('```json\n{"tool_call":{"name":"shell","arguments":{"command":["ls"]}}}\n```', tools);
  assert.deepEqual([call.name, JSON.parse(call.arguments), call.namespace], ['shell', { command: ['ls'] }, undefined]);
  assert.match(call.id, /^call_[0-9a-f]{24}$/);
  assert.equal(parseToolCall('{"tool_call":{"name":"spawn","arguments":{}}}', tools).namespace, 'agents');
  assert.equal(parseToolCall('{"tool_call":{"name":"shell"}}', tools).arguments, '{}');
  assert.equal(parseToolCall('{"tool_call":{"name":"shell","arguments":["a"]}}', tools).arguments, '{}');
  assert.equal(parseToolCall('{"tool_call":{"name":"shell","arguments":"not json"}}', tools).arguments, '{}');
  assert.equal(parseToolCall('{"tool_call":{"name":"unknown","arguments":{}}}', tools), null);
  for (const bad of ['just prose', '{"not_a_tool":1}', '{"tool_call":"shell"}', '{"tool_call":{"arguments":{}}}', '{"tool_call":{"name":3}}']) {
    assert.equal(parseToolCall(bad, tools), null);
  }
});

test('buildPrompt: tool framing, plain passthrough and framed conversation', () => {
  const convo = [{ kind: 'user', text: '  first task  ' }, { kind: 'assistant', text: 'ok' }, { kind: 'user', text: 'second task' },
    { kind: 'ran', text: 'shell {}' }, { kind: 'result', text: 'out' }];
  const tooled = buildPrompt({ system: ['x'.repeat(TOOL_INSTRUCTIONS_LIMIT + 1)], convo, tools: [{ name: 'shell', params: { p: 1 }, desc: 'Run' }],
    envParts: ['<cwd>/w</cwd>'], budget: 1000 });
  assert.match(tooled, /^<role>next-action emitter<\/role>/);
  assert.match(tooled, /\n- shell\n {4}params: \{"p":1\}\n {4}Run\n/);
  assert.match(tooled, /\n<cwd>\/w<\/cwd>\n\nTASK:\nsecond task\n\nTRANSCRIPT SO FAR:\n\[user\]\n {2}first task {2}\n\n\[assistant\]\nok\n\n\[executor ran\]\nshell \{\}\n\n\[result\]\nout\n\n=+\nReply with ONE JSON object now\. Actions: shell\n/);
  assert.ok(!tooled.includes('xxxx') && !tooled.includes('Caller instructions'));
  assert.match(buildPrompt({ convo: [], tools: [{ name: 'shell', params: {}, desc: '' }], budget: 10 }), /TASK:\n\(none\)\n\nTRANSCRIPT SO FAR:\n\(empty - the executor has run nothing yet\)/);
  assert.equal(buildPrompt({ convo: [{ kind: 'user', text: ' raw ' }], tools: [], budget: 10 }), ' raw ');
  assert.equal(buildPrompt({ system: ['  ', ''], convo: [{ kind: 'user', text: 'raw' }], tools: [], budget: 10 }), 'raw');
  assert.equal(buildPrompt({ convo: [], tools: [], budget: 10 }), '');
  const framed = buildPrompt({ convo: [{ kind: 'assistant', text: 'a' }, { kind: 'user', text: 'u' }, { kind: 'result', text: 'r' }], tools: [], budget: 1000 });
  assert.match(framed, /\[assistant\]\na\n\n\[user\]\nu\n\n\[result\]\nr$/);
});

test('the protocol makes done the complete final reply and the reminder says the same', () => {
  const protocol = toolProtocol('- shell');
  assert.ok(!protocol.includes('one line summary') && !protocol.includes('<one line'));
  assert.ok(protocol.includes('{"done":"<complete final reply to the user>"}'));
  assert.ok(protocol.includes('COMPLETE reply the user will read'));
  assert.ok(protocol.includes('markdown allowed, newlines written as \\n inside the JSON string'));
  assert.ok(protocol.includes('a greeting, or a question\n    that can be answered from the conversation and the transcript alone'));
  assert.ok(protocol.includes('the TRANSCRIPT shows that the requested work is finished'));
  assert.ok(protocol.includes('Never claim work the TRANSCRIPT does not show. An empty transcript never proves'));
  assert.ok(!protocol.includes('Prose output breaks the pipeline'));
  const reminder = toolReminder('a, b');
  assert.ok(reminder.includes('Actions: a, b') && reminder.includes('{"done":"<complete reply to the user>"}'));
  assert.ok(reminder.includes('markdown allowed, \\n for newlines') && reminder.includes('Never claim work the TRANSCRIPT does not show'));
});

test('looseJSON repairs literal control characters inside strings only', () => {
  assert.deepEqual(looseJSON('{"done":"a\nb\tc"}'), { done: 'a\nb\tc' });
  assert.deepEqual(looseJSON('{\n  "done": "a\nb"\n}'), { done: 'a\nb' });
  assert.equal(parseDone('{"done":"x\u0001y"}'), 'x\u0001y');
});
