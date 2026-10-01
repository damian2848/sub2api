import { PrismError, aborted, interruptible } from './errors.mjs';

const origin = 'https://prism.openai.com';
const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

// While Statsig (the prism_codex_models dynamic config) is still loading, the model menu
// shows a fallback list of only this model. Such a result must not replace a larger catalog.
export const FALLBACK_MODEL = 'gpt-5.6-sol';
export function catalogCollapsed(collected, previous = []) {
  return collected.length === 1 && collected[0] === FALLBACK_MODEL && previous.length > 1;
}

export function modelFromLabel(label) {
  const value = label.trim().replace(/\s+/g, ' ');
  const match = /^(\d+(?:\.\d+)?) (Sol|Terra|Luna|Astra)\b/.exec(value);
  return match ? `gpt-${match[1]}-${match[2].toLowerCase()}` : null;
}

export class BrowserSession {
  constructor(browser, onHeartbeat, source) {
    this.browser = browser;
    this.onHeartbeat = onHeartbeat;
    this.context = null;
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
  }

  audit(event, details) {
    if (process.env.PRISM_AUDIT_REQUESTS === 'true') console.log(JSON.stringify({ event, source: this.source, ...details }));
  }

  async authenticate({ access_token, expected_email, expected_user_id }, signal) {
    this.context = await this.browser.newContext({ locale: 'en-US' });
    await this.context.addCookies([{ name: 'prism_oai_access_token', value: access_token,
      domain: 'prism.openai.com', path: '/', secure: true, httpOnly: true, sameSite: 'Lax' }]);
    this.page = await this.context.newPage();
    await this.page.route(`${origin}/**`, route => this.route(route).catch(() => route.abort().catch(() => {})));
    this.page.on('response', response => this.observe(response).catch(() => {}));
    this.page.on('close', () => this.turn?.reject(new PrismError('browser_session_closed', 503)));
    this.page.on('crash', () => this.close().catch(() => {}));
    return interruptible(async () => {
      await this.page.goto(origin + '/', { waitUntil: 'domcontentloaded', timeout: 25000 });
      const auth = await this.page.evaluate(async expected => {
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
      if (!auth.signedIn) throw new PrismError('oauth_session_rejected', 401);
      if (!auth.emailMatches || !auth.idMatches) throw new PrismError('oauth_identity_mismatch', 403);
      return auth.actualUserId;
    }, signal, () => this.close());
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
      const body = request.postDataJSON();
      const mismatch = body?.metadata?.projectId !== this.projectId ? 'browser_project_mismatch' :
        body.metadata.model !== turn.request.model ? 'browser_model_selection_mismatch' :
        body.metadata.reasoning_effort !== turn.request.effort ? 'browser_effort_selection_mismatch' :
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
      this.audit('upstream_start', { model: body.metadata.model, effort: body.metadata.reasoning_effort,
        sentinel_present: Boolean(request.headers?.()['openai-sentinel-token']), input_roles: turn.request.input.map(item => item.role) });
      // The official UI still generates Sentinel proof, identity, and sandbox metadata.
      // Replace only validated text so no unrelated native UI history is sent.
      return route.continue({ postData: JSON.stringify({ ...body, input: turn.request.input }) });
    }
    if (path.startsWith('/api/llm/') && !this.turn && !path.endsWith('_stop')) return route.abort();
    return route.continue();
  }

  async observe(response) {
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
      this.syncSeen = data.status === 'synced';
      return;
    }
    if (!['/api/llm/response_with_tools_start', '/api/llm/response_with_tools_status'].includes(path)) return;
    const turn = this.turn;
    if (!turn?.started) return;
    const request = response.request();
    const sent = request.postDataJSON();
    const isStart = path === '/api/llm/response_with_tools_start';
    // Old tabs can finish polling after a new chat has started, including HTTP errors.
    const matchesTurn = () => this.turn === turn && !turn.completed && (isStart
      ? request === turn.startRequest && sent?.conversationId === turn.conversationId
      : Boolean(turn.requestId && sent?.request_id === turn.requestId &&
        (!sent.conversation_id || sent.conversation_id === turn.conversationId)));
    if (!matchesTurn()) return;
    if (isStart) {
      this.audit('upstream_start_actual_input', { input_roles: (sent?.input || []).map(item => item.role) });
    }
    if (!response.ok()) {
      this.audit('upstream_http_error', { status: response.status() });
      return turn.reject(new PrismError([401, 403].includes(response.status())
        ? 'session_expired' : 'prism_upstream_http_error', [401, 403].includes(response.status()) ? 401 : 502));
    }
    const data = await response.json();
    if (!matchesTurn()) return;
    if (data.request_id && turn.requestId && data.request_id !== turn.requestId) return;
    if (data.conversation_id && data.conversation_id !== turn.conversationId) return;
    if (data.request_id) turn.requestId = data.request_id;
    if (data.turn_state) turn.turnState = data.turn_state;
    if (data.conversation_id) turn.conversationId = data.conversation_id;
    if (!['completed', 'error', 'failed'].includes(data.status)) return;
    turn.completed = true;
    this.audit('upstream_result', { status: data.response?.status,
      reported_model: data.response?.payload?.model || null,
      failure_code: /^[a-z_]{1,100}$/.test(data.response?.payload?.reason || '') ? data.response.payload.reason : undefined });
    if (data.response?.status !== 'success') return turn.reject(new PrismError('prism_generation_failed'));
    const payload = data.response.payload || {};
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
    this.bootstrapping = true;
    this.syncSeen = false;
    this.lastHeartbeat = 0;
    this.reserveProject = onProjectCreated;
    try {
      await interruptible(async () => {
        if (projectId) {
          if (!uuidPattern.test(projectId)) throw new PrismError('invalid_managed_project');
          this.projectId = projectId;
          await this.page.goto(`${origin}/?u=${encodeURIComponent(projectId)}&pg=1`,
            { waitUntil: 'domcontentloaded', timeout: 45000 });
        } else {
          this.creating = true;
          const creation = this.page.waitForResponse(response => new URL(response.url()).pathname === '/api/projects' &&
            response.request().method() === 'POST', { timeout: 90000 });
          creation.catch(() => {});
          await this.page.goto(`${origin}/?n=1`, { waitUntil: 'domcontentloaded', timeout: 45000 });
          const response = await creation;
          const data = await response.json();
          if (!response.ok() || data.uuid !== this.projectId) throw new PrismError('project_creation_failed');
        }
        await this.composer();
        const deadline = Date.now() + 120000;
        while ((!this.syncSeen || !this.lastHeartbeat) && Date.now() < deadline) {
          aborted(signal);
          await this.page.waitForTimeout(250);
        }
        if (!this.syncSeen || !this.lastHeartbeat) throw new PrismError('sandbox_initialization_timeout', 504);
        this.labels = await this.catalog(previousModels, signal);
        if (!this.labels.size) throw new PrismError('model_catalog_unavailable');
      }, signal, () => this.close());
      return [...this.labels.keys()];
    } finally {
      this.creating = false;
      this.bootstrapping = false;
      this.reserveProject = null;
    }
  }

  // Waits (read-only) until the Statsig client reports Ready; its absence is tolerated.
  async waitForStatsig(signal, timeoutMs = 20000, pollMs = 250) {
    const deadline = Date.now() + timeoutMs;
    for (;;) {
      aborted(signal);
      const status = await this.page.evaluate(() => window.__STATSIG__?.firstInstance?.loadingStatus ?? null)
        .catch(() => null);
      if (status === 'Ready') return true;
      if (Date.now() >= deadline) {
        this.audit('statsig_wait_timeout', { status });
        return false;
      }
      await this.page.waitForTimeout(pollMs);
    }
  }

  async readModelMenu() {
    await this.thinking().click();
    await this.page.getByRole('menuitem', { name: /^(Model|模型)/ }).hover();
    await this.page.getByRole('menuitem').filter({ hasText: /^\d+(?:\.\d+)? (?:Sol|Terra|Luna|Astra)\b/ }).first()
      .waitFor({ state: 'visible', timeout: 15000 });
    const observed = new Map();
    const collect = async () => {
      const items = await this.page.getByRole('menuitem').evaluateAll(elements => elements.map(element => ({
        label: element.textContent.trim(), disabled: element.hasAttribute('data-disabled') ||
          element.getAttribute('aria-disabled') === 'true', visible: Boolean(element.getClientRects().length) })));
      for (const item of items) observed.set(item.label, { ...item,
        visible: item.visible || observed.get(item.label)?.visible === true });
    };
    await collect();
    const menu = this.page.getByRole('menu').last();
    for (const fraction of [0.5, 1]) {
      await menu.evaluate((element, offset) => { element.scrollTop = element.scrollHeight * offset; }, fraction);
      await this.page.waitForTimeout(200);
      await collect();
    }
    await menu.evaluate(element => { element.scrollTop = 0; });
    this.audit('model_menu', { items: [...observed.values()] });
    const labels = new Map();
    for (const item of observed.values()) {
      const id = modelFromLabel(item.label);
      if (id && !item.disabled && item.visible) labels.set(id, item.label);
    }
    await this.page.keyboard.press('Escape');
    await this.page.keyboard.press('Escape');
    return labels;
  }

  // The menu is only trusted once Statsig is Ready. If it still collapses to the fallback
  // list although the persisted catalog was larger, read it once more and accept that result.
  async catalog(previousModels, signal) {
    await this.waitForStatsig(signal);
    let labels = await this.readModelMenu();
    if (catalogCollapsed([...labels.keys()], previousModels)) {
      this.audit('model_catalog_collapsed', { previous_count: previousModels.length });
      await this.page.waitForTimeout(3000);
      aborted(signal);
      labels = await this.readModelMenu();
    }
    return labels;
  }

  thinking() { return this.page.getByRole('button', { name: /^(Thinking|思考中):/ }); }

  async composer() {
    const composer = this.page.locator('textarea:visible').last();
    await composer.waitFor({ state: 'visible', timeout: 30000 });
    await this.page.waitForFunction(() => {
      const item = Array.from(document.querySelectorAll('textarea')).filter(element => element.getClientRects().length).at(-1);
      return item && !item.disabled;
    }, null, { timeout: 120000 });
    return composer;
  }

  async select(request) {
    const label = this.labels.get(request.model);
    if (!label) throw new PrismError('model_not_available', 400, 'model');
    await this.thinking().click();
    await this.page.getByRole('menuitem', { name: /^(Model|模型)/ }).hover();
    await this.page.getByRole('menuitem', { name: label, exact: true }).click();
    await this.thinking().click();
    await this.page.getByRole('menuitem', { name: /^(Effort|Reasoning effort|推理强度)/ }).hover();
    const effortLabel = request.effort[0].toUpperCase() + request.effort.slice(1);
    await this.page.getByRole('menuitem', { name: effortLabel, exact: true }).click();
  }

  async generate(request, signal) {
    aborted(signal);
    if (this.turn) throw new PrismError('account_busy', 409);
    const turn = { request, signal, started: false };
    const result = new Promise((resolve, reject) => { turn.resolve = resolve; turn.reject = reject; });
    // A native response can finish before press('Enter') returns.
    result.catch(() => {});
    this.turn = turn;
    let stage = 'new_chat';
    try {
      return await interruptible(async () => {
        await this.page.getByRole('button', { name: 'New chat tab', exact: true }).click({ timeout: 15000 });
        stage = 'composer';
        const composer = await this.composer();
        stage = 'model_selection';
        await this.select(request);
        stage = 'submit';
        await composer.fill('Process the submitted text.');
        await composer.press('Enter');
        stage = 'generation';
        return result;
      }, signal, () => this.stop());
    } catch (error) {
      this.audit('browser_ui_failure', { stage, error_type: error.constructor.name,
        code: error instanceof PrismError ? error.code : undefined });
      if (process.env.PRISM_AUDIT_REQUESTS === 'true' && this.page && !this.page.isClosed()) {
        const composers = await this.page.locator('textarea').evaluateAll(elements => elements.map(element => ({
          visible: Boolean(element.getClientRects().length), disabled: element.disabled }))).catch(() => []);
        const thinking = await this.thinking().allTextContents().catch(() => []);
        this.audit('browser_ui_state', { composers, thinking });
      }
      await this.stop();
      throw error instanceof PrismError ? error : new PrismError(`browser_ui_${stage}_failed`);
    } finally {
      this.turn = null;
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
    const context = this.context;
    this.context = null;
    this.page = null;
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
