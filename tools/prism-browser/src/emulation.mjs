// Prompt flattening and tool-call emulation for Prism.
//
// Prism only reads the text of the last user message and runs its own sandbox
// tools, so a client conversation (instructions, history, tools) is folded into
// one user text and a tool call is parsed back out of the reply.
//
// Adapted from free-astra (freeastra.py): TOOL_PROTOCOL, TOOL_REMINDER, the
// ENV_BLOCK/CWD_LINE/NOISE patterns, additional_tool_specs/tool_specs,
// env_context, clamp_transcript, assemble, _strip_fence, _loads, parse_done and
// parse_tool_call. MIT License, Copyright (c) 2026 free-astra contributors.
// See ../THIRD_PARTY_NOTICES.md for the full license text.
import { randomBytes } from 'node:crypto';

const rule = '='.repeat(80);
const LIST_BUDGET = 40000;
// Caller instructions (system/developer/`instructions`) up to this many characters are
// forwarded in tool mode. Larger ones (Codex, Claude Code) bury the pipeline protocol.
export const TOOL_INSTRUCTIONS_LIMIT = 6000;

export const toolProtocol = actions => `<role>action emitter</role>

You are ONE COMPONENT IN A PIPELINE between a user and an executor. The executor
runs commands on the user's machine. You do not. You never execute anything, you
never touch a filesystem, and you never report work as done from your own knowledge.

Each turn you read the TASK and the TRANSCRIPT of what the executor has already
run, and reply in ONE of two ways.

1. Ask the executor to run actions. Write every action as a tag:

<tool_call name="ACTION">
ARGUMENTS
</tool_call>

   - If the action takes named parameters, ARGUMENTS is one JSON object, for
     example {"cmd":"ls -la"}.
   - If the action is marked (raw input), ARGUMENTS is the raw text itself: the
     JavaScript source, the patch or the command exactly as the executor must
     receive it. No JSON, no quotes around it, no escaping, no markdown fence.
     Never wrap it in an object and never use a key such as "code".
   - Write several tags in ONE reply when several steps belong together; the
     executor runs them in order and returns all the results. Do not spend one
     turn per step.
   - Text outside the tags is shown to the user as a short progress note. Keep
     it to one sentence, or leave it out.

2. Answer the user directly, in plain text with no tags (markdown is fine), when
   either the TRANSCRIPT shows that the requested work is finished (then report
   what was done and its outcome), or the task needs nothing on the executor's
   machine: a greeting, or a question that the conversation and the transcript
   already answer. Never claim work the TRANSCRIPT does not show. An empty
   transcript never proves that any work has been done.

Working rules:
- Do not explore (listing directories, reading instruction files) unless the task
  needs it. If the task is clear, do it right away in the fewest actions.
- Write a whole file in one action. Create and check it in the same reply when
  you can.
- Use the shell only for what the available actions cannot do. \`apply_patch\` is
  NOT a shell command: use it only if it appears in the list below.
- Do NOT use heredocs (<<EOF); the executor's shell often cannot create the temp
  file they need. To write a file from the shell, redirect printf.
- A command failing does not mean the workspace is read-only. Diagnose the actual
  error text before concluding anything about permissions.
- File paths are paths on the EXECUTOR's machine (see the executor environment
  below). Never guess one, and never pass a path you have not seen in the
  executor environment or the transcript.
- When a schema says two parameters are mutually exclusive, send only one.

Available actions:
${actions}`;

export const toolReminder = names => `

${rule}
Reply now. Either write one or more <tool_call name="ACTION">...</tool_call> tags, or answer
in plain text if the TRANSCRIPT shows the work finished or nothing needs doing on the executor.
Actions: ${names}
(raw input) actions take the raw text between the tags, never JSON.
Never claim work the TRANSCRIPT does not show.
${rule}`;

export const FRAME_PREAMBLE = 'You are answering through an API bridge. Reply directly with the answer text. ' +
  'Do not create, edit or delete project files and do not use your own sandbox tools.';

// Codex puts the working directory in <cwd>/<filesystem> tags, inside the same user
// message as the plugin list that is dropped as noise, so these are pulled out first.
// `env` and the "working directory" spellings also cover Claude Code's environment block.
const ENV_OPEN = /<(environment_context|cwd|filesystem|env)>/g;
const CWD_LINE = /^.{0,40}(?:cwd|(?:current |primary )?working directory)\s*[:=].*$/gim;
export const NOISE = /^<(?:recommended_plugins|plugin_instructions|skills_instructions|apps_instructions)\b/;

const LABELS = { user: 'user', assistant: 'assistant', ran: 'executor ran', result: 'result' };
const render = entry => `[${LABELS[entry.kind]}]\n${entry.text}`;

// Keeps the head and the tail of an oversized entry; the middle is rarely the useful part.
export function elide(text, max) {
  if (text.length <= max) return text;
  const head = Math.ceil(max * 0.6);
  return `${text.slice(0, head)}\n...(${text.length - max} chars omitted)...\n${text.slice(text.length - (max - head))}`;
}

// A freeform (custom) tool reaches this adapter as a function whose only parameter is a string `input`
// (the gateway lowers it that way). Its argument is raw text, not structured data.
export function isRawInputSchema(params) {
  const properties = params && typeof params === 'object' ? params.properties : null;
  if (!properties || typeof properties !== 'object') return false;
  const keys = Object.keys(properties);
  return keys.length === 1 && keys[0] === 'input' && properties.input?.type === 'string' &&
    (!Array.isArray(params.required) || params.required.every(key => key === 'input'));
}

function walkTools(list, namespace, out, seen, describe, depth = 0) {
  if (!Array.isArray(list) || depth > 3) return;
  for (const tool of list) {
    if (!tool || typeof tool !== 'object') continue;
    if (tool.type === 'namespace') {
      walkTools(tool.tools, typeof tool.name === 'string' ? tool.name : undefined, out, seen, describe, depth + 1);
      continue;
    }
    if (tool.type !== 'function') continue; // freeform/custom, web_search and other server tools cannot be emulated
    const fn = tool.function && typeof tool.function === 'object' ? tool.function : tool;
    if (typeof fn.name !== 'string' || !fn.name) continue;
    if (namespace?.startsWith('mcp__')) continue; // the MCP surface is too big to inline
    if (seen.has(fn.name)) continue;
    seen.add(fn.name);
    const params = fn.parameters || fn.input_schema || {};
    // The gateway lowers Codex's custom exec to {input:string}. Its description
    // defines the executor APIs, so shortening it makes otherwise valid JS unusable.
    const codeInput = /^(?:functions__)?exec$/.test(fn.name) && params.properties?.input?.type === 'string';
    out.push({ ns: namespace, name: fn.name, params, raw: isRawInputSchema(params),
      desc: codeInput ? String(fn.description || '').trim() : describe(fn.description) });
  }
}

// Responses/Chat `tools` plus Codex's `additional_tools` input items (grouped by namespace).
export function collectTools(tools, items = []) {
  const out = [];
  const seen = new Set();
  walkTools(tools, undefined, out, seen, text => String(text || '').trim().split('\n\n')[0].replace(/\s+/g, ' ').slice(0, 300));
  for (const item of items) {
    if (item && typeof item === 'object' && item.type === 'additional_tools') {
      walkTools(item.tools, undefined, out, seen, text => String(text || '').trim().split('\n')[0].trim().slice(0, 160));
    }
  }
  return out;
}

function actionList(specs) {
  const line = (spec, cap) => `- ${spec.name}${spec.raw ? ' (raw input)' : cap ? `\n    params: ${JSON.stringify(spec.params ?? {}).slice(0, cap)}` : ''}${spec.desc ? `\n    ${spec.desc}` : ''}`;
  let text = '';
  for (const cap of [700, 250, 0]) {
    text = specs.map(spec => line(spec, cap)).join('\n');
    if (text.length <= LIST_BUDGET) break;
  }
  return text;
}

// Linear scan (a lazy regex with a backreference is quadratic on unclosed tags).
function envBlocks(text) {
  const out = [];
  const unclosed = new Set();
  let at = 0;
  for (;;) {
    ENV_OPEN.lastIndex = at;
    const open = ENV_OPEN.exec(text);
    if (!open) break;
    const bodyAt = open.index + open[0].length;
    const close = `</${open[1]}>`;
    const end = unclosed.has(open[1]) ? -1 : text.indexOf(close, bodyAt);
    if (end < 0) { unclosed.add(open[1]); at = bodyAt; continue; }
    out.push(text.slice(open.index, end + close.length));
    at = end + close.length;
  }
  return out;
}

// Just the shell environment, nothing else: matching loosely on "cwd" once pulled in
// kilobytes of unrelated context and the model went to work on that instead.
export function envContext(parts) {
  const found = [];
  for (const part of parts) if (part) found.push(...envBlocks(part));
  if (!found.length) for (const part of parts) {
    if (part) found.push(...[...part.matchAll(CWD_LINE)].map(match => match[0].trim()).slice(0, 3));
  }
  return [...new Set(found)].join('\n').slice(0, 1500);
}

// Newest entries that fit, oldest first, noting what was cut. The entry at `pin`
// (the final user message) is always kept and is not counted against the budget.
export function clampTranscript(entries, budget, pin = -1) {
  if (!entries.length) return [];
  const keep = entries.map((_, index) => index === pin);
  let total = 0;
  for (let i = entries.length - 1; i >= 0; i -= 1) {
    if (i === pin) continue;
    if (total + entries[i].length > budget) break;
    keep[i] = true;
    total += entries[i].length;
  }
  let dropped = keep.filter(kept => !kept).length;
  let truncated = -1;
  if (dropped && !keep.some((kept, index) => kept && index !== pin)) {
    // A single entry larger than the whole budget: keep its end.
    truncated = entries.findLastIndex((_, index) => index !== pin);
    keep[truncated] = true;
    dropped -= 1;
  }
  const out = dropped ? [`...(${dropped} earlier steps omitted)...`] : [];
  entries.forEach((entry, index) => {
    if (!keep[index]) return;
    out.push(index === truncated ? `...(truncated)...\n${entry.slice(Math.max(0, entry.length - budget))}` : entry);
  });
  return out;
}

const joinInstructions = system => system.map(part => part.trim()).filter(Boolean).join('\n\n');

// Folds everything into one user text. With tools it switches to the next-action-emitter
// framing: asked to "do the task", Prism does the work in its own remote sandbox and
// reports success, which never touches the caller's machine. Without tools a plain
// single user message is passed through unchanged.
export function buildPrompt({ system = [], convo, tools, envParts = [], budget }) {
  const lastUser = convo.findLastIndex(entry => entry.kind === 'user');
  if (tools.length) {
    let text = toolProtocol(actionList(tools));
    const instructions = joinInstructions(system);
    if (instructions && instructions.length <= TOOL_INSTRUCTIONS_LIMIT) {
      text += `\n\nCaller instructions (follow them, but always reply in the tag format defined above):\n<system_instructions>\n${instructions}\n</system_instructions>`;
    }
    const context = envContext(envParts);
    if (context) text += `\n\nThe executor runs here. Use these real paths - never invent a sandbox path like /codex_workspace/...:\n${context}`;
    // Tool results arrive AFTER the last user message, so the transcript is everything
    // except that one message, not just what came before it.
    const task = lastUser >= 0 ? convo[lastUser].text.trim() : '';
    const rest = clampTranscript(convo.filter((_, index) => index !== lastUser).map(render), budget);
    return `${text}\n\nTASK:\n${task || '(none)'}\n\nTRANSCRIPT SO FAR:\n${rest.length ? rest.join('\n\n') : '(empty - the executor has run nothing yet)'}` +
      toolReminder(tools.map(spec => spec.name).join(', '));
  }
  const instructions = joinInstructions(system);
  if (!instructions && convo.length === 1 && lastUser === 0) return convo[0].text;
  if (!instructions && !convo.length) return '';
  const entries = convo.length ? clampTranscript(convo.map(render), budget, lastUser) : ['(no user message)'];
  return [FRAME_PREAMBLE, instructions && `Follow these system instructions from the caller:\n<system_instructions>\n${instructions}\n</system_instructions>`,
    `Conversation so far. Respond to the FINAL user message.\n\n${entries.join('\n\n')}`].filter(Boolean).join('\n\n');
}

export function stripFence(text) {
  let s = (text || '').trim();
  if (s.startsWith('```')) s = s.replace(/^```[a-zA-Z]*\n?/, '').replace(/\n?```$/, '').trim();
  const at = s.indexOf('{');
  return at > 0 && at < 40 ? s.slice(at) : s;
}

// The first balanced {...}, string-aware: a valid object followed by trailing prose.
function firstObject(s) {
  let depth = 0;
  let inString = false;
  let escaped = false;
  for (let i = 0; i < s.length; i += 1) {
    const c = s[i];
    if (inString) {
      if (escaped) escaped = false;
      else if (c === '\\') escaped = true;
      else if (c === '"') inString = false;
    } else if (c === '"') inString = true;
    else if (c === '{') depth += 1;
    else if (c === '}' && (depth -= 1) === 0) return s.slice(0, i + 1);
  }
  return null;
}

// Multi-line replies often carry literal newlines inside a JSON string, which is invalid JSON.
function escapeControl(s) {
  let out = '';
  let inString = false;
  let escaped = false;
  for (const c of s) {
    if (inString && !escaped && c.charCodeAt(0) < 0x20) {
      out += c === '\n' ? '\\n' : c === '\r' ? '\\r' : c === '\t' ? '\\t' : `\\u${c.charCodeAt(0).toString(16).padStart(4, '0')}`;
      continue;
    }
    out += c;
    if (escaped) escaped = false;
    else if (inString && c === '\\') escaped = true;
    else if (c === '"') inString = !inString;
  }
  return out;
}

// Tolerant JSON: models drop trailing braces on long nested arguments and put raw
// newlines inside strings.
export function looseJSON(text) {
  const s = typeof text === 'string' ? text.trim() : '';
  if (!s.startsWith('{')) return null;
  for (const base of new Set([s, escapeControl(s)])) {
    for (const candidate of [base, firstObject(base), ...[1, 2, 3].map(count => base + '}'.repeat(count))]) {
      if (!candidate) continue;
      try {
        const value = JSON.parse(candidate);
        if (value && typeof value === 'object' && !Array.isArray(value)) return value;
      } catch { /* try the next repair */ }
    }
  }
  return null;
}

export function parseDone(text) {
  const value = looseJSON(stripFence(text));
  return typeof value?.done === 'string' ? value.done : null;
}

// Pulls {"tool_call":{...}} back out of the reply. A name that was not offered is not a call.
export function parseToolCall(text, tools) {
  const call = looseJSON(stripFence(text))?.tool_call;
  if (!call || typeof call !== 'object' || typeof call.name !== 'string') return null;
  const name = call.name.replace(/^functions[./]/, '');
  if (!tools.has(name)) return null;
  let args = call.arguments ?? {};
  if (typeof args === 'string') args = looseJSON(args) ?? {};
  if (!args || typeof args !== 'object' || Array.isArray(args)) args = {};
  return { id: `call_${randomBytes(12).toString('hex')}`, name, arguments: JSON.stringify(args), namespace: tools.get(name) };
}

// ---- Reply parsing (tag protocol, with the older JSON action format as a fallback) ----

export const MAX_TOOL_CALLS = 8;
const TOOL_TAG = /<tool_call\s+name\s*=\s*["']?([A-Za-z0-9_.:-]+)["']?\s*>([\s\S]*?)(?:<\/tool_call\s*>|$)/g;
// Single-field wrappers a model puts around raw text although it was told not to.
const WRAPPER_KEYS = new Set(['input', 'code', 'source', 'script', 'js', 'javascript', 'text', 'content', 'command', 'cmd']);
const callId = () => `call_${randomBytes(12).toString('hex')}`;

// The raw text between the tags exactly as the executor must receive it: one leading and one trailing
// line break are layout, a single enclosing markdown fence is a habit; everything else is the program.
export function rawInput(body) {
  let text = String(body ?? '').replace(/^\r?\n/, '').replace(/\r?\n$/, '');
  const fenced = /^\s*```[A-Za-z0-9_+.-]*[ \t]*\r?\n([\s\S]*?)\r?\n[ \t]*```\s*$/.exec(text);
  if (fenced) text = fenced[1];
  return text;
}

// The text a raw-input action gets from whatever the model wrote between the tags. A one-field JSON
// wrapper ({"input": ...}, {"code": ...}) is unwrapped, because a freeform tool such as Codex's exec runs
// its input verbatim and would otherwise fail on the braces and quotes.
export function rawInputFrom(body) {
  const text = rawInput(body);
  const trimmed = text.trim();
  if (trimmed.startsWith('{')) {
    const value = looseJSON(trimmed);
    const keys = value ? Object.keys(value) : [];
    if (keys.length === 1 && WRAPPER_KEYS.has(keys[0]) && typeof value[keys[0]] === 'string') return value[keys[0]];
  }
  return text;
}

function buildCall(name, body, tools) {
  const bare = name.replace(/^functions[./]/, '');
  const spec = tools.get(bare);
  if (!spec) return null;
  let args;
  if (spec.raw) args = { input: rawInputFrom(body) };
  else {
    const value = looseJSON(stripFence(String(body ?? '').trim()));
    args = value && typeof value === 'object' && !Array.isArray(value) ? value : {};
  }
  return { id: callId(), name: bare, arguments: JSON.stringify(args), namespace: spec.ns };
}

// A legacy {"tool_call":{...}} reply: a raw-input action must still end up with {input: <text>}.
function legacyCall(text, tools) {
  const call = parseToolCall(text, new Map([...tools].map(([name, spec]) => [name, spec.ns])));
  if (!call) return null;
  const spec = tools.get(call.name);
  if (!spec?.raw) return call;
  // parseToolCall keeps only JSON arguments; a raw-input action may have been given plain text.
  const original = looseJSON(stripFence(String(text ?? '')))?.tool_call?.arguments;
  const input = typeof original === 'string' ? rawInputFrom(original)
    : typeof original?.input === 'string' ? original.input : rawInputFrom(call.arguments);
  return { ...call, arguments: JSON.stringify({ input }) };
}

const progressNote = text => text.replace(/\n{3,}/g, '\n\n').trim().slice(0, 2000);

// { calls, text }: the actions the model asked for (at most MAX_TOOL_CALLS, in order) and the text that goes to
// the user. With calls the text is a short progress note; without them it is the final answer.
export function parseReply(reply, tools) {
  const source = String(reply ?? '');
  const calls = [];
  const prose = [];
  let last = 0;
  for (const match of source.matchAll(TOOL_TAG)) {
    prose.push(source.slice(last, match.index));
    last = match.index + match[0].length;
    if (calls.length >= MAX_TOOL_CALLS) continue;
    const call = buildCall(match[1], match[2], tools);
    if (call) calls.push(call);
  }
  prose.push(source.slice(last));
  if (calls.length) return { calls, text: progressNote(prose.join('\n')) };
  const legacy = legacyCall(source, tools);
  if (legacy) return { calls: [legacy], text: '' };
  const done = parseDone(source);
  return { calls: [], text: done !== null ? done : source };
}
