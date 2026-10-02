import { PrismError, aborted, interruptible } from './errors.mjs';

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

export function modelFromLabel(label) {
  const value = label.trim().replace(/\s+/g, ' ');
  const match = /^(\d+(?:\.\d+)?) (Sol|Terra|Luna|Astra)\b/.exec(value);
  return match ? `gpt-${match[1]}-${match[2].toLowerCase()}` : null;
}

export class BrowserSession {
  constructor(browser, onHeartbeat, source, worker = 0) {
    this.browser = browser;
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
        context = await this.browser.newContext({ locale: 'en-US' });
        checkCurrent();
        this.context = context;
        installed = true;
        await context.addCookies([{ name: 'prism_oai_access_token', value: access_token,
          domain: 'prism.openai.com', path: '/', secure: true, httpOnly: true, sameSite: 'Lax' }]);
        checkCurrent();
        const page = await context.newPage();
        checkCurrent();
        this.page = page;
        const currentPage = () => this.contextEpoch === epoch && this.page === page;
        await page.route(`${origin}/**`, route => currentPage()
          ? this.route(route).catch(() => route.abort().catch(() => {})) : route.abort().catch(() => {}));
        checkCurrent();
        page.on('response', response => { if (currentPage()) this.observe(response).catch(() => {}); });
        page.on('close', () => { if (currentPage()) this.turn?.reject(new PrismError('browser_session_closed', 503)); });
        page.on('crash', () => { if (currentPage()) this.close().catch(() => {}); });
        await page.goto(origin + '/', { waitUntil: 'domcontentloaded', timeout: 25000 });
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
        return auth.actualUserId;
      } catch (error) {
        if (context && !installed) await context.close().catch(() => {});
        else if (context && this.context === context) await this.terminateContext();
        throw error;
      }
    }, signal, () => this.contextEpoch === epoch ? this.close() : undefined);
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
      turn.started = true;
      turn.startRequest = request;
      turn.conversationId = body.conversationId;
      this.audit('upstream_start', { model: turn.request.model, effort: turn.request.effort, ui_model: body.metadata.model,
        sentinel_present: Boolean(request.headers?.()['openai-sentinel-token']), input_roles: turn.request.input.map(item => item.role) });
      // The official UI still generates Sentinel proof, identity, and sandbox metadata.
      // Replace only validated text so no unrelated native UI history is sent. The UI's own model
      // and effort controls can sit on their loading defaults, so the exact requested values
      // replace them; select() has already confirmed the model is in Prism's catalog.
      return route.continue({ postData: JSON.stringify({ ...body, input: turn.request.input,
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
    // Our own status polls run next to the page's. A failed poll of ours only ends our polling; while
    // ours are healthy, a failed page poll is not fatal either. Prism reports real failures inside a
    // 200 response, and the other poller still sees the terminal state.
    const own = !isStart && Boolean(turn.ownBodies?.has(request.postData?.()));
    if (!response.ok() && !isStart && (own || (turn.ownPolling && !turn.ownPollFailed))) {
      if (own) turn.ownPollFailed = true;
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
      if (own) { turn.ownPollFailed = true; return; }
      throw error;
    }
    if (!matchesTurn()) return;
    if (data.request_id && turn.requestId && data.request_id !== turn.requestId) return;
    if (data.conversation_id && data.conversation_id !== turn.conversationId) return;
    if (data.request_id) turn.requestId = data.request_id;
    if (data.turn_state) turn.turnState = data.turn_state;
    if (data.conversation_id) turn.conversationId = data.conversation_id;
    if (!isStart && !own && !turn.statusTemplate && sent && typeof sent === 'object') {
      // The page's first good poll gives the exact body shape (diff_format etc.); ours copy it.
      turn.statusTemplate = sent;
      if (this.statusPollMs > 0 && turn.ownBodies) this.pollStatus(turn).catch(() => { turn.ownPollFailed = true; });
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
      failure_code: /^[a-z_]{1,100}$/.test(payload.reason || '') ? payload.reason : undefined,
      payload_http_status: Number.isInteger(payload.httpStatus) && payload.httpStatus >= 100 && payload.httpStatus <= 599
        ? payload.httpStatus : undefined,
      resubmission_requested: resubmissionRequested, completed_by: turn.completedBy,
      own_polls: turn.ownPolls, own_poll_failed: turn.ownPollFailed,
      exec_meta_shape: payloadShape(payload.codexExecMeta), debug_shape: payloadShape(payload.codexDebug) });
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

  async initialize(projectId, onProjectCreated, signal, previousModels = []) {
    const page = this.page;
    const epoch = this.contextEpoch;
    const checkCurrent = () => {
      aborted(signal);
      if (this.contextEpoch !== epoch || this.page !== page || !page || page.isClosed?.()) {
        throw new PrismError('browser_session_closed', 503);
      }
    };
    checkCurrent();
    this.bootstrapping = true;
    this.syncSeen = false;
    this.lastHeartbeat = 0;
    this.reserveProject = onProjectCreated;
    try {
      await interruptible(async () => {
        if (projectId) {
          if (!uuidPattern.test(projectId)) throw new PrismError('invalid_managed_project');
          this.projectId = projectId;
          await page.goto(`${origin}/?u=${encodeURIComponent(projectId)}&pg=1`,
            { waitUntil: 'domcontentloaded', timeout: 45000 });
          checkCurrent();
        } else {
          this.creating = true;
          const creation = page.waitForResponse(response => new URL(response.url()).pathname === '/api/projects' &&
            response.request().method() === 'POST', { timeout: 90000 });
          creation.catch(() => {});
          await page.goto(`${origin}/?n=1`, { waitUntil: 'domcontentloaded', timeout: 45000 });
          checkCurrent();
          const response = await creation;
          checkCurrent();
          const data = await response.json();
          checkCurrent();
          if (!response.ok() || data.uuid !== this.projectId) throw new PrismError('project_creation_failed');
        }
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
      }, signal, () => this.contextEpoch === epoch ? this.close() : undefined);
      return [...this.labels.keys()];
    } finally {
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
    await composer.waitFor({ state: 'visible', timeout: 30000 });
    checkCurrent();
    await page.waitForFunction(() => {
      const item = Array.from(document.querySelectorAll('textarea')).filter(element => element.getClientRects().length).at(-1);
      return item && !item.disabled;
    }, null, { timeout: 120000 });
    checkCurrent();
    return composer;
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
    const page = this.page;
    // onText is reserved for verified cumulative assistant text; pending progress is not that text.
    const turn = { request, signal, onText, started: false, submitAllowed: false,
      ownBodies: new Set(), ownPolls: 0, ownPolling: false, ownPollFailed: false, statusTemplate: null, completedBy: null };
    const checkCurrent = () => {
      aborted(signal);
      if (this.turn !== turn || this.page !== page || !page || page.isClosed?.()) {
        throw new PrismError('browser_session_closed', 503);
      }
    };
    const result = new Promise((resolve, reject) => { turn.resolve = resolve; turn.reject = reject; });
    // A native response can finish before press('Enter') returns.
    result.catch(() => {});
    this.turn = turn;
    let stage = 'new_chat';
    try {
      return await interruptible(async () => {
        checkCurrent();
        await page.getByRole('button', { name: 'New chat tab', exact: true }).click({ timeout: 15000 });
        checkCurrent();
        stage = 'composer';
        const composer = await this.composer(checkCurrent);
        checkCurrent();
        stage = 'model_selection';
        await this.select(request, checkCurrent);
        checkCurrent();
        stage = 'submit';
        await composer.fill('Process the submitted text.');
        checkCurrent();
        turn.submitAllowed = true;
        await composer.press('Enter');
        checkCurrent();
        stage = 'generation';
        return result;
      }, signal, () => this.page === page ? this.close() : undefined);
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
        else await this.stop();
      }
      throw error instanceof PrismError ? error : new PrismError(`browser_ui_${stage}_failed`);
    } finally {
      if (this.turn === turn) this.turn = null;
    }
  }

  // Polls status from the official page (its fetch carries the page's own verification) with the
  // newest turn_state, one request at a time, until the turn ends or a poll of ours fails.
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
          await response.text();
          return response.ok;
        } catch { return false; }
      }, { path: STATUS_PATH, body }).catch(() => false);
      if (!ok) turn.ownPollFailed = true;
      const wait = this.statusPollMs - (Date.now() - begun);
      if (wait > 0 && live()) await new Promise(resolve => setTimeout(resolve, wait));
    }
  }

  async stop() {
    const turn = this.turn;
    if (!turn?.started || turn.completed || !this.page || this.page.isClosed()) return;
    if (!turn.stopping) turn.stopping = this.stopTurn(turn);
    return turn.stopping;
  }

  async stopTurn(turn) {
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
    const context = this.context;
    this.context = null;
    this.page = null;
    this.creating = false;
    this.bootstrapping = false;
    this.reserveProject = null;
    this.turn?.reject(new PrismError('session_closed', 503));
    this.turn = null;
    if (context) await context.close().catch(() => {});
  }

  isAlive() { return Boolean(this.context && this.page && !this.page.isClosed()); }
}

export async function launchBrowser() {
  const { chromium } = await import('playwright');
  return chromium.launch({ headless: true,
    ...(process.env.PRISM_CHROMIUM_EXECUTABLE ? { executablePath: process.env.PRISM_CHROMIUM_EXECUTABLE } : {}) });
}
