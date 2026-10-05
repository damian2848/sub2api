import { PrismError, aborted, interruptible, projectRuntimeRateLimitedError } from './errors.mjs';
import { NativeAttachmentUpload, nativeAttachmentInput } from './browser-attachments.mjs';
import { httpCacheEnabled, installCachePreservingInterceptor } from './cache-interceptor.mjs';
import { multiplexEnabled } from './page-multiplexer.mjs';

const origin = 'https://prism.openai.com';
const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

// While Statsig (the prism_codex_models dynamic config) is still loading, the model menu
// shows a fallback list of only this model. Such a result must not replace a larger catalog.
export const FALLBACK_MODEL = 'gpt-5.6-sol';
export function catalogCollapsed(collected, previous = []) {
  return collected.length === 1 && collected[0] === FALLBACK_MODEL && previous.length > 1;
}

// Prism builds its model menu from the `prism_codex_models` Statsig dynamic config. The menu can sit
// on its one-model loading fallback although the client is Ready and already holds the full list,
// so the config itself is the catalog; the menu is only a fallback for a client that exposes none.
const MODEL_ID = /^gpt-[0-9]+(?:\.[0-9]+)?(?:-[a-z0-9]+)+$/;
export function catalogFromConfig(models) {
  const labels = new Map();
  if (!Array.isArray(models)) return labels;
  for (const item of models.slice(0, 32)) {
    if (!item || typeof item.id !== 'string' || !MODEL_ID.test(item.id)) continue;
    labels.set(item.id, typeof item.label === 'string' && item.label.trim() ? item.label.trim() : item.id);
  }
  return labels;
}

// The model a readiness probe uses: the known-good fallback when offered, else the first model.
export const probeModel = models => models.includes(FALLBACK_MODEL) ? FALLBACK_MODEL : models[0];

// Structure of a Prism metadata object for the audit log: key paths and value types only. Numbers are
// kept just for keys that look like counters (tokens, usage, cache, timings); text is never logged.
const COUNTER_KEY = /token|usage|cache|cached|count|_ms$|ms$|duration|elapsed/i;
export function payloadShape(value, path = '', out = [], depth = 0) {
  if (out.length >= 80) return out;
  if (Array.isArray(value)) {
    out.push(`${path}: array(${value.length})`);
    if (value.length && depth < 5) payloadShape(value[0], `${path}[0]`, out, depth + 1);
  } else if (value && typeof value === 'object') {
    const keys = Object.keys(value).filter(key => /^[A-Za-z0-9_.-]{1,64}$/.test(key)).sort();
    if (!keys.length || depth >= 5) out.push(`${path}: object(${Object.keys(value).length})`);
    else for (const key of keys) payloadShape(value[key], path ? `${path}.${key}` : key, out, depth + 1);
  } else if (typeof value === 'number') {
    out.push(`${path}: ${COUNTER_KEY.test(path.split('.').at(-1) || '') && Number.isFinite(value) ? value : 'number'}`);
  } else out.push(`${path}: ${value === null ? 'null' : typeof value === 'string' ? `string(${value.length})` : typeof value}`);
  return out;
}

export const STATUS_PATH = '/api/llm/response_with_tools_status';
export function statusPollInterval(value = process.env.PRISM_STATUS_POLL_MS) {
  const parsed = value === undefined || value === '' ? 1000 : Number(value);
  return Number.isInteger(parsed) && (parsed === 0 || (parsed >= 250 && parsed <= 10000)) ? parsed : 1000;
}

// Prism keeps every chat tab it opened mounted (hidden) in the page, and each request opens one more,
// so the page and the time to prepare a request grow with every turn. Chat tab ids are
// `chat:<epoch ms>`; file tabs use project node ids.
export const CHAT_TAB_SELECTOR = '[data-tab-id^="chat:"]';
// A chat opened while the worker was idle is used only while it is this fresh.
export const PREPARED_CHAT_MAX_AGE_MS = 10 * 60 * 1000;

// Prism renders this in the project page when its sandbox startup allowance is exhausted. Keep
// the match narrow and bounded so an unrelated user prompt cannot turn into a rate-limit signal.
export const RUNTIME_RATE_LIMIT = /项目运行环境的启动请求受到限流|(?:project|sandbox|runtime).{0,80}(?:startup|start).{0,80}rate[ -]?limit/i;

export async function runtimeRateLimitVisible(page) {
  try {
    return Boolean(await page.getByText(RUNTIME_RATE_LIMIT).first().isVisible());
  } catch {
    return false;
  }
}

export function terminalFailureReason(payload = {}) {
  const reason = typeof payload.reason === 'string' ? payload.reason : '';
  if (reason === 'sandbox_reconnecting' || reason === 'conversation_too_large' ||
    reason === 'project_edit_access_required') return reason;
  return null;
}

// PRISM_PREWARM_CHAT=false turns off opening the next chat (and closing old chat tabs) between turns.
export function prewarmEnabled(value = process.env.PRISM_PREWARM_CHAT) {
  return !['false', '0', 'off'].includes(String(value ?? '').trim().toLowerCase());
}

// Runs in the page: closes every chat tab except the active and the newest one the way a middle click
// on a tab does (Prism's tab handles mousedown with button 1). One tab at a time with a yield in
// between, so each close is applied to the tab list the previous one left.
export async function closeOldChatTabsInPage(selector) {
  const tabs = () => Array.from(document.querySelectorAll(selector));
  const stamp = element => Number(element.getAttribute('data-tab-id').slice('chat:'.length)) || 0;
  const before = tabs();
  if (before.length < 2) return { before: before.length, after: before.length };
  const newest = before.reduce((best, element) => stamp(element) > stamp(best) ? element : best);
  const keep = new Set([newest.getAttribute('data-tab-id'), ...before
    .filter(element => String(element.className).includes('--tabs-active-border'))
    .map(element => element.getAttribute('data-tab-id'))]);
  for (const element of before) {
    if (keep.has(element.getAttribute('data-tab-id')) || !element.isConnected) continue;
    element.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, cancelable: true, button: 1 }));
    await new Promise(resolve => setTimeout(resolve, 0));
  }
  await new Promise(resolve => setTimeout(resolve, 50));
  return { before: before.length, after: tabs().length };
}

const inputChars = input => (Array.isArray(input) ? input : []).reduce((sum, item) => sum +
  (Array.isArray(item?.content) ? item.content.reduce((total, part) =>
    total + (typeof part?.text === 'string' ? part.text.length : 0), 0) : 0), 0);

// Counts of a Prism output's item types (message, reasoning, ...) for the audit; names only.
function outputTypes(output) {
  if (!Array.isArray(output)) return undefined;
  const counts = {};
  for (const item of output.slice(0, 200)) {
    if (typeof item?.type === 'string' && /^[a-z_]{1,40}$/.test(item.type)) counts[item.type] = (counts[item.type] || 0) + 1;
  }
  return counts;
}

// What the page itself calls a tool call of Prism's own agent: a coarse kind, used for its progress line.
export function toolProgressKind(call) {
  const text = [call?.name, call?.call_type, call?.arguments_preview ?? '', call?.source ?? ''].join(' ').toLowerCase();
  if (text.includes('apply_patch')) return 'edit';
  if (/\brg\b|\bgrep\b|\bsearch\b/.test(text)) return 'search';
  if (/\bls\b|\blist\b|\bfind\b|\bglob\b/.test(text)) return 'explore';
  if (/\bcat\b|\bsed\b|\bhead\b|\btail\b|\bread\b/.test(text)) return 'read';
  if (/\bexec\b|\brun\b|\bcommand\b|\bbash\b|\bshell\b/.test(text)) return 'command';
  return 'other';
}

const plural = (count, noun) => `${count} ${noun}${count === 1 ? '' : 's'}`;
// Progress lines for the tool calls seen so far, one per kind, in the order the page shows them.
export function toolProgressLines(counts) {
  const lines = [];
  if (counts.explore) lines.push(`Explored ${plural(counts.explore, 'location')}`);
  if (counts.search) lines.push('Searched');
  if (counts.read) lines.push(`Read ${plural(counts.read, 'file')}`);
  if (counts.edit) lines.push('Editing files');
  if (counts.command) lines.push('Running a command');
  if (!lines.length && counts.other) lines.push('Using a tool');
  return lines;
}

// Streaming Responses clients get Prism's progress as reasoning summaries; keep each part and the total bounded.
export const MAX_FORWARDED_PARTS = 64;
export const MAX_FORWARDED_CHARS = 4000;
const normalizedNote = text => text.replace(/\s+/g, ' ').trim();

export function modelFromLabel(label) {
  const value = label.trim().replace(/\s+/g, ' ');
  const match = /^(\d+(?:\.\d+)?) (Sol|Terra|Luna|Astra)\b/.exec(value);
  return match ? `gpt-${match[1]}-${match[2].toLowerCase()}` : null;
}

export class BrowserSession {
  constructor(browser, onHeartbeat, source, worker = 0, options = {}) {
    this.browser = browser;
    this.origin = new URL(options.origin || origin).origin;
    this.admissionGuard = options.admissionGuard;
    this.metrics = options.metrics;
    this.multiplexer = options.multiplexer || null;
    this.multiplex = (options.multiplex ?? multiplexEnabled()) && Boolean(this.multiplexer);
    this.cacheMode = null;
    this.authIdentity = null;
    this.authCredentials = null;
    this.httpCache = options.httpCache ?? httpCacheEnabled();
    this.onHeartbeat = onHeartbeat;
    this.context = null;
    this.contextEpoch = 0;
    this.page = null;
    this.projectId = null;
    this.creating = false;
    this.bootstrapping = false;
    this.turn = null;
    this.syncSeen = false;
    this.lastHeartbeat = 0;
    this.labels = new Map();
    this.reserveProject = null;
    this.source = source;
    this.worker = worker;
    // The official page polls status every 3.4-4 s; we poll every statusPollMs as well (0: page only).
    this.statusPollMs = statusPollInterval();
    this.prewarm = options.prewarm ?? prewarmEnabled();
    // The next chat, opened while idle: a promise of { page, generation, at } or null.
    this.preparing = null;
    // Bumped whenever the page is (re)loaded, so a chat prepared on an earlier load is never used.
    this.pageGeneration = 0;
  }

  audit(event, details) {
    if (process.env.PRISM_AUDIT_REQUESTS === 'true') console.log(JSON.stringify({ event, source: this.source,
      worker: this.worker, ...details }));
  }

  async authenticate({ access_token, expected_email, expected_user_id }, signal) {
    const epoch = this.contextEpoch;
    let context;
    let installed = false;
    const checkCurrent = () => {
      aborted(signal);
      if (this.contextEpoch !== epoch) throw new PrismError('browser_session_closed', 503);
    };
    return interruptible(async () => {
      try {
        await this.admissionGuard?.assertAdmission('context');
        checkCurrent();
        // Blocking service workers is part of the opt-in CDP/cache path; the default page environment is unchanged.
        context = await this.browser.newContext({ locale: 'en-US', ...(this.httpCache ? { serviceWorkers: 'block' } : {}) });
        checkCurrent();
        this.context = context;
        installed = true;
        await context.addCookies([{ name: 'prism_oai_access_token', value: access_token,
          domain: new URL(this.origin).hostname, path: '/', secure: new URL(this.origin).protocol === 'https:',
          httpOnly: true, sameSite: 'Lax' }]);
        checkCurrent();
        const page = await this.createSubmissionPage(epoch);
        checkCurrent();
        await this.loadPage(page, this.origin + '/', { waitUntil: 'domcontentloaded', timeout: 25000 });
        checkCurrent();
        const auth = await page.evaluate(async expected => {
          const response = await fetch('/auth/session', { signal: AbortSignal.timeout(15000) });
          const data = await response.json();
          const user = data.user || {};
          const policy = data.policy?.user || {};
          const ids = [user.id, user.app_metadata?.user_id, policy.id, policy.openai_user_id, policy.prism_user_id];
          const emails = [user.email, policy.email].filter(value => typeof value === 'string').map(value => value.toLowerCase());
          return { signedIn: response.ok && Boolean(user.id) && !user.is_anonymous, actualUserId: user.id,
            emailMatches: !expected.email || emails.includes(expected.email.toLowerCase()),
            idMatches: !expected.id || ids.includes(expected.id) };
        }, { email: expected_email, id: expected_user_id });
        checkCurrent();
        if (!auth.signedIn) throw new PrismError('oauth_session_rejected', 401);
        if (!auth.emailMatches || !auth.idMatches) throw new PrismError('oauth_identity_mismatch', 403);
        // AccountManager verifies its persisted source identity AFTER authenticate returns.
        // Do not establish a shared resident with an identity that may still be rejected.
        this.authIdentity = auth.actualUserId;
        this.authCredentials = { access_token };
        return auth.actualUserId;
      } catch (error) {
        if (context && !installed) await context.close().catch(() => {});
        else if (context && this.context === context) await this.terminateContext();
        throw error;
      }
    }, signal, () => this.contextEpoch === epoch ? this.close() : undefined);
  }

  async loadPage(page, url, options) {
    const begun = performance.now();
    try { return await page.goto(url, options); }
    finally { this.metrics?.record('page_load', performance.now() - begun,
      { worker: this.worker, multiplex: this.multiplex, cache_mode: this.cacheMode }); }
  }

  async createSubmissionPage(epoch = this.contextEpoch) {
    if (!this.context || this.contextEpoch !== epoch) throw new PrismError('browser_session_closed', 503);
    const context = this.context;
    await this.admissionGuard?.assertAdmission('page');
    if (this.context !== context || this.contextEpoch !== epoch) throw new PrismError('browser_session_closed', 503);
    const page = await context.newPage();
    if (!this.context || this.contextEpoch !== epoch) {
      await page.close?.().catch(() => {});
      throw new PrismError('browser_session_closed', 503);
    }
    this.page = page;
    const currentPage = () => this.contextEpoch === epoch && this.page === page;
    await installCachePreservingInterceptor(this.context, page, { origin: this.origin,
      enabled: this.httpCache, current: currentPage, route: route => this.route(route), observe: response => this.observe(response),
      onMode: mode => { this.cacheMode = mode; this.audit('browser_cache_mode', { mode }); } });
    page.on('close', () => { if (currentPage()) this.turn?.reject(new PrismError('browser_session_closed', 503)); });
    page.on('crash', () => { if (currentPage()) this.close().catch(() => {}); });
    return page;
  }

  async detachSubmissionPage(turn) {
    if (!this.multiplex || turn.detached || !turn.submitFinished || !turn.statusTemplate || turn.completed || this.turn !== turn) return;
    // No page is released until start acceptance and the first matching successful native status
    // provide the exact opaque polling shape. The resident poller never invents an upstream body.
    this.multiplexer.startPolling(this, turn);
    turn.detached = true;
    const page = this.page;
    this.page = null;
    this.preparing = null;
    this.pageGeneration += 1;
    turn.releaseSubmission?.(); turn.releaseSubmission = null;
    await page?.close().catch(() => {});
    this.audit('submission_page_detached', { worker: this.worker, independent_context: true });
  }

  async route(route) {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    const mutation = !['GET', 'HEAD'].includes(request.method());
    if (path === '/api/projects' && mutation) {
      if (!this.creating || this.projectId || request.method() !== 'POST') return route.abort();
      const body = request.postDataJSON();
      if (!uuidPattern.test(body?.project_uuid || '')) return route.abort();
      this.projectId = body.project_uuid;
      // Remember the UI-generated UUID before a request can outlive its client.
      await this.reserveProject?.(this.projectId);
      if (!this.creating) return route.abort();
    } else if (path.startsWith('/api/projects/') && mutation &&
      (!this.projectId || !path.startsWith(`/api/projects/${this.projectId}/`))) {
      // No request may alter an unrelated project, including its deletion.
      return route.abort();
    }
    if (path === '/api/backend/1/new' && !this.bootstrapping && !this.projectId) return route.abort();
    if (path === '/api/project-files/upload' && mutation) {
      if (!this.turn?.attachments?.allowsUpload(request)) return route.abort();
    }
    if (path === '/api/llm/response_with_tools_start') {
      const turn = this.turn;
      if (!turn || turn.started || turn.signal?.aborted) return route.abort();
      if (!turn.submitAllowed) {
        this.audit('upstream_start_blocked', { code: 'browser_start_before_submit' });
        return route.abort();
      }
      const body = request.postDataJSON();
      const mismatch = body?.metadata?.projectId !== this.projectId ? 'browser_project_mismatch' :
        body.previousResponseId ? 'browser_previous_context_present' : null;
      if (mismatch) {
        this.audit('upstream_start_blocked', { code: mismatch, model: body?.metadata?.model,
          effort: body?.metadata?.reasoning_effort, previous_context_present: Boolean(body.previousResponseId) });
        turn.reject(new PrismError(mismatch));
        return route.abort();
      }
      let input = turn.request.input;
      if (turn.request.attachments?.length) {
        try {
          input = nativeAttachmentInput(input, body, turn.request.attachments, turn.attachments?.references);
        } catch (error) {
          turn.reject(error);
          return route.abort();
        }
      }
      turn.started = true;
      turn.startRequest = request;
      turn.conversationId = body.conversationId;
      turn.startedAt = performance.now();
      // prep_ms: from taking the request to Prism's start; submit_ms: from pressing Enter to it (the page's
      // own work, e.g. its Sentinel proof); prewarmed: the chat was opened while the worker was idle.
      const timing = turn.timing;
      this.audit('upstream_start', { model: turn.request.model, effort: turn.request.effort, ui_model: body.metadata.model,
        sentinel_present: Boolean(request.headers?.()['openai-sentinel-token']), input_roles: turn.request.input.map(item => item.role),
        input_chars: inputChars(turn.request.input),
        ...(timing ? { prep_ms: Math.round(turn.startedAt - timing.begun), prewarmed: timing.prewarmed,
          submit_ms: timing.submit ? Math.round(turn.startedAt - timing.submit) : undefined } : {}) });
      // The official UI still generates Sentinel proof, identity, and sandbox metadata.
      // Replace only validated text so no unrelated native UI history is sent. The UI's own model
      // and effort controls can sit on their loading defaults, so the exact requested values
      // replace them; select() has already confirmed the model is in Prism's catalog.
      return route.continue({ postData: JSON.stringify({ ...body, input,
        metadata: { ...body.metadata, model: turn.request.model, reasoning_effort: turn.request.effort } }) });
    }
    if (path.startsWith('/api/llm/') && !this.turn && !path.endsWith('_stop')) return route.abort();
    return route.continue();
  }

  async observe(response) {
    const epoch = this.contextEpoch;
    const path = new URL(response.url()).pathname;
    if (path.endsWith('/heartbeat')) {
      if (response.ok()) {
        this.lastHeartbeat = Math.floor(Date.now() / 1000);
        this.onHeartbeat(this.lastHeartbeat);
      } else if ([401, 403].includes(response.status())) this.onHeartbeat(0, 'session_expired');
      return;
    }
    if (path.endsWith('/wait-for-sync') && response.ok()) {
      const data = await response.json();
      if (epoch === this.contextEpoch) this.syncSeen = data.status === 'synced';
      return;
    }
    if (!['/api/llm/response_with_tools_start', '/api/llm/response_with_tools_status'].includes(path)) return;
    const turn = this.turn;
    if (!turn?.started) return;
    const request = response.request();
    const sent = request.postDataJSON();
    const isStart = path === '/api/llm/response_with_tools_start';
    // Old tabs can finish polling after a new chat has started, including HTTP errors.
    const matchesTurn = () => this.turn === turn && !turn.completed && !turn.signal?.aborted && (isStart
      ? request === turn.startRequest && sent?.conversationId === turn.conversationId
      : Boolean(turn.requestId && sent?.request_id === turn.requestId &&
        (!sent.conversation_id || sent.conversation_id === turn.conversationId)));
    if (!matchesTurn()) return;
    if (isStart) {
      this.audit('upstream_start_actual_input', { input_roles: (sent?.input || []).map(item => item.role) });
    }
    // Our status polls run alongside the page's. Only three consecutive own-poll failures stop ours;
    // while ours are healthy, a failed page poll is not fatal either. Real turn failures arrive in
    // a 200 response, and either poller can still observe that terminal state.
    const own = !isStart && Boolean(turn.ownBodies?.has(request.postData?.()));
    if (!response.ok() && !isStart && (own || (turn.ownPolling && !turn.ownPollFailed))) {
      this.audit('status_poll_http_error', { status: response.status(), own });
      return;
    }
    if (!response.ok()) {
      const status = response.status();
      this.audit('upstream_http_error', { status });
      const error = new PrismError([401, 403].includes(status) ? 'session_expired' : 'prism_upstream_http_error',
        [401, 403].includes(status) ? 401 : 502);
      // Prism's own servers failed (overloaded, bad gateway, ...): one resubmission may succeed.
      error.transient = Number.isInteger(status) && status >= 500 && status <= 599;
      return turn.reject(error);
    }
    let data;
    try { data = await response.json(); } catch (error) {
      if (own) return;
      throw error;
    }
    if (!matchesTurn()) return;
    if (data.request_id && turn.requestId && data.request_id !== turn.requestId) return;
    if (data.conversation_id && data.conversation_id !== turn.conversationId) return;
    if (own) turn.ownPollErrors = 0;
    if (data.request_id) turn.requestId = data.request_id;
    if (data.turn_state) turn.turnState = data.turn_state;
    if (data.conversation_id) turn.conversationId = data.conversation_id;
    this.trackProgress(turn, data.codex_live_progress);
    const terminal = ['completed', 'error', 'failed'].includes(data.status);
    // Prism accepted the start (it is running, or already finished successfully): a streaming
    // caller may now open its stream. A refused start never opens it, so it can still be a 429.
    if (isStart && !(terminal && data.response?.status !== 'success')) turn.request.onAccepted?.();
    if (!isStart && !own && !turn.statusTemplate && sent && typeof sent === 'object') {
      // The page's first good poll gives the exact body shape (diff_format etc.); ours copy it.
      turn.statusTemplate = sent;
      turn.nativeStatusHeaderNames = headerNames(request);
      if (this.multiplex) {
        if (!terminal) await this.detachSubmissionPage(turn);
      } else if (this.statusPollMs > 0 && turn.ownBodies) this.pollStatus(turn).catch(() => { turn.ownPollFailed = true; });
    }
    if (!['completed', 'error', 'failed'].includes(data.status)) return;
    turn.completed = true;
    turn.completedBy = isStart ? 'start' : own ? 'own_poll' : 'page_poll';
    const payload = data.response?.payload || {};
    const resubmissionRequested = data.status === 'completed' && data.response?.status === 'error' &&
      payload.httpStatus === 403 && payload.reason === 'unknown' && payload.message ===
      'Error while processing conversation (403 Forbidden). Please submit prompt again.';
    this.audit('upstream_result', { status: data.response?.status,
      reported_model: payload.model || null,
      failure_code: terminalFailureReason(payload) || (payload.reason === 'unknown' ? 'unknown' : undefined),
      payload_http_status: Number.isInteger(payload.httpStatus) && payload.httpStatus >= 100 && payload.httpStatus <= 599
        ? payload.httpStatus : undefined,
      resubmission_requested: resubmissionRequested, completed_by: turn.completedBy,
      own_polls: turn.ownPolls, own_poll_failed: turn.ownPollFailed, own_poll_errors: turn.ownPollErrorTotal || 0,
      prism_ms: turn.startedAt ? Math.round(performance.now() - turn.startedAt) : undefined,
      internal_tool_calls: turn.internalTools?.size || 0, internal_tool_names: [...(turn.internalToolNames || [])],
      reasoning_summaries: turn.reasoningSummaries?.size || 0, reasoning_forwarded: turn.notes?.forwarded || 0,
      output_types: outputTypes(payload.output),
      exec_meta_shape: payloadShape(payload.codexExecMeta), debug_shape: payloadShape(payload.codexDebug) });
    const terminalReason = terminalFailureReason(payload);
    if (data.response?.status !== 'success' && (payload.httpStatus === 413 || terminalReason === 'conversation_too_large')) {
      // Prism refused the size of the conversation. Every account would refuse it the same way, so it is the
      // request's fault: a 400 the client can act on (compact), never a 5xx that is retried or cools accounts down.
      return turn.reject(new PrismError('context_length_exceeded', 400));
    }
    if (data.response?.status !== 'success' && terminalReason === 'sandbox_reconnecting') {
      // Do not turn a sandbox reconnect into a generic resubmission: it may already have
      // executed. Only the official page can safely reconcile that sandbox state.
      return turn.reject(new PrismError('sandbox_reconnecting', 503));
    }
    if (data.response?.status !== 'success' && terminalReason === 'project_edit_access_required') {
      return turn.reject(new PrismError('project_edit_access_required', 403));
    }
    if (data.response?.status !== 'success' && isStart && resubmissionRequested) {
      // Refused at the start itself ("please submit prompt again"): Prism's start allowance for the
      // account, not a broken project, so no project refresh and no retry on this account.
      return turn.reject(new PrismError('prism_start_rejected', 429));
    }
    if (data.response?.status !== 'success') {
      const error = new PrismError('prism_generation_failed');
      error.retryConversation = resubmissionRequested;
      error.transient = Number.isInteger(payload.httpStatus) && payload.httpStatus >= 500 && payload.httpStatus <= 599;
      return turn.reject(error);
    }
    if (payload.model && payload.model !== turn.request.model) return turn.reject(new PrismError('upstream_model_mismatch'));
    const output = payload.output;
    if (!Array.isArray(output)) return turn.reject(new PrismError('prism_invalid_output'));
    const messages = output.filter(item => item.type === 'message');
    const text = messages.flatMap(item => item.content || []).filter(item => item.type === 'output_text' ||
      item.type === 'text').map(item => typeof item.text === 'string' ? item.text : '').join('');
    if (!text) return turn.reject(new PrismError('prism_empty_output'));
    turn.resolve(text);
  }

  // Prism runs its own agent behind the start. Its live progress lists the tools that agent called and its
  // reasoning summaries; the audit keeps only how many there were and the tool names.
  trackProgress(turn, progress) {
    if (!progress || typeof progress !== 'object') return;
    this.forwardProgress(turn, progress);
    if (Array.isArray(progress.toolCalls)) {
      turn.internalTools ??= new Set();
      turn.internalToolNames ??= new Set();
      for (const call of progress.toolCalls.slice(0, 500)) {
        if (!call || typeof call !== 'object' || turn.internalTools.size >= 1000) continue;
        turn.internalTools.add(`${call.line_index}:${call.call_id ?? ''}:${call.name ?? ''}`);
        if (typeof call.name === 'string' && /^[A-Za-z0-9_.:-]{1,64}$/.test(call.name) && turn.internalToolNames.size < 12) {
          turn.internalToolNames.add(call.name);
        }
      }
    }
    if (Array.isArray(progress.reasoningSummaries)) {
      turn.reasoningSummaries ??= new Set();
      for (const summary of progress.reasoningSummaries.slice(0, 500)) {
        if (turn.reasoningSummaries.size < 1000) {
          turn.reasoningSummaries.add(`${summary?.line_index}:${typeof summary?.text === 'string' ? summary.text.length : ''}`);
        }
      }
    }
  }

  // Hands Prism's new reasoning summaries and tool progress to the caller (request.onReasoning), once each.
  // A failing callback never disturbs the turn.
  forwardProgress(turn, progress) {
    const emit = turn.request.onReasoning;
    if (typeof emit !== 'function') return;
    turn.notes ??= { seen: new Set(), toolKeys: new Set(), counts: {}, lines: new Set(), forwarded: 0 };
    const notes = turn.notes;
    const send = text => {
      if (notes.forwarded >= MAX_FORWARDED_PARTS) return;
      notes.forwarded += 1;
      try { emit(text.length > MAX_FORWARDED_CHARS ? `${text.slice(0, MAX_FORWARDED_CHARS - 1)}…` : text); } catch { /* ignored */ }
    };
    if (Array.isArray(progress.reasoningSummaries)) {
      for (const summary of progress.reasoningSummaries.slice(0, 500)) {
        const text = typeof summary?.text === 'string' ? summary.text.trim() : '';
        const key = normalizedNote(text);
        if (!key || notes.seen.has(key)) continue;
        notes.seen.add(key);
        send(text);
      }
    }
    if (Array.isArray(progress.toolCalls)) {
      let changed = false;
      for (const call of progress.toolCalls.slice(0, 500)) {
        if (!call || typeof call !== 'object') continue;
        const key = [call.line_index, call.call_id ?? '', call.name, call.call_type].join(':');
        if (notes.toolKeys.has(key)) continue;
        notes.toolKeys.add(key);
        const kind = toolProgressKind(call);
        notes.counts[kind] = (notes.counts[kind] || 0) + 1;
        changed = true;
      }
      if (changed) {
        // A line is sent once; a count that grew ("Explored 2 locations") is a new line.
        for (const line of toolProgressLines(notes.counts)) {
          if (notes.lines.has(line)) continue;
          notes.lines.add(line);
          send(`**${line}**`);
        }
      }
    }
  }

  async initialize(projectId, onProjectCreated, signal, previousModels = []) {
    const epoch = this.contextEpoch;
    // A project refresh may begin while the idle prewarm promise is still waiting for
    // its composer.  Never let that promise keep mutating the page we are about to
    // navigate: invalidate it before the first await, and retire the old page so a
    // pending Playwright action fails quickly instead of holding a 120s composer wait.
    const stalePage = this.page;
    const stalePreparation = this.preparing;
    this.preparing = null;
    this.pageGeneration += 1;
    if (stalePreparation && stalePage && !this.turn && !this.multiplex) {
      this.page = null;
      await stalePage.close?.().catch(() => {});
    }
    let releasePreparation;
    if (this.multiplex) releasePreparation = await this.multiplexer.acquireSubmission(signal);
    const preparationAt = performance.now();
    // A newly created project always starts on the blank chat rendered by ?n=1.
    // A reload may restore a project chat with prior history, so it must still
    // open a fresh chat in generate() unless the page was created above.
    const freshProjectChat = !projectId;
    let page;
    try {
      await interruptible(async () => {
        await this.admissionGuard?.assertAdmission('project');
        aborted(signal);
        if (this.contextEpoch !== epoch) throw new PrismError('browser_session_closed', 503);
        if (this.multiplex) {
          if (!this.authIdentity || !this.authCredentials) throw new PrismError('oauth_identity_unavailable', 403);
          await this.multiplexer.register(this.browser, this.authCredentials, this.authIdentity, this);
          if (this.contextEpoch !== epoch) {
            this.multiplexer.unregister(this);
            throw new PrismError('browser_session_closed', 503);
          }
          aborted(signal);
        }
        page = this.page || await this.createSubmissionPage(epoch);
      }, signal, () => this.contextEpoch === epoch ? this.close() : undefined);
    } catch (error) { releasePreparation?.(); throw error; }
    // A null means a NEW isolated project, not reuse of the previous scope's project UUID.
    this.projectId = projectId || null;
    const initializationGeneration = this.pageGeneration;
    const checkCurrent = () => {
      aborted(signal);
      if (this.contextEpoch !== epoch || this.pageGeneration !== initializationGeneration ||
        this.page !== page || !page || page.isClosed?.()) {
        throw new PrismError('browser_session_closed', 503);
      }
    };
    this.bootstrapping = true;
    this.syncSeen = false;
    this.lastHeartbeat = 0;
    this.reserveProject = onProjectCreated;
    try {
      checkCurrent();
      await interruptible(async () => {
        if (projectId) {
          if (!uuidPattern.test(projectId)) throw new PrismError('invalid_managed_project');
          this.projectId = projectId;
          await this.loadPage(page, `${this.origin}/?u=${encodeURIComponent(projectId)}&pg=1`,
            { waitUntil: 'domcontentloaded', timeout: 45000 });
          checkCurrent();
        } else {
          this.creating = true;
          const creation = page.waitForResponse(response => new URL(response.url()).pathname === '/api/projects' &&
            response.request().method() === 'POST', { timeout: 90000 });
          creation.catch(() => {});
          await this.loadPage(page, `${this.origin}/?n=1`, { waitUntil: 'domcontentloaded', timeout: 45000 });
          checkCurrent();
          const response = await creation;
          checkCurrent();
          if (response.status?.() === 429) throw projectRuntimeRateLimitedError();
          const data = await response.json();
          checkCurrent();
          if (!response.ok() || data.uuid !== this.projectId) throw new PrismError('project_creation_failed');
        }
        // A new project URL (`?n=1`) renders Prism's first, empty chat tab.
        // The composer wait below makes that tab ready for the first request;
        // only a fresh project is marked as prepared after catalog loading.
        // Reloaded projects still open a fresh chat in generate() because their
        // initial tab may restore conversation history.
        await this.composer(checkCurrent);
        checkCurrent();
        const deadline = Date.now() + 120000;
        while ((!this.syncSeen || !this.lastHeartbeat) && Date.now() < deadline) {
          checkCurrent();
          await page.waitForTimeout(250);
          checkCurrent();
        }
        if (!this.syncSeen || !this.lastHeartbeat) throw new PrismError('sandbox_initialization_timeout', 504);
        const labels = await this.catalog(previousModels, signal, checkCurrent);
        checkCurrent();
        this.labels = labels;
        if (!this.labels.size) throw new PrismError('model_catalog_unavailable');
        if (freshProjectChat && this.prewarm && this.page === page) {
          // Keep the same shape as schedulePrepare() so the first request takes
          // the existing initialized tab without another New chat click.
          this.preparing = Promise.resolve({ page, generation: this.pageGeneration, at: Date.now() });
        }
      }, signal, () => this.contextEpoch === epoch ? this.close() : undefined);
      return [...this.labels.keys()];
    } finally {
      releasePreparation?.();
      this.metrics?.record('project_prepare', performance.now() - preparationAt, { worker: this.worker, multiplex: this.multiplex });
      if (this.contextEpoch === epoch) {
        this.creating = false;
        this.bootstrapping = false;
        this.reserveProject = null;
      }
    }
  }

  // Waits (read-only) until the Statsig client reports Ready; its absence is tolerated.
  async waitForStatsig(signal, timeoutMs = 20000, pollMs = 250, checkCurrent = () => {}) {
    const deadline = Date.now() + timeoutMs;
    for (;;) {
      aborted(signal);
      checkCurrent();
      const status = await this.page.evaluate(() => window.__STATSIG__?.firstInstance?.loadingStatus ?? null)
        .catch(() => null);
      checkCurrent();
      if (status === 'Ready') return true;
      if (Date.now() >= deadline) {
        this.audit('statsig_wait_timeout', { status });
        return false;
      }
      await this.page.waitForTimeout(pollMs);
      checkCurrent();
    }
  }

  // Read-only: the models of the Statsig `prism_codex_models` config, or null when unavailable.
  async readConfigModels(checkCurrent = () => {}) {
    const page = this.page;
    checkCurrent();
    const models = await page.evaluate(() => {
      const config = window.__STATSIG__?.firstInstance?.getDynamicConfig?.('prism_codex_models');
      const value = config?.get?.('models', null) ?? config?.value?.models;
      return Array.isArray(value) ? value.map(item => ({ id: item?.id, label: item?.label })) : null;
    }).catch(() => null);
    checkCurrent();
    return models;
  }

  async readModelMenu(checkCurrent = () => {}) {
    const page = this.page;
    checkCurrent();
    await this.thinking(page).click();
    checkCurrent();
    await page.getByRole('menuitem', { name: /^(Model|模型)/ }).hover();
    checkCurrent();
    await page.getByRole('menuitem').filter({ hasText: /^\d+(?:\.\d+)? (?:Sol|Terra|Luna|Astra)\b/ }).first()
      .waitFor({ state: 'visible', timeout: 15000 });
    checkCurrent();
    const observed = new Map();
    const collect = async () => {
      const items = await page.getByRole('menuitem').evaluateAll(elements => elements.map(element => ({
        label: element.textContent.trim(), disabled: element.hasAttribute('data-disabled') ||
          element.getAttribute('aria-disabled') === 'true', visible: Boolean(element.getClientRects().length) })));
      checkCurrent();
      for (const item of items) observed.set(item.label, { ...item,
        visible: item.visible || observed.get(item.label)?.visible === true });
    };
    await collect();
    checkCurrent();
    const menu = page.getByRole('menu').last();
    for (const fraction of [0.5, 1]) {
      await menu.evaluate((element, offset) => { element.scrollTop = element.scrollHeight * offset; }, fraction);
      checkCurrent();
      await page.waitForTimeout(200);
      checkCurrent();
      await collect();
      checkCurrent();
    }
    await menu.evaluate(element => { element.scrollTop = 0; });
    checkCurrent();
    this.audit('model_menu', { items: [...observed.values()] });
    const labels = new Map();
    for (const item of observed.values()) {
      const id = modelFromLabel(item.label);
      if (id && !item.disabled && item.visible) labels.set(id, item.label);
    }
    await page.keyboard.press('Escape');
    checkCurrent();
    await page.keyboard.press('Escape');
    checkCurrent();
    return labels;
  }

  // The menu is only trusted once Statsig is Ready. If it still collapses to the fallback
  // list although the persisted catalog was larger, read it once more and accept that result.
  async catalog(previousModels, signal, checkCurrent = () => {}) {
    await this.waitForStatsig(signal, 20000, 250, checkCurrent);
    checkCurrent();
    const configured = catalogFromConfig(await this.readConfigModels(checkCurrent));
    if (configured.size) {
      this.audit('model_catalog_config', { models: [...configured.keys()] });
      return configured;
    }
    let labels = await this.readModelMenu(checkCurrent);
    checkCurrent();
    if (catalogCollapsed([...labels.keys()], previousModels)) {
      this.audit('model_catalog_collapsed', { previous_count: previousModels.length });
      await this.page.waitForTimeout(3000);
      aborted(signal);
      checkCurrent();
      labels = await this.readModelMenu(checkCurrent);
      checkCurrent();
    }
    return labels;
  }

  thinking(page = this.page) { return page.getByRole('button', { name: /^(Thinking|思考中):/ }); }

  async composer(checkCurrent = () => {}) {
    const page = this.page;
    checkCurrent();
    const composer = page.locator('textarea:visible').last();
    const deadline = Date.now() + 120000;
    while (Date.now() < deadline) {
      checkCurrent();
      if (await runtimeRateLimitVisible(page)) throw projectRuntimeRateLimitedError();
      const timeout = Math.min(500, Math.max(1, deadline - Date.now()));
      try { await composer.waitFor({ state: 'visible', timeout }); } catch { /* keep polling for the rate-limit banner */ }
      checkCurrent();
      if (await runtimeRateLimitVisible(page)) throw projectRuntimeRateLimitedError();
      try {
        await page.waitForFunction(() => {
          const item = Array.from(document.querySelectorAll('textarea')).filter(element => element.getClientRects().length).at(-1);
          return item && !item.disabled;
        }, null, { timeout: 500 });
        checkCurrent();
        return composer;
      } catch { /* editor is still loading */ }
    }
    if (await runtimeRateLimitVisible(page)) throw projectRuntimeRateLimitedError();
    throw new PrismError('project_editor_unavailable', 503);
  }

  // The model and effort are applied by route() on the native start. This only checks that Prism
  // offers the model; nothing is clicked in the (possibly still loading) menu.
  async select(request, checkCurrent = () => {}) {
    checkCurrent();
    if (!this.labels.has(request.model)) throw new PrismError('model_not_available', 400, 'model');
    if (!['low', 'medium', 'high', 'xhigh'].includes(request.effort)) throw new PrismError('unsupported_reasoning_effort', 400, 'reasoning_effort');
  }

  async generate(request, signal, onText) {
    aborted(signal);
    if (this.turn) throw new PrismError('account_busy', 409);
    const releaseSubmission = this.multiplex ? await this.multiplexer.acquireSubmission(signal) : null;
    const page = this.page;
    // onText is reserved for verified cumulative assistant text; pending progress is not that text.
    const turn = { request, signal, onText, started: false, submitAllowed: false,
      ownBodies: new Set(), ownPolls: 0, ownPolling: false, ownPollFailed: false,
      ownPollErrors: 0, ownPollErrorTotal: 0, statusTemplate: null, completedBy: null,
      releaseSubmission, submitFinished: false, detached: false };
    const checkCurrent = () => {
      aborted(signal);
      if (this.turn !== turn || (!turn.detached && (this.page !== page || !page || page.isClosed?.()))) {
        throw new PrismError('browser_session_closed', 503);
      }
    };
    const result = new Promise((resolve, reject) => { turn.resolve = resolve; turn.reject = reject; });
    // A native response can finish before press('Enter') returns.
    result.catch(() => {});
    const preparing = this.preparing;
    this.preparing = null;
    turn.timing = { begun: performance.now(), prewarmed: false, submit: 0 };
    this.turn = turn;
    let stage = 'new_chat';
    let succeeded = false;
    try {
      const text = await interruptible(async () => {
        checkCurrent();
        // The chat opened while the worker was idle, when it is still usable; otherwise open one now.
        const prepared = preparing ? await preparing : null;
        checkCurrent();
        let composer = prepared ? await this.preparedComposer(prepared, checkCurrent) : null;
        if (composer) turn.timing.prewarmed = true;
        else {
          await page.getByRole('button', { name: 'New chat tab', exact: true }).click({ timeout: 15000 });
          checkCurrent();
          stage = 'composer';
          composer = await this.composer(checkCurrent);
          checkCurrent();
        }
        stage = 'model_selection';
        await this.select(request, checkCurrent);
        checkCurrent();
        stage = 'submit';
        await composer.fill('Process the submitted text.');
        checkCurrent();
        if (request.attachments?.length) {
          stage = 'attachments';
          turn.attachments = new NativeAttachmentUpload(page, this.projectId, request.attachments, signal, checkCurrent);
          await turn.attachments.prepare();
          checkCurrent();
        }
        stage = 'submit';
        turn.submitAllowed = true;
        turn.timing.submit = performance.now();
        await composer.press('Enter');
        turn.submitFinished = true;
        await this.detachSubmissionPage(turn);
        checkCurrent();
        stage = 'generation';
        return result;
      }, signal, () => this.page === page ? this.close() : undefined);
      succeeded = true;
      return text;
    } catch (error) {
      this.audit('browser_ui_failure', { stage, error_type: error.constructor.name,
        code: error instanceof PrismError ? error.code : undefined });
      if (process.env.PRISM_AUDIT_REQUESTS === 'true' && this.page === page && page && !page.isClosed()) {
        const composers = await this.page.locator('textarea').evaluateAll(elements => elements.map(element => ({
          visible: Boolean(element.getClientRects().length), disabled: element.disabled }))).catch(() => []);
        const thinking = await this.thinking().allTextContents().catch(() => []);
        this.audit('browser_ui_state', { composers, thinking });
      }
      if (this.turn === turn) {
        if (signal?.aborted) await this.close();
        else if (turn.attachments && !turn.started) await this.terminateContext();
        else await this.stop();
      }
      throw error instanceof PrismError ? error : new PrismError(`browser_ui_${stage}_failed`);
    } finally {
      turn.releaseSubmission?.(); turn.releaseSubmission = null;
      this.multiplexer?.cancel(turn);
      if (this.turn === turn) this.turn = null;
      // A non-reusable isolated scope will never select this worker's prepared chat again.
      // Skip opening one for it; reusable conversations and legacy shared projects keep the warm path.
      const reusableScope = request.projectScope?.reusable !== false;
      // Only after a clean turn: a failed one may still be stopping, or be followed by a page reload.
      if (succeeded && reusableScope && this.page === page && !this.turn) this.schedulePrepare();
    }
  }

  // While the worker is idle: open the next chat, close the older chat tabs and wait for the composer,
  // so the next request only fills it in and submits. The promise never rejects; null means "not
  // prepared" and the request opens its chat itself, as without prewarming.
  schedulePrepare() {
    if (this.multiplex || !this.prewarm || !this.isAlive()) return;
    const page = this.page;
    const generation = this.pageGeneration;
    const checkCurrent = () => {
      if (this.page !== page || this.pageGeneration !== generation || !page || page.isClosed?.()) {
        throw new PrismError('browser_session_closed', 503);
      }
    };
    const begun = performance.now();
    let stage = 'new_chat';
    this.preparing = (async () => {
      try {
        // initialize() can invalidate a prewarm while this promise is pending. Check
        // before the first page action so a stale prewarm never clicks the new-chat
        // control on a page that is about to be reloaded.
        checkCurrent();
        await page.getByRole('button', { name: 'New chat tab', exact: true }).click({ timeout: 15000 });
        checkCurrent();
        stage = 'composer';
        await this.composer(checkCurrent);
        stage = 'close_tabs';
        const tabs = await page.evaluate(closeOldChatTabsInPage, CHAT_TAB_SELECTOR).catch(() => null);
        checkCurrent();
        stage = 'composer';
        await this.composer(checkCurrent);
        this.audit('chat_prepared', { prepare_ms: Math.round(performance.now() - begun),
          chat_tabs_before: tabs?.before, chat_tabs_after: tabs?.after });
        return { page, generation, at: Date.now() };
      } catch (error) {
        this.audit('chat_prepare_failed', { stage, error_type: error?.constructor?.name,
          code: error instanceof PrismError ? error.code : undefined });
        return null;
      }
    })();
  }

  // The composer of the chat schedulePrepare() opened, if it is from this page load, fresh, and still an
  // empty, enabled composer; otherwise null.
  async preparedComposer(prepared, checkCurrent = () => {}) {
    const page = this.page;
    if (prepared.page !== page || prepared.generation !== this.pageGeneration ||
      !(Date.now() - prepared.at <= PREPARED_CHAT_MAX_AGE_MS)) return null;
    const composer = page.locator('textarea:visible').last();
    const ready = await composer.evaluate(element => !element.disabled && element.value === '').catch(() => false);
    checkCurrent();
    return ready ? composer : null;
  }

  // Use the official page's fetch so polling carries its own session and verification; a Node-side
  // request would bypass that page context. Keep the latest turn_state and one request in flight,
  // retry transient failures, and leave polling to the page after three consecutive failures.
  async pollStatus(turn) {
    const page = this.page;
    turn.ownPolling = true;
    const live = () => this.turn === turn && !turn.completed && !turn.ownPollFailed && !turn.signal?.aborted &&
      this.page === page && page && !page.isClosed?.();
    while (live()) {
      const begun = Date.now();
      const body = JSON.stringify({ ...turn.statusTemplate, request_id: turn.requestId, turn_state: turn.turnState });
      turn.ownBodies.add(body);
      turn.ownPolls += 1;
      const ok = await page.evaluate(async ({ path, body }) => {
        try {
          const response = await fetch(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body,
            signal: AbortSignal.timeout(30000) });
          const text = await response.text();
          if (!response.ok) return false;
          JSON.parse(text);
          return true;
        } catch { return false; }
      }, { path: STATUS_PATH, body }).catch(() => false);
      if (ok) turn.ownPollErrors = 0;
      else {
        turn.ownPollErrors = (turn.ownPollErrors || 0) + 1;
        turn.ownPollErrorTotal = (turn.ownPollErrorTotal || 0) + 1;
        if (turn.ownPollErrors >= 3) turn.ownPollFailed = true;
      }
      const wait = turn.ownPollErrors > 0 ? Math.min(8000, this.statusPollMs * 2 ** turn.ownPollErrors)
        : this.statusPollMs - (Date.now() - begun);
      if (wait > 0 && live()) await new Promise(resolve => setTimeout(resolve, wait));
    }
  }

  async stop() {
    const turn = this.turn;
    if (!turn?.started || turn.completed || (!turn.detached && (!this.page || this.page.isClosed()))) return;
    if (!turn.stopping) turn.stopping = this.stopTurn(turn);
    return turn.stopping;
  }

  async stopTurn(turn) {
    if (turn.detached) {
      // Resident stop/cancel belongs only to this accepted turn; never close a shared poller.
      const stopped = await this.multiplexer.stop(turn);
      this.multiplexer.cancel(turn);
      if (!stopped) {
        this.audit('resident_stop_failed', { worker: this.worker });
        // Close only this submission context. Other in-flight turns and the resident stay alive.
        await this.terminateContext();
      }
      return;
    }
    try {
      const stop = this.page.getByTestId('ai-stop-button');
      const visible = await stop.first().waitFor({ state: 'visible', timeout: 5000 }).then(() => true).catch(() => false);
      if (visible) {
        const stopped = this.page.waitForResponse(response => new URL(response.url()).pathname ===
          '/api/llm/response_with_tools_stop', { timeout: 10000 });
        stopped.catch(() => {});
        await stop.first().click({ timeout: 5000 });
        if (!(await stopped).ok()) throw new PrismError('prism_stop_failed');
      } else if (turn.requestId && turn.turnState) {
        const ok = await this.page.evaluate(async body => {
          const response = await fetch('/api/llm/response_with_tools_stop', { method: 'POST',
            headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
            signal: AbortSignal.timeout(10000) });
          return response.ok;
        }, { request_id: turn.requestId, conversation_id: turn.conversationId, turn_state: turn.turnState });
        if (!ok) throw new PrismError('prism_stop_failed');
        await this.terminateContext();
      } else {
        await this.terminateContext();
      }
    } catch {
      await this.terminateContext();
    }
  }

  async close() {
    await this.stop();
    await this.terminateContext();
  }

  async terminateContext() {
    this.contextEpoch += 1;
    this.multiplexer?.unregister?.(this);
    const context = this.context;
    this.context = null;
    this.page = null;
    this.authIdentity = null;
    this.authCredentials = null;
    this.preparing = null;
    this.creating = false;
    this.bootstrapping = false;
    this.reserveProject = null;
    this.turn?.releaseSubmission?.();
    if (this.turn) this.multiplexer?.cancel(this.turn);
    this.turn?.reject(new PrismError('session_closed', 503));
    this.turn = null;
    if (context) await context.close().catch(() => {});
  }

  isAlive() { return Boolean(this.context && ((this.page && !this.page.isClosed()) ||
    (this.multiplex && this.multiplexer?.isAlive()))); }
}

// Names (never values) of the headers a request carried. Used only to explain why the resident poller
// is refused where the editor page's own poll for the same turn is accepted. Authorization-style
// values must never reach a log, so only the names survive; unusable names are dropped.
export function headerNames(request) {
  let headers;
  try { headers = request?.headers?.(); } catch { return undefined; }
  if (!headers || typeof headers !== 'object') return undefined;
  return Object.keys(headers).map(name => String(name).toLowerCase())
    .filter(name => /^[a-z0-9-]{1,64}$/.test(name)).sort();
}

export async function launchBrowser() {
  const { chromium } = await import('playwright');
  return chromium.launch({ headless: true,
    ...(process.env.PRISM_CHROMIUM_EXECUTABLE ? { executablePath: process.env.PRISM_CHROMIUM_EXECUTABLE } : {}) });
}
