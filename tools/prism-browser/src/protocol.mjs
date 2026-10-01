// Request normalisation, output shaping and SSE framing for the Prism bridge.
//
// flatten/flatten_responses (conversation folding) and the function_call /
// tool_calls output shapes are adapted from free-astra (freeastra.py).
// MIT License, Copyright (c) 2026 free-astra contributors.
// See ../THIRD_PARTY_NOTICES.md for the full license text.
import { randomBytes, randomUUID } from 'node:crypto';
import { PrismError } from './errors.mjs';
import { NOISE, buildPrompt, collectTools, elide, parseDone, parseToolCall } from './emulation.mjs';

const DEFAULT_MAX_TEXT_BYTES = 256 * 1024;
const RESULT_CHARS = 8000;
const CALL_CHARS = 4000;
const TEXT_PARTS = new Set(['text', 'input_text', 'output_text']);
const MEDIA_PARTS = new Set(['input_image', 'image_url', 'input_file', 'input_audio', 'file']);
// Server-side tool traces and reasoning carry nothing the emulated conversation needs.
const SKIPPED_ITEMS = new Set(['reasoning', 'web_search_call', 'file_search_call', 'code_interpreter_call',
  'image_generation_call', 'compaction']);
const efforts = new Map([['none', 'low'], ['minimal', 'low'], ['low', 'low'], ['medium', 'medium'], ['high', 'high'], ['xhigh', 'high']]);

function invalid(code, param) { throw new PrismError(code, 400, param); }
function object(value, param) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) invalid('expected_object', param);
}
function envInt(name, fallback) {
  const value = Number(process.env[name]);
  return Number.isInteger(value) && value > 0 ? value : fallback;
}

export const mapEffort = value => efforts.get(typeof value === 'string' ? value.toLowerCase() : '') ?? 'medium';

function partText(part, param) {
  if (typeof part === 'string') return part;
  if (!part || typeof part !== 'object') invalid('unsupported_content_type', param);
  if (MEDIA_PARTS.has(part.type)) invalid('image_input_not_supported', param);
  if (TEXT_PARTS.has(part.type) && typeof part.text === 'string') return part.text;
  if (part.type === 'refusal' && typeof part.refusal === 'string') return part.refusal;
  return invalid('unsupported_content_type', param);
}

function contentText(content, param) {
  if (content === null || content === undefined) return '';
  if (typeof content === 'string') return content;
  return (Array.isArray(content) ? content : [content]).map((part, index) => partText(part, `${param}.${index}`)).join('');
}

// Tool output: a string, text parts, or arbitrary JSON (serialised as is).
function outputText(output, param) {
  if (output === null || output === undefined) return '';
  if (typeof output === 'string') return output;
  const parts = Array.isArray(output) ? output : [output];
  const typed = value => value && typeof value === 'object';
  if (parts.some(part => typed(part) && MEDIA_PARTS.has(part.type))) invalid('image_input_not_supported', param);
  if (parts.every(part => typed(part) && TEXT_PARTS.has(part.type) && typeof part.text === 'string')) return parts.map(part => part.text).join('');
  return JSON.stringify(output);
}

const callText = (name, args) => elide(`${name} ${typeof args === 'string' ? args : JSON.stringify(args ?? {})}`, CALL_CHARS);

function addMessage(state, role, text, param) {
  if (role === 'system' || role === 'developer') { state.system.push(text); state.env.push(text); }
  else if (role === 'user') {
    state.env.push(text);
    // Codex scaffolding (plugin/skill lists), not something the user typed.
    if (text.trim() && !NOISE.test(text.trimStart())) state.convo.push({ kind: 'user', text });
  } else if (role === 'assistant') { if (text.trim()) state.convo.push({ kind: 'assistant', text }); }
  else invalid('unsupported_message_role', `${param}.role`);
}

function chatConversation(body) {
  if (!Array.isArray(body.messages) || !body.messages.length) invalid('messages_required', 'messages');
  const state = { system: [], convo: [], env: [], items: [] };
  body.messages.forEach((message, index) => {
    const param = `messages.${index}`;
    object(message, param);
    const text = contentText(message.content, `${param}.content`);
    if (message.role === 'tool' || message.role === 'function') {
      state.convo.push({ kind: 'result', text: elide(text, RESULT_CHARS) });
    } else if (message.role === 'assistant') {
      addMessage(state, 'assistant', text, param);
      const calls = Array.isArray(message.tool_calls) ? message.tool_calls : message.function_call ? [{ function: message.function_call }] : [];
      for (const call of calls) {
        if (call?.function && typeof call.function.name === 'string') {
          state.convo.push({ kind: 'ran', text: callText(call.function.name, call.function.arguments) });
        }
      }
    } else addMessage(state, message.role, text, param);
  });
  return state;
}

function responsesConversation(body) {
  const state = { system: [], convo: [], env: [], items: [] };
  if (typeof body.instructions === 'string' && body.instructions.trim()) {
    state.system.push(body.instructions);
    state.env.push(body.instructions);
  }
  // Generic clients send `input` as one string or one object; Codex sends a list.
  const items = typeof body.input === 'string' || (body.input && typeof body.input === 'object' && !Array.isArray(body.input))
    ? [body.input] : body.input;
  if (!Array.isArray(items) || !items.length) invalid('input_required', 'input');
  state.items = items;
  items.forEach((item, index) => {
    const param = `input.${index}`;
    if (typeof item === 'string') return addMessage(state, 'user', item, param);
    object(item, param);
    const type = item.type ?? 'message';
    if (type === 'message') return addMessage(state, item.role ?? 'user', contentText(item.content, `${param}.content`), param);
    if (type === 'function_call') {
      state.convo.push({ kind: 'ran', text: callText(item.name ?? '', item.arguments) });
    } else if (type === 'custom_tool_call') {
      state.convo.push({ kind: 'ran', text: callText(item.name ?? '', item.input ?? '') });
    } else if (type === 'function_call_output' || type === 'custom_tool_call_output') {
      state.convo.push({ kind: 'result', text: elide(outputText(item.output, `${param}.output`), RESULT_CHARS) });
    } else if (type === 'item_reference') invalid('item_reference_not_supported', param);
    else if (type !== 'additional_tools' && !SKIPPED_ITEMS.has(type)) invalid('unsupported_input_item', param);
  });
  return state;
}

// `limits` is `{ maxTextBytes, maxTranscriptChars }`; a bare number is maxTextBytes.
export function parseRequest(body, family, models, limits = {}) {
  const { maxTextBytes = DEFAULT_MAX_TEXT_BYTES, maxTranscriptChars = envInt('PRISM_MAX_TRANSCRIPT_CHARS', 32000) } =
    typeof limits === 'number' ? { maxTextBytes: limits } : limits;
  object(body, 'body');
  if (typeof body.model !== 'string' || !models.includes(body.model)) invalid('model_not_available', 'model');
  if (body.stream !== undefined && body.stream !== null && typeof body.stream !== 'boolean') invalid('expected_boolean', 'stream');
  // Prism keeps no server-side conversation, so there is nothing to continue from.
  if (body.previous_response_id !== undefined && body.previous_response_id !== null) {
    invalid('previous_response_not_supported', 'previous_response_id');
  }
  // Every other parameter (sampling, tool_choice, include, store, ...) is ignored, never rejected.
  const chat = family === 'chat';
  const param = chat ? 'messages' : 'input';
  const state = chat ? chatConversation(body) : responsesConversation(body);
  if (!state.convo.length && !state.system.length) invalid('empty_input', param);
  const specs = collectTools(body.tools, state.items);
  const text = buildPrompt({ system: state.system, convo: state.convo, tools: specs, envParts: state.env,
    budget: maxTranscriptChars });
  if (!text.trim()) invalid('empty_input', param);
  if (Buffer.byteLength(text) > maxTextBytes) invalid('input_too_large', param);
  return { family, model: body.model, input: [{ type: 'message', role: 'user', content: [{ type: 'input_text', text }] }],
    effort: mapEffort(chat ? body.reasoning_effort ?? body.reasoning?.effort : body.reasoning?.effort ?? body.reasoning_effort),
    stream: body.stream === true, includeUsage: body.stream_options?.include_usage === true,
    tools: specs.length ? new Map(specs.map(spec => [spec.name, spec.ns])) : null };
}

export function estimatedUsage(input, text) {
  const inputTokens = Math.ceil(input.reduce((sum, item) => sum + item.content[0].text.length, 0) / 3);
  const outputTokens = Math.ceil(text.length / 3);
  return { input_tokens: inputTokens, output_tokens: outputTokens, total_tokens: inputTokens + outputTokens,
    input_tokens_details: { cached_tokens: 0 }, output_tokens_details: { reasoning_tokens: 0 },
    estimation: 'character_based_estimate' };
}

export function resultBody(request, text) {
  let call = null;
  if (request.tools) {
    call = parseToolCall(text, request.tools);
    if (!call) {
      const done = parseDone(text);
      if (done !== null) text = done;
    }
  }
  const usage = estimatedUsage(request.input, call ? `${call.name} ${call.arguments}` : text);
  const created = Math.floor(Date.now() / 1000);
  if (request.family === 'chat') {
    const message = call ? { role: 'assistant', content: null, tool_calls: [{ id: call.id, type: 'function',
      function: { name: call.name, arguments: call.arguments } }] } : { role: 'assistant', content: text };
    return { id: `chatcmpl-${randomUUID()}`, object: 'chat.completion', created, model: request.model,
      choices: [{ index: 0, message, finish_reason: call ? 'tool_calls' : 'stop' }],
      usage: { prompt_tokens: usage.input_tokens, completion_tokens: usage.output_tokens,
        total_tokens: usage.total_tokens, estimation: usage.estimation } };
  }
  const item = call ? { id: `fc_${randomBytes(12).toString('hex')}`, type: 'function_call', call_id: call.id, name: call.name,
    arguments: call.arguments, status: 'completed', ...(call.namespace && call.namespace !== 'functions' ? { namespace: call.namespace } : {}) }
    : { id: `msg_${randomUUID()}`, type: 'message', role: 'assistant', status: 'completed',
      content: [{ type: 'output_text', text, annotations: [] }] };
  return { id: `resp_${randomUUID()}`, object: 'response', created_at: created, status: 'completed', model: request.model,
    output: [item], output_text: call ? '' : text, usage };
}

// Fixed-size slices that never split a surrogate pair.
function* slices(text, size = 600) {
  for (let at = 0; at < text.length;) {
    let end = Math.min(at + size, text.length);
    const last = text.charCodeAt(end - 1);
    if (end < text.length && end - 1 > at && last >= 0xd800 && last <= 0xdbff) end -= 1;
    yield text.slice(at, end);
    at = end;
  }
}

export function writeCompletedStream(res, request, result) {
  if (request.family === 'chat') {
    const choice = result.choices[0];
    const chunk = (delta, finishReason = null) => res.write(`data: ${JSON.stringify({
      id: result.id, object: 'chat.completion.chunk', created: result.created, model: result.model,
      choices: [{ index: 0, delta, finish_reason: finishReason }] })}\n\n`);
    chunk({ role: 'assistant', content: '' });
    if (choice.message.tool_calls) chunk({ tool_calls: choice.message.tool_calls.map((call, index) => ({ index, ...call })) });
    else for (const piece of slices(choice.message.content)) chunk({ content: piece });
    chunk({}, choice.finish_reason);
    if (request.includeUsage) res.write(`data: ${JSON.stringify({ id: result.id, object: 'chat.completion.chunk',
      created: result.created, model: result.model, choices: [], usage: result.usage })}\n\n`);
    res.write('data: [DONE]\n\n');
  } else {
    let sequence = 0;
    const event = (type, data) => res.write(`event: ${type}\ndata: ${JSON.stringify({ type,
      sequence_number: sequence++, ...data })}\n\n`);
    const item = result.output[0];
    const shell = { ...result, status: 'in_progress', output: [], output_text: '', usage: null };
    event('response.created', { response: shell });
    event('response.in_progress', { response: shell });
    if (item.type === 'function_call') {
      event('response.output_item.added', { output_index: 0, item: { ...item, status: 'in_progress', arguments: '' } });
      for (const delta of slices(item.arguments)) {
        event('response.function_call_arguments.delta', { item_id: item.id, output_index: 0, delta });
      }
      event('response.function_call_arguments.done', { item_id: item.id, output_index: 0, name: item.name,
        arguments: item.arguments });
      event('response.output_item.done', { output_index: 0, item });
    } else {
      const part = item.content[0];
      event('response.output_item.added', { output_index: 0, item: { ...item, status: 'in_progress', content: [] } });
      event('response.content_part.added', { item_id: item.id, output_index: 0, content_index: 0,
        part: { ...part, text: '' } });
      for (const delta of slices(part.text)) {
        event('response.output_text.delta', { item_id: item.id, output_index: 0, content_index: 0, delta });
      }
      event('response.output_text.done', { item_id: item.id, output_index: 0, content_index: 0, text: part.text });
      event('response.content_part.done', { item_id: item.id, output_index: 0, content_index: 0, part });
      event('response.output_item.done', { output_index: 0, item });
    }
    event('response.completed', { response: result });
  }
  res.end();
}
