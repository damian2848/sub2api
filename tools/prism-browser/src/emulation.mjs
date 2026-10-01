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

export const toolProtocol = actions => `<role>next-action emitter</role>

You are ONE COMPONENT IN A PIPELINE. A separate executor process runs commands on
the user's machine. You do not. You never execute anything, you never touch a
filesystem, and you never report work as done from your own knowledge.

Your entire contract: read the task plus the transcript of what the executor has
already run, then reply with ONE JSON object. Either it asks the executor to run
an action, which appends the result to the transcript and asks you again, or it
carries your final reply to the user. Text outside the JSON object breaks the
pipeline and is discarded, so never add commentary around it and never use
markdown fences.

Available actions:
${actions}

Shell guidance:
- \`apply_patch\` is NOT a shell command. Never pipe into it, never call it from a
  shell. Use it only if it appears as an action in the list above.
- Do NOT use heredocs (<<EOF). The executor's shell often cannot create the temp
  file they need.
- To write a file, redirect printf:
    printf '%s\\n' 'first line' 'second line' > path/to/file
- To read a file use \`cat\`, to search use \`rg\`. Verify with \`cat\` after writing.
- A command failing does not mean the workspace is read-only. Diagnose the actual
  error text before concluding anything about permissions.
- File paths in arguments are paths on the EXECUTOR's machine (see the executor
  environment below). Never guess one, and never pass a path you have not seen in
  the executor environment or the transcript.
- When a schema says two parameters are mutually exclusive, send only one.

Emit exactly one of:
  {"tool_call":{"name":"<action>","arguments":{...}}}
      <- when something must still be run or looked up on the executor's machine
  {"done":"<complete final reply to the user>"}

The "done" value is the COMPLETE reply the user will read: as long as it needs to
be, markdown allowed, newlines written as \\n inside the JSON string. It is not a
one-line status. Use "done" only when either
  - the TRANSCRIPT shows that the requested work is finished (then report what was
    done and its outcome), or
  - the task needs no action on the executor's machine: a greeting, or a question
    that can be answered from the conversation and the transcript alone.
Never claim work the TRANSCRIPT does not show. An empty transcript never proves
that any work has been done.`;

export const toolReminder = names => `

${rule}
Reply with ONE JSON object now. Actions: ${names}
{"tool_call":{"name":"...","arguments":{...}}}  or  {"done":"<complete reply to the user>"}
"done" is the complete final reply (markdown allowed, \\n for newlines), only when the
TRANSCRIPT shows the work finished or nothing needs doing on the executor's machine.
Never claim work the TRANSCRIPT does not show. Nothing outside the JSON. Start with { .
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
    out.push({ ns: namespace, name: fn.name, params,
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
  const line = (spec, cap) => `- ${spec.name}${cap ? `\n    params: ${JSON.stringify(spec.params ?? {}).slice(0, cap)}` : ''}${spec.desc ? `\n    ${spec.desc}` : ''}`;
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
      text += `\n\nCaller instructions (follow them, but always reply in the JSON action format defined above):\n<system_instructions>\n${instructions}\n</system_instructions>`;
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
