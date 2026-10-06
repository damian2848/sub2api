import test from 'node:test';
import assert from 'node:assert/strict';
import { MAX_TOOL_CALLS, NOISE, OUTPUT_RULES_LIMIT, TOOL_INSTRUCTIONS_LIMIT, buildPrompt, clampTranscript, collectTools, elide, envContext, isRawInputSchema,
  looseJSON, outputRules, parseDone, parseReply, parseToolCall, rawInput, rawInputFrom, stripFence, toolPolicyRules, toolProtocol, toolReminder } from '../src/emulation.mjs';
import { validateJsonSchema, validateLark } from '../src/tool-validation.mjs';

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
    assert.match(prompt, /- (?:functions__)?exec \(raw input\)/, 'a freeform exec is listed as raw input, not as a JSON schema');
    assert.ok(!prompt.includes('"input":{"type":"string"}'));
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
  assert.match(tooled, /^<role>action emitter<\/role>/);
  assert.match(tooled, /\n- shell\n {4}params: \{"p":1\}\n {4}Run\n/);
  assert.match(tooled, /\n<cwd>\/w<\/cwd>\n\nTASK:\nsecond task\n\nTRANSCRIPT SO FAR:\n\[user\]\n {2}first task {2}\n\n\[assistant\]\nok\n\n\[executor ran\]\nshell \{\}\n\n\[result\]\nout\n\n=+\nReply now\. Either write one or more <tool_call name="ACTION">\.\.\.<\/tool_call> tags, or answer\nin plain text if the TRANSCRIPT shows the work finished or nothing needs doing on the executor\.\nActions: shell\n/);
  assert.ok(!tooled.includes('xxxx') && !tooled.includes('Caller instructions'));
  assert.match(buildPrompt({ convo: [], tools: [{ name: 'shell', params: {}, desc: '' }], budget: 10 }), /TASK:\n\(none\)\n\nTRANSCRIPT SO FAR:\n\(empty - the executor has run nothing yet\)/);
  assert.equal(buildPrompt({ convo: [{ kind: 'user', text: ' raw ' }], tools: [], budget: 10 }), ' raw ');
  assert.equal(buildPrompt({ system: ['  ', ''], convo: [{ kind: 'user', text: 'raw' }], tools: [], budget: 10 }), 'raw');
  assert.equal(buildPrompt({ convo: [], tools: [], budget: 10 }), '');
  const framed = buildPrompt({ convo: [{ kind: 'assistant', text: 'a' }, { kind: 'user', text: 'u' }, { kind: 'result', text: 'r' }], tools: [], budget: 1000 });
  assert.match(framed, /\[assistant\]\na\n\n\[user\]\nu\n\n\[result\]\nr$/);
});

test('the protocol asks for tagged actions, plain-text answers and several actions per turn', () => {
  const protocol = toolProtocol('- shell');
  assert.ok(protocol.includes('<tool_call name="ACTION">') && protocol.includes('</tool_call>'));
  assert.ok(protocol.includes('If the action is marked (raw input), ARGUMENTS is the raw text itself'));
  assert.ok(protocol.includes('No JSON, no quotes around it, no escaping, no markdown fence.'));
  assert.ok(protocol.includes('Never wrap it in an object and never use a key such as "code".'));
  assert.ok(protocol.includes('Write several tags in ONE reply'));
  assert.ok(protocol.includes('Answer the user directly, in plain text with no tags'));
  assert.ok(protocol.includes('Do not explore (listing directories, reading instruction files) unless the task'));
  assert.ok(protocol.includes('Never claim work the TRANSCRIPT does not show. An empty\n   transcript never proves'));
  assert.ok(!protocol.includes('{"done"') && !protocol.includes('"tool_call"'), 'the JSON action format is gone from the prompt');
  assert.ok(protocol.trimEnd().endsWith('Available actions:\n- shell'));
  const reminder = toolReminder('a, b');
  assert.ok(reminder.includes('Actions: a, b') && reminder.includes('<tool_call name="ACTION">'));
  assert.ok(reminder.includes('(raw input) actions take the raw text between the tags, never JSON.'));
});

test('looseJSON repairs literal control characters inside strings only', () => {
  assert.deepEqual(looseJSON('{"done":"a\nb\tc"}'), { done: 'a\nb\tc' });
  assert.deepEqual(looseJSON('{\n  "done": "a\nb"\n}'), { done: 'a\nb' });
  assert.equal(parseDone('{"done":"x\u0001y"}'), 'x\u0001y');
});

// ---- the tag protocol ----
const rawSchema = { type: 'object', properties: { input: { type: 'string', description: 'The raw input.' } }, required: ['input'] };
const toolSpecs = () => new Map(collectTools([
  { type: 'function', name: 'exec', description: 'Run JavaScript.', parameters: rawSchema },
  { type: 'function', name: 'exec_command', description: 'Run a shell command.',
    parameters: { type: 'object', properties: { cmd: { type: 'string' } }, required: ['cmd'] } },
  { type: 'namespace', name: 'agents', tools: [{ type: 'function', name: 'spawn', parameters: { type: 'object', properties: { task: { type: 'string' } } } }] },
]).map(spec => [spec.name, spec]));

test('a freeform tool is recognised by its lowered {input: string} schema only', () => {
  assert.equal(isRawInputSchema(rawSchema), true);
  assert.equal(isRawInputSchema({ type: 'object', properties: { input: { type: 'string' } } }), true);
  assert.equal(isRawInputSchema({ type: 'object', properties: { input: { type: 'string' }, cwd: { type: 'string' } } }), false);
  assert.equal(isRawInputSchema({ type: 'object', properties: { input: { type: 'number' } } }), false);
  assert.equal(isRawInputSchema({ type: 'object', properties: { cmd: { type: 'string' } } }), false);
  assert.equal(isRawInputSchema(undefined), false);
  const specs = toolSpecs();
  assert.equal(specs.get('exec').raw, true);
  assert.equal(specs.get('exec_command').raw, false);
});

test('raw input between the tags reaches the executor exactly: no JSON, no escaping, any characters', () => {
  const html = '<!DOCTYPE html>\n<html lang="zh"><body>\n  <svg viewBox="0 0 10 10"><text>\\n "quoted" \'single\' {braces} </div></text></svg>\n<script>const a = {"k": [1, 2]}; console.log(`x${a.k}`);</script>\n</body></html>';
  const program = `await tools.exec_command({cmd: ${JSON.stringify('printf %s ' + html)}});`;
  const { calls, text } = parseReply(`<tool_call name="exec">\n${program}\n</tool_call>`, toolSpecs());
  assert.equal(calls.length, 1);
  assert.equal(calls[0].name, 'exec');
  assert.deepEqual(JSON.parse(calls[0].arguments), { input: program });
  assert.equal(text, '');
  assert.match(calls[0].id, /^call_[0-9a-f]{24}$/);
});

test('several tags become several calls in order; the text around them is a short progress note', () => {
  const reply = 'Writing the file, then checking it.\n<tool_call name="exec_command">{"cmd":"printf a > a.txt"}</tool_call>\n' +
    '<tool_call name="exec_command">\n{"cmd":"cat a.txt"}\n</tool_call>\nDone soon.';
  const { calls, text } = parseReply(reply, toolSpecs());
  assert.deepEqual(calls.map(call => JSON.parse(call.arguments).cmd), ['printf a > a.txt', 'cat a.txt']);
  assert.equal(text, 'Writing the file, then checking it.\n\nDone soon.');
  assert.equal(new Set(calls.map(call => call.id)).size, 2);
  const flood = Array.from({ length: MAX_TOOL_CALLS + 4 }, (_, index) => `<tool_call name="exec_command">{"cmd":"echo ${index}"}</tool_call>`).join('\n');
  assert.throws(() => parseReply(flood, toolSpecs()), error => error.status === 502 && error.code === 'prism_too_many_tool_calls');
});

test('a reply cut off inside the last tag still yields its call; namespaces and names are honoured', () => {
  const cut = parseReply('<tool_call name="exec">text(await tools.exec_command({cmd: "pwd"}));', toolSpecs());
  assert.equal(JSON.parse(cut.calls[0].arguments).input, 'text(await tools.exec_command({cmd: "pwd"}));');
  const spawn = parseReply('<tool_call name="spawn">{"task":"x"}</tool_call>', toolSpecs());
  assert.equal(spawn.calls[0].namespace, 'agents');
  assert.equal(parseReply('<tool_call name="functions.exec_command">{"cmd":"ls"}</tool_call>', toolSpecs()).calls[0].name, 'exec_command');
  assert.equal(parseReply("<tool_call name='exec_command'>{\"cmd\":\"ls\"}</tool_call>", toolSpecs()).calls.length, 1);
});

test('unknown tags reject the entire reply, including a mixed plan, instead of leaking or silently dropping them', () => {
  const unknown = '<tool_call name="rm_rf">{"path":"/"}</tool_call>';
  for (const reply of [unknown, `${unknown}<tool_call name="exec_command">{"cmd":"ls"}</tool_call>`,
    `<tool_call name="exec_command">{"cmd":"ls"}</tool_call>${unknown}`]) {
    assert.throws(() => parseReply(reply, toolSpecs()), error => error.status === 502 && error.code === 'prism_unknown_tool');
  }
});

test('raw input is unwrapped from a fence or a one-field JSON wrapper, a program that merely starts with a brace is kept', () => {
  assert.equal(rawInput('\nhello\n'), 'hello');
  assert.equal(rawInput('\n\nhello\n\n'), '\nhello\n', 'only one line break each side is layout');
  assert.equal(rawInput('```js\nconst a = 1;\n```'), 'const a = 1;');
  assert.equal(rawInput('```\nconst a = 1;\n```\n'), 'const a = 1;');
  assert.equal(rawInput('text\n```js\ncode\n```'), 'text\n```js\ncode\n```', 'a fence inside other text is content');
  for (const key of ['input', 'code', 'source', 'script', 'cmd']) {
    assert.equal(rawInputFrom(JSON.stringify({ [key]: 'notify("a");\ntext(1);' })), 'notify("a");\ntext(1);', key);
  }
  assert.equal(rawInputFrom('{"code":"x","extra":"y"}'), '{"code":"x","extra":"y"}', 'two fields are not a wrapper');
  assert.equal(rawInputFrom('{"code":5}'), '{"code":5}');
  assert.equal(rawInputFrom('{ const a = 1; }\ntext(a);'), '{ const a = 1; }\ntext(a);');
  const wrapped = parseReply('<tool_call name="exec">{"code":"text(await tools.exec_command({cmd: \\"pwd\\"}));"}</tool_call>', toolSpecs());
  assert.equal(JSON.parse(wrapped.calls[0].arguments).input, 'text(await tools.exec_command({cmd: "pwd"}));');
});

test('JSON arguments are repaired the way the older format was, an irreparable body fails safely', () => {
  assert.deepEqual(JSON.parse(parseReply('<tool_call name="exec_command">{"cmd":"ls"</tool_call>', toolSpecs()).calls[0].arguments), { cmd: 'ls' });
  assert.deepEqual(JSON.parse(parseReply('<tool_call name="exec_command">```json\n{"cmd":"ls"}\n```</tool_call>', toolSpecs()).calls[0].arguments), { cmd: 'ls' });
  assert.throws(() => parseReply('<tool_call name="exec_command">ls -la</tool_call>', toolSpecs()),
    error => error.status === 502 && error.code === 'prism_invalid_tool_arguments');
});

test('the older JSON action replies still work, and a raw-input action always ends up with {input: text}', () => {
  const legacy = parseReply('{"tool_call":{"name":"exec_command","arguments":{"cmd":"ls"}}}', toolSpecs());
  assert.deepEqual(JSON.parse(legacy.calls[0].arguments), { cmd: 'ls' });
  const keyed = parseReply('{"tool_call":{"name":"exec","arguments":{"code":"notify(\'a\');"}}}', toolSpecs());
  assert.deepEqual(JSON.parse(keyed.calls[0].arguments), { input: "notify('a');" });
  const stringArgs = parseReply('{"tool_call":{"name":"exec","arguments":"text(1);"}}', toolSpecs());
  assert.deepEqual(JSON.parse(stringArgs.calls[0].arguments), { input: 'text(1);' });
  const proper = parseReply('{"tool_call":{"name":"exec","arguments":{"input":"text(2);"}}}', toolSpecs());
  assert.deepEqual(JSON.parse(proper.calls[0].arguments), { input: 'text(2);' });
  assert.deepEqual(parseReply('{"done":"All finished."}', toolSpecs()), { calls: [], text: 'All finished.' });
});

test('a plain-text reply is the final answer, whatever it contains', () => {
  const answer = '你好！\n\n```js\nconsole.log(1)\n```\n- a\n- b';
  assert.deepEqual(parseReply(answer, toolSpecs()), { calls: [], text: answer });
  assert.deepEqual(parseReply('', toolSpecs()), { calls: [], text: '' });
  assert.deepEqual(parseReply(undefined, toolSpecs()), { calls: [], text: '' });
});

// ---- output conventions taken from long client instructions ----
const longInstructions = [
  '# Personality\nBe warm.\n\n# Working with the user\nTwo channels.\n\n## Intermediate commentary\nStart with a note in the `commentary` channel.\nDo NOT put a final response in the commentary channel.\n\n',
  '## Final answer\nFocus on the most important information.\n\n### Formatting rules\n- When referencing a real local file, prefer a clickable markdown link like [app.py](/abs/path/app.py:12).\n- Do not wrap markdown links in backticks.\n\n',
  '### Visualizations\nUse a chart when it helps.\n\n# Rules for getting work done\nRun the tests.\n',
  `<app-context>\n# Codex desktop context\n### Images/Visuals/Files\n- When referencing code or workspace files in responses, always use full absolute file paths.\n- Return web URLs as Markdown links.\n### Automations\n- Search for automation_update first.\n</app-context>`,
  'filler '.repeat(2000)].join('');

test('outputRules keeps only the sections that control the look of the reply', () => {
  const rules = outputRules([longInstructions]);
  assert.match(rules, /^## Intermediate commentary\nStart with a note in the `commentary` channel\./);
  assert.match(rules, /## Final answer\nFocus on the most important information\./);
  assert.match(rules, /## Formatting rules\n- When referencing a real local file, prefer a clickable markdown link like \[app\.py\]/);
  assert.match(rules, /## Images\/Visuals\/Files\n- When referencing code or workspace files in responses, always use full absolute file paths\./);
  for (const left of ['Be warm', 'Run the tests', 'Use a chart', 'automation_update', 'filler']) assert.ok(!rules.includes(left), left);
  assert.ok(rules.length <= OUTPUT_RULES_LIMIT);
  assert.equal(outputRules(['no headings here', '# Other\ntext']), '');
  assert.equal(outputRules([]), '');
  // Each section is capped and the total honours the limit.
  const big = `## Formatting rules\n${'a'.repeat(5000)}\n## Final answer\nshort`;
  assert.equal(outputRules([big]).length, '## Formatting rules\n'.length + 1800 + '\n\n## Final answer\nshort'.length);
  assert.ok(!outputRules([big], 1000).includes('Formatting rules'), 'a section that does not fit is skipped, later ones still fit');
  assert.match(outputRules([big], 1000), /## Final answer\nshort/);
});

test('buildPrompt forwards the output conventions, not the whole prompt, when client instructions are long', () => {
  const tools = [{ name: 'exec', params: {}, desc: 'Run', raw: true }];
  const convo = [{ kind: 'user', text: 'make a page' }];
  const prompt = buildPrompt({ system: [longInstructions], convo, tools, budget: 1000 });
  assert.match(prompt, /Output conventions from the caller\./);
  assert.match(prompt, /<output_conventions>\n## Intermediate commentary/);
  assert.match(prompt, /prefer a clickable markdown link like \[app\.py\]\(\/abs\/path\/app\.py:12\)/);
  assert.ok(prompt.indexOf('<output_conventions>') > prompt.indexOf('<role>action emitter</role>'));
  assert.ok(prompt.indexOf('</output_conventions>') < prompt.indexOf('TASK:'));
  for (const left of ['Be warm', 'Run the tests', 'filler', 'Caller instructions']) assert.ok(!prompt.includes(left), left);
  // Short instructions are still forwarded whole, and long ones without such sections add nothing.
  assert.match(buildPrompt({ system: ['Be brief.'], convo, tools, budget: 1000 }), /<system_instructions>\nBe brief\.\n<\/system_instructions>/);
  assert.ok(!buildPrompt({ system: ['x'.repeat(TOOL_INSTRUCTIONS_LIMIT + 1)], convo, tools, budget: 1000 }).includes('output_conventions'));
  // Without tools the instructions keep their existing handling.
  assert.ok(!buildPrompt({ system: [longInstructions], convo, tools: [], budget: 1000 }).includes('output_conventions'));
});


// Current-turn tool policy must be enforced for tag and legacy output alike.
test('tool policies reject required/forced/parallel violations without returning any partial calls', () => {
  const one = '<tool_call name="exec_command">{"cmd":"ls"}</tool_call>';
  const other = '<tool_call name="spawn">{"task":"x"}</tool_call>';
  const legacy = '{"tool_call":{"name":"exec_command","arguments":{"cmd":"ls"}}}';
  for (const [reply, policy] of [
    ['Already done.', { choice: 'required' }], ['{"done":"Already done."}', { choice: 'required' }],
    ['Already done.', { choice: 'function', name: 'exec_command' }],
    [other, { choice: 'function', name: 'exec_command' }],
    [one + other, { choice: 'function', name: 'exec_command' }],
    [one + one, { choice: 'auto', parallel: false }],
    [one + one, { choice: 'required', parallel: false }],
    [one + one, { choice: 'function', name: 'exec_command', parallel: false }],
    [one + one, { choice: 'function', name: 'exec_command', parallel: true }],
  ]) {
    assert.throws(() => parseReply(reply, toolSpecs(), policy),
      error => error.status === 502 && error.code === 'prism_tool_policy_violation', JSON.stringify(policy));
  }
  for (const reply of [one, legacy]) {
    for (const policy of [{ choice: 'auto', parallel: false }, { choice: 'required', parallel: false },
      { choice: 'function', name: 'exec_command', parallel: false }]) {
      assert.equal(parseReply(reply, toolSpecs(), policy).calls.length, 1);
    }
  }
  assert.equal(parseReply(one + one, toolSpecs(), { choice: 'auto', parallel: true }).calls.length, 2);
  assert.equal(parseReply(one + other, toolSpecs(), { choice: 'required', parallel: true }).calls.length, 2);
  assert.deepEqual(parseReply('No actions needed.', toolSpecs(), { choice: 'none', parallel: false }),
    { calls: [], text: 'No actions needed.' });
  // tool_choice=none never emits a call and never fails the reply: the text goes back as it is.
  for (const reply of [one, legacy, `Example: \`${one}\``]) {
    assert.deepEqual(parseReply(reply, toolSpecs(), { choice: 'none' }), { calls: [], text: reply });
  }
});

test('a tool without parameters may be called with an empty body, as before', () => {
  const noArgs = new Map(collectTools([{ type: 'function', name: 'no_args', parameters: {} }]).map(spec => [spec.name, spec]));
  const empty = parseReply('<tool_call name="no_args"></tool_call>', noArgs);
  assert.deepEqual(empty.calls.map(call => [call.name, call.arguments]), [['no_args', '{}']]);
  assert.equal(parseReply('<tool_call name="no_args">\n  \n</tool_call>', noArgs).calls[0].arguments, '{}');
});

test('tool arguments enforce Draft 7 and Draft 2020-12 object constraints before shaping a call', () => {
  const draft7 = { $schema: 'http://json-schema.org/draft-07/schema#', type: 'object', properties: {
    command: { type: 'string', minLength: 1 }, count: { type: 'integer', minimum: 1, maximum: 3 },
  }, required: ['command'], additionalProperties: false };
  assert.equal(validateJsonSchema({ command: 'ls', count: 2 }, draft7).valid, true);
  assert.equal(validateJsonSchema({ command: '', count: 2 }, draft7).valid, false);
  assert.equal(validateJsonSchema({ command: 'ls', count: 4 }, draft7).valid, false);
  assert.equal(validateJsonSchema({ command: 'ls', other: true }, draft7).valid, false);
  const draft2020 = { $schema: 'https://json-schema.org/draft/2020-12/schema', type: 'object', properties: {
    mode: { enum: ['read', 'write'] }, payload: { type: 'array', prefixItems: [{ type: 'string' }, { type: 'integer' }], minItems: 2 },
  }, required: ['mode', 'payload'], allOf: [{ required: ['mode'] }] };
  assert.equal(validateJsonSchema({ mode: 'read', payload: ['x', 1] }, draft2020).valid, true);
  assert.equal(validateJsonSchema({ mode: 'delete', payload: ['x', 1] }, draft2020).valid, false);
  assert.equal(validateJsonSchema({ mode: 'read', payload: ['x', '1'] }, draft2020).valid, false);
  const specs = new Map(collectTools([{ type: 'function', name: 'run', parameters: draft7 }]).map(spec => [spec.name, spec]));
  assert.throws(() => parseReply('<tool_call name="run">{"command":"ls","other":true}</tool_call>', specs),
    error => error.code === 'prism_invalid_tool_arguments');
  assert.throws(() => parseToolCall('{"tool_call":{"name":"run","arguments":{"other":true}}}', specs),
    error => error.code === 'prism_invalid_tool_arguments');
});

test('constrained Lark grammars validate raw and structured tool arguments without executing grammar code', () => {
  const words = 'start: WORD+\n%import common.WORD\n%import common.WS\n%ignore WS';
  assert.equal(validateLark('ls -la', words).valid, true);
  assert.equal(validateLark('ls && rm -rf /', words).valid, false);
  const raw = { type: 'object', properties: { input: { type: 'string' } }, required: ['input'], 'x-lark': words };
  const specs = new Map(collectTools([{ type: 'function', name: 'exec', parameters: raw }]).map(spec => [spec.name, spec]));
  assert.equal(parseReply('<tool_call name="exec">ls -la</tool_call>', specs).calls.length, 1);
  assert.throws(() => parseReply('<tool_call name="exec">ls && rm -rf /</tool_call>', specs),
    error => error.code === 'prism_invalid_tool_arguments');
  assert.equal(validateLark('anything', 'start: missing').valid, false);
});

test('an unknown tag that only appears inside markdown code is an example, not a call attempt', () => {
  const quoted = 'Write the call as `<tool_call name="unknown_tool">{}</tool_call>` and keep the JSON valid.';
  assert.deepEqual(parseReply(quoted, toolSpecs()), { calls: [], text: quoted });
  const fenced = 'Example:\n```xml\n<tool_call name="unknown_tool">\n{}\n</tool_call>\n```\nThat is all.';
  assert.deepEqual(parseReply(fenced, toolSpecs()), { calls: [], text: fenced });
  // A real call next to a quoted example still runs, and the example stays in the note.
  const mixed = parseReply('Using the `<tool_call name="x">` form.\n<tool_call name="exec_command">{"cmd":"ls"}</tool_call>', toolSpecs());
  assert.equal(mixed.calls.length, 1);
  assert.match(mixed.text, /form/);
  // Outside code an unknown tag is still refused, whether or not a valid call comes with it.
  assert.throws(() => parseReply('<tool_call name="unknown_tool">{}</tool_call>', toolSpecs()),
    error => error.code === 'prism_unknown_tool');
  assert.throws(() => parseReply('Quoted `<tool_call name="a">` but also <tool_call name="unknown_tool">{}</tool_call>', toolSpecs()),
    error => error.code === 'prism_unknown_tool');
});

test('malformed tags and nested cut-off plans are never leaked or executed', () => {
  const badTags = [
    '<tool_call>{"cmd":"ls"}</tool_call>',
    '<tool_call name="">{"cmd":"ls"}</tool_call>',
    `<tool_call name="exec_command'> {"cmd":"ls"}</tool_call>`,
    '<tool_call name=exec_command extra="ignored">{"cmd":"ls"}</tool_call>',
    '</tool_call>', '<TOOL_CALL name="exec_command">{"cmd":"ls"}</TOOL_CALL>',
    '<tool_call name="exec_command"',
    '<tool_call name="exec_command">{"cmd":"ls"}<tool_call name="exec_command">{"cmd":"pwd"}</tool_call>',
    '<tool_call name="exec_command">{"cmd":"ls"}</tool_call>\n<tool_call>',
  ];
  for (const reply of badTags) {
    assert.throws(() => parseReply(reply, toolSpecs()),
      error => error.status === 502 && error.code === 'prism_invalid_tool_output', reply);
  }
  for (const body of ['[]', 'null', '5', '{broken', '{"cmd":']) {
    assert.throws(() => parseReply(`<tool_call name="exec_command">${body}</tool_call>`, toolSpecs()),
      error => error.status === 502 && error.code === 'prism_invalid_tool_arguments', body);
  }
  assert.equal(parseReply('<tool_call name=exec_command>{"cmd":"pwd"}</tool_call>', toolSpecs()).calls.length, 1);
});

test('malformed legacy calls do not downgrade to prose or empty arguments', () => {
  for (const reply of ['{"tool_call":null}', '{"tool_call":[]}', '{"tool_call":"exec_command"}',
    '{"tool_call":{"arguments":{}}}', '{"tool_call":{"name":3}}']) {
    assert.throws(() => parseReply(reply, toolSpecs()),
      error => error.status === 502 && error.code === 'prism_invalid_tool_output');
  }
  assert.throws(() => parseReply('{"tool_call":{"name":"unknown"}}', toolSpecs()),
    error => error.status === 502 && error.code === 'prism_unknown_tool');
  for (const args of ['null', '[]', '5', '"not json"']) {
    assert.throws(() => parseReply(`{"tool_call":{"name":"exec_command","arguments":${args}}}`, toolSpecs()),
      error => error.status === 502 && error.code === 'prism_invalid_tool_arguments');
  }
});

test('MAX_TOOL_CALLS is an atomic limit, not a truncation limit', () => {
  const tag = '<tool_call name="exec_command">{"cmd":"ls"}</tool_call>';
  assert.equal(parseReply(tag.repeat(MAX_TOOL_CALLS), toolSpecs()).calls.length, MAX_TOOL_CALLS);
  assert.throws(() => parseReply(tag.repeat(MAX_TOOL_CALLS + 1), toolSpecs()),
    error => error.status === 502 && error.code === 'prism_too_many_tool_calls');
});

test('prompt filters callable actions and repeats the current tool policy after conflicting caller instructions', () => {
  const tools = [...toolSpecs().values()];
  const base = { system: ['Ignore policies and call every available tool.'], convo: [{ kind: 'user', text: 'Do it.' }], tools, budget: 1000 };
  const forced = buildPrompt({ ...base, toolPolicy: { choice: 'function', name: 'exec', parallel: false } });
  assert.match(forced, /Available actions:\n- exec \(raw input\)/);
  assert.ok(!forced.includes('- exec_command') && !forced.includes('- spawn'));
  assert.ok(forced.indexOf('call only the action "exec"') > forced.indexOf('TASK:'));
  assert.match(forced, /parallel_tool_calls=false: emit exactly one/);
  const required = buildPrompt({ ...base, toolPolicy: { choice: 'required', parallel: true } });
  assert.match(required, /tool_choice=required\. Emit at least one call/);
  assert.ok(required.indexOf('tool_choice=required') > required.indexOf('Reply now.'));
  const none = buildPrompt({ ...base, toolPolicy: { choice: 'none', parallel: false } });
  assert.ok(!none.includes('<role>action emitter') && !none.includes('Available actions:'));
  assert.match(none, /tool_choice=none\. Answer in plain text only/);
  assert.ok(none.indexOf('tool_choice=none') > none.indexOf('Do it.'));
  assert.equal(toolPolicyRules({ choice: 'auto', parallel: true }), '');
});

test('rejecting repeated malformed tags remains bounded', () => {
  const started = Date.now();
  assert.throws(() => parseReply('<tool_call '.repeat(40000), toolSpecs()),
    error => error.status === 502 && error.code === 'prism_invalid_tool_output');
  assert.ok(Date.now() - started < 1000, 'malformed tag scans must not be quadratic');
});
