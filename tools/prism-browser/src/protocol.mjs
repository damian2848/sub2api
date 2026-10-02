// Request normalisation, output shaping and SSE framing for the Prism bridge.
//
// flatten/flatten_responses (conversation folding) and the function_call /
// tool_calls output shapes are adapted from free-astra (freeastra.py).
// MIT License, Copyright (c) 2026 free-astra contributors.
// See ../THIRD_PARTY_NOTICES.md for the full license text.
import { randomBytes, randomUUID } from 'node:crypto';
import { PrismError, publicError } from './errors.mjs';
import { NOISE, buildPrompt, collectTools, elide, parseReply } from './emulation.mjs';

const DEFAULT_MAX_TEXT_BYTES = 256 * 1024;
const RESULT_CHARS = 8000;
const CALL_CHARS = 4000;
const TEXT_PARTS = new Set(['text', 'input_text', 'output_text']);
const MEDIA_PARTS = new Set(['input_image', 'image_url', 'input_file', 'input_audio', 'file']);
// Server-side tool traces and reasoning carry nothing the emulated conversation needs.
const SKIPPED_ITEMS = new Set(['reasoning', 'web_search_call', 'file_search_call', 'code_interpreter_call',
  'image_generation_call', 'compaction']);
// Prism's UI offers four reasoning efforts for every model: low, medium, high and xhigh ("Extra High").
// Levels below low collapse to low and the levels above xhigh (`max`, and Codex's `ultra`, which adds
// task delegation on the client side) to the highest one Prism has; nothing is lowered.
const efforts = new Map([['none', 'low'], ['minimal', 'low'], ['low', 'low'], ['medium', 'medium'], ['high', 'high'],
  ['xhigh', 'xhigh'], ['extrahigh', 'xhigh'], ['extra-high', 'xhigh'], ['extra_high', 'xhigh'], ['max', 'xhigh'], ['ultra', 'xhigh']]);

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
    tools: specs.length ? new Map(specs.map(spec => [spec.name, spec.ns])) : null,
    toolSpecs: specs.length ? new Map(specs.map(spec => [spec.name, spec])) : null };
}

// input_tokens is the whole prompt; cached_tokens is the estimated share of it read from the prompt
// cache (see prompt-cache.mjs), never more than the prompt itself.
export function estimatedUsage(input, text, cachedTokens = 0) {
  const inputTokens = Math.ceil(input.reduce((sum, item) => sum + item.content[0].text.length, 0) / 3);
  const outputTokens = Math.ceil(text.length / 3);
  const cached = Math.max(0, Math.min(Number.isInteger(cachedTokens) ? cachedTokens : 0, inputTokens));
  return { input_tokens: inputTokens, output_tokens: outputTokens, total_tokens: inputTokens + outputTokens,
    input_tokens_details: { cached_tokens: cached }, output_tokens_details: { reasoning_tokens: 0 },
    estimation: 'character_based_estimate' };
}

export function resultIdentity(request) {
  return { id: request.family === 'chat' ? `chatcmpl-${randomUUID()}` : `resp_${randomUUID()}`,
    created: Math.floor(Date.now() / 1000) };
}

export function resultBody(request, text, identity = resultIdentity(request), { cachedTokens = 0 } = {}) {
  // In tool mode the reply is parsed into the actions asked for and the text for the user.
  const reply = request.toolSpecs ? parseReply(text, request.toolSpecs) : { calls: [], text };
  const { calls } = reply;
  const message = reply.text;
  const usage = estimatedUsage(request.input, `${message} ${calls.map(call => `${call.name} ${call.arguments}`).join(' ')}`.trim(),
    cachedTokens);
  if (request.family === 'chat') {
    const assistant = { role: 'assistant', content: message || (calls.length ? null : message) };
    if (calls.length) assistant.tool_calls = calls.map(call => ({ id: call.id, type: 'function',
      function: { name: call.name, arguments: call.arguments } }));
    return { id: identity.id, object: 'chat.completion', created: identity.created, model: request.model,
      choices: [{ index: 0, message: assistant, finish_reason: calls.length ? 'tool_calls' : 'stop' }],
      usage: { prompt_tokens: usage.input_tokens, completion_tokens: usage.output_tokens,
        total_tokens: usage.total_tokens, prompt_tokens_details: { cached_tokens: usage.input_tokens_details.cached_tokens },
        estimation: usage.estimation } };
  }
  const output = [];
  // `phase` is the channel Codex uses to fold progress notes under "worked for ..." and to show the
  // final answer: a note that comes with tool calls is commentary, a reply without calls is the answer.
  if (message || !calls.length) output.push({ id: `msg_${randomUUID()}`, type: 'message', role: 'assistant',
    status: 'completed', phase: calls.length ? 'commentary' : 'final_answer',
    content: [{ type: 'output_text', text: message, annotations: [] }] });
  for (const call of calls) output.push({ id: `fc_${randomBytes(12).toString('hex')}`, type: 'function_call',
    call_id: call.id, name: call.name, arguments: call.arguments, status: 'completed',
    ...(call.namespace && call.namespace !== 'functions' ? { namespace: call.namespace } : {}) });
  return { id: identity.id, object: 'response', created_at: identity.created, status: 'completed', model: request.model,
    output, output_text: message, usage };
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

function streamFrames(request, identity) {
  let sequence = 0;
  // Allocate sequences at write time so cancelled frames cannot leave gaps.
  const event = (type, data) => () => `event: ${type}\ndata: ${JSON.stringify({ type,
    sequence_number: sequence++, ...data })}\n\n`;
  const chunk = (delta, finishReason = null) => `data: ${JSON.stringify({
    id: identity.id, object: 'chat.completion.chunk', created: identity.created, model: request.model,
    choices: [{ index: 0, delta, finish_reason: finishReason }] })}\n\n`;
  const shell = request.family === 'chat' ? null : { id: identity.id, object: 'response', created_at: identity.created,
    status: 'in_progress', model: request.model, output: [], output_text: '', usage: null };
  return {
    *begin() {
      if (request.family === 'chat') {
        yield chunk({ role: 'assistant', content: '' });
        return;
      }
      yield event('response.created', { response: shell });
      yield event('response.in_progress', { response: shell });
    },
    // Sent while Prism is still generating. A comment keeps proxies open, but clients such as Codex only
    // count real events towards their stream idle timeout (5 minutes by default): after that they drop the
    // connection and resubmit the whole request, which starts a second generation. A repeated
    // response.in_progress carries no content and is ignored by clients and by the gateway's converters.
    *heartbeat() {
      yield ': waiting for Prism\n\n';
      if (request.family !== 'chat') yield event('response.in_progress', { response: shell });
    },
    *finish(result) {
      if (request.family === 'chat') {
        const choice = result.choices[0];
        if (choice.message.content) for (const piece of slices(choice.message.content)) yield chunk({ content: piece });
        if (choice.message.tool_calls) yield chunk({ tool_calls: choice.message.tool_calls.map((call, index) => ({ index, ...call })) });
        yield chunk({}, choice.finish_reason);
        if (request.includeUsage) yield `data: ${JSON.stringify({ id: result.id, object: 'chat.completion.chunk',
          created: result.created, model: result.model, choices: [], usage: result.usage })}\n\n`;
        yield 'data: [DONE]\n\n';
        return;
      }
      for (const [outputIndex, item] of result.output.entries()) {
        if (item.type === 'function_call') {
          yield event('response.output_item.added', { output_index: outputIndex, item: { ...item, status: 'in_progress', arguments: '' } });
          for (const delta of slices(item.arguments)) {
            yield event('response.function_call_arguments.delta', { item_id: item.id, output_index: outputIndex, delta });
          }
          yield event('response.function_call_arguments.done', { item_id: item.id, output_index: outputIndex, name: item.name,
            arguments: item.arguments });
          yield event('response.output_item.done', { output_index: outputIndex, item });
        } else {
          const part = item.content[0];
          yield event('response.output_item.added', { output_index: outputIndex, item: { ...item, status: 'in_progress', content: [] } });
          yield event('response.content_part.added', { item_id: item.id, output_index: outputIndex, content_index: 0,
            part: { ...part, text: '' } });
          for (const delta of slices(part.text)) {
            yield event('response.output_text.delta', { item_id: item.id, output_index: outputIndex, content_index: 0, delta });
          }
          yield event('response.output_text.done', { item_id: item.id, output_index: outputIndex, content_index: 0, text: part.text });
          yield event('response.content_part.done', { item_id: item.id, output_index: outputIndex, content_index: 0, part });
          yield event('response.output_item.done', { output_index: outputIndex, item });
        }
      }
      yield event('response.completed', { response: result });
    },
    *error(error) {
      const output = publicError(error);
      if (request.family === 'chat') yield `data: ${JSON.stringify(output)}\n\n`;
      else yield event('error', { code: output.error.code, message: output.error.message, param: output.error.param ?? null });
    },
  };
}

export function writeCompletedStream(res, request, result) {
  const frames = streamFrames(request, { id: result.id, created: result.created ?? result.created_at });
  for (const frame of frames.begin()) res.write(typeof frame === 'function' ? frame() : frame);
  for (const frame of frames.finish(result)) res.write(typeof frame === 'function' ? frame() : frame);
  res.end();
}

export function createStreamWriter(res, request, { signal, onDisconnect } = {}) {
  const identity = resultIdentity(request);
  const frames = streamFrames(request, identity);
  let pending;
  let started = false;
  let terminalRequested = false;
  let errorRequested = false;
  let ended = false;
  const write = (frame, allowAborted) => new Promise((resolve, reject) => {
    let settled = false;
    const cleanup = () => {
      res.off('drain', drained); res.off('close', disconnected); res.off('error', disconnected);
      signal?.removeEventListener('abort', onAbort);
    };
    const drained = () => { if (settled) return; settled = true; cleanup(); resolve(); };
    const cancelled = () => {
      if (settled) return;
      settled = true; cleanup();
      reject(signal.reason instanceof PrismError ? signal.reason : new PrismError('request_cancelled', 499));
    };
    const disconnected = () => {
      if (settled) return;
      settled = true; cleanup(); onDisconnect?.(); reject(new PrismError('request_cancelled', 499));
    };
    const onAbort = allowAborted ? drained : cancelled;
    if (res.destroyed || res.writableEnded) return disconnected();
    if (!allowAborted && signal?.aborted) return cancelled();
    res.once('close', disconnected); res.once('error', disconnected);
    signal?.addEventListener('abort', onAbort, { once: true });
    try {
      const writable = res.write(typeof frame === 'function' ? frame() : frame);
      // A deadline may still enqueue its one bounded error frame, then end the response.
      if (!settled && writable === false && !(allowAborted && signal?.aborted)) res.once('drain', drained);
      else drained();
    } catch { disconnected(); }
  });
  const run = (source, terminal = false, allowAborted = false) => {
    const previous = pending;
    const task = (async () => {
      if (previous) await previous.catch(() => {});
      for (const frame of source) await write(frame, allowAborted);
      if (terminal && !res.destroyed && !res.writableEnded) { ended = true; res.end(); }
    })();
    pending = task;
    task.catch(() => {}).finally(() => { if (pending === task) pending = null; });
    return task;
  };
  return {
    identity,
    begin() {
      if (started || terminalRequested || ended) return pending || Promise.resolve();
      started = true;
      return run((function* () { yield ': waiting for Prism\n\n'; yield* frames.begin(); })());
    },
    heartbeat() {
      if (pending || terminalRequested || ended) return Promise.resolve();
      return run(frames.heartbeat());
    },
    finish(result) {
      if (terminalRequested || ended) return pending || Promise.resolve();
      terminalRequested = true;
      return run(frames.finish(result), true);
    },
    error(error) {
      if (errorRequested || ended || res.destroyed || res.writableEnded) return pending || Promise.resolve();
      errorRequested = true;
      terminalRequested = true;
      return run(frames.error(error), true, true);
    },
  };
}
