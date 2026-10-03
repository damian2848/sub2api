import { PrismError, aborted, pause } from './errors.mjs';

export function multiplexEnabled(value = process.env.PRISM_MULTIPLEX_PAGES) {
  return ['true', '1', 'on'].includes(String(value ?? '').trim().toLowerCase());
}

// Experimental and deliberately account-bound. Submission contexts remain isolated (and retain
// their cache) while one small resident document polls that account's accepted turns. This is NOT
// shared-context multiplexing: contexts are retained, so only editor-page memory/loading is saved.
export class AccountPageMultiplexer {
  constructor({ origin = 'https://prism.openai.com', admissionGuard, metrics, pollMs = 1000,
    onAudit = () => {}, healthMs = 30000, now = Date.now } = {}) {
    this.origin = new URL(origin).origin;
    this.admissionGuard = admissionGuard;
    this.metrics = metrics;
    this.pollMs = Math.max(250, pollMs || 1000);
    this.onAudit = onAudit;
    this.healthMs = Math.min(30000, Math.max(250, healthMs)); this.now = now;
    this.sessions = new Set(); this.healthTimer = null; this.healthProbe = null;
    this.context = null; this.page = null; this.identity = null; this.registration = null;
    this.jobs = new Map(); this.waiters = []; this.submitting = false; this.closed = false;
  }
  async register(browser, credentials, identity, session) {
    if (this.closed) throw new PrismError('browser_session_closed', 503);
    if (this.identity && this.identity !== identity) throw new PrismError('source_identity_changed', 409);
    if (this.registration) await this.registration;
    if (this.identity && this.identity !== identity) throw new PrismError('source_identity_changed', 409);
    if (this.page && !this.page.isClosed()) {
      await this.context.addCookies([this.cookie(credentials.access_token)]);
      if (session) this.sessions.add(session);
      return;
    }
    this.registration = (async () => {
      await this.admissionGuard?.assertAdmission('resident_context');
      const context = await browser.newContext({ locale: 'en-US', serviceWorkers: 'block' });
      try {
        await context.addCookies([this.cookie(credentials.access_token)]);
        const page = await context.newPage();
        // Prism refuses model API calls (403, no error code) that were not made by ITS OWN page script: the
        // editor wraps window.fetch and attaches the Sentinel proof. A bare JSON document such as
        // /auth/session has no wrapper, so a poll from it is refused. Load the real site instead and wait
        // until the official wrapper and Sentinel SDK are in place; the resident page is never used for
        // anything else, so it never opens an editor tab or submits a prompt.
        await page.addInitScript(() => { window.__prismOriginalFetch = window.fetch; });
        await page.goto(`${this.origin}/`, { waitUntil: 'domcontentloaded', timeout: 25000 });
        try {
          await page.waitForFunction(() => Boolean(window.__prismOriginalFetch) && window.fetch !== window.__prismOriginalFetch
            && window.SentinelSDK && typeof window.SentinelSDK.token === 'function', undefined, { timeout: 30000 });
        } catch { throw new PrismError('poll_carrier_unavailable', 503); }
        if (this.closed) throw new PrismError('browser_session_closed', 503);
        this.context = context; this.page = page; this.identity = identity;
        page.on('crash', () => this.close().catch(() => {}));
        page.on('close', () => { if (this.page === page) this.failJobs(new PrismError('browser_session_closed', 503)); });
      } catch (error) { await context.close().catch(() => {}); throw error; }
    })();
    try {
      await this.registration;
      if (session) this.sessions.add(session);
      if (!this.healthTimer) {
        this.healthTimer = setInterval(() => this.probeHealth().catch(() => {}), this.healthMs);
        this.healthTimer.unref?.();
      }
    } finally { this.registration = null; }
  }
  unregister(session) { this.sessions.delete(session); }
  markHealthy(session) {
    if (!session || !session.context || session.page) return;
    const stamp = Math.floor(this.now() / 1000);
    session.lastHeartbeat = stamp;
    session.onHeartbeat(stamp);
  }
  async probeHealth() {
    if (this.healthProbe) return this.healthProbe;
    if (!this.isAlive() || !this.sessions.size) return;
    this.healthProbe = (async () => {
      const result = await this.page.evaluate(async () => {
        try {
          const response = await window.fetch('/auth/session', { cache: 'no-store', credentials: 'same-origin', signal: AbortSignal.timeout(15000) });
          const data = await response.json();
          return { ok: response.ok, status: response.status, id: data.user?.id, anonymous: data.user?.is_anonymous };
        } catch { return { ok: false, status: 0 }; }
      }).catch(() => ({ ok: false, status: 0 }));
      if (result.ok && result.id === this.identity && !result.anonymous) {
        // Actual authenticated network success, not a clock-only synthetic heartbeat. Keep idle
        // detached workers eligible after 60s; they rebuild their editor on the next request.
        for (const session of this.sessions) this.markHealthy(session);
      } else if ([401, 403].includes(result.status) || result.ok && result.id !== this.identity) {
        for (const session of this.sessions) if (!session.page) session.onHeartbeat(0, 'session_expired');
        this.failJobs(new PrismError('session_expired', 401));
      }
      this.onAudit('resident_health', { healthy: Boolean(result.ok && result.id === this.identity && !result.anonymous),
        status: result.status, sessions: this.sessions.size });
    })();
    try { await this.healthProbe; } finally { this.healthProbe = null; }
  }
  cookie(token) {
    const target = new URL(this.origin);
    return { name: 'prism_oai_access_token', value: token, domain: target.hostname, path: '/',
      secure: target.protocol === 'https:', httpOnly: true, sameSite: 'Lax' };
  }
  isAlive() { return Boolean(!this.closed && this.context && this.page && !this.page.isClosed()); }
  acquireSubmission(signal) {
    aborted(signal);
    if (this.closed) return Promise.reject(new PrismError('browser_session_closed', 503));
    const begun = performance.now();
    return new Promise((resolve, reject) => {
      const waiter = { signal, resolve, reject, begun, abort: null };
      waiter.abort = () => { const index = this.waiters.indexOf(waiter); if (index >= 0) this.waiters.splice(index, 1);
        try { aborted(signal); } catch (error) { reject(error); } };
      signal?.addEventListener('abort', waiter.abort, { once: true });
      this.waiters.push(waiter); this.drain();
    });
  }
  drain() {
    if (this.submitting || this.closed) return;
    const waiter = this.waiters.shift(); if (!waiter) return;
    waiter.signal?.removeEventListener('abort', waiter.abort);
    if (waiter.signal?.aborted) { waiter.abort(); this.drain(); return; }
    this.submitting = true;
    this.metrics?.record('submission_queue_wait', performance.now() - waiter.begun, { multiplex: true });
    let released = false;
    waiter.resolve(() => { if (released) return; released = true; this.submitting = false; this.drain(); });
  }
  startPolling(session, turn) {
    if (!this.isAlive()) throw new PrismError('browser_session_closed', 503);
    if (this.jobs.has(turn)) return;
    this.jobs.set(turn, { session, cancelled: false });
    turn.ownPolling = true;
    this.poll(session, turn).catch(error => { if (session.turn === turn && !turn.completed && !turn.signal?.aborted) turn.reject(error); })
      .finally(() => this.jobs.delete(turn));
  }
  async fetch(path, body, timeoutMs = 30000) {
    if (!this.isAlive()) throw new PrismError('browser_session_closed', 503);
    return this.page.evaluate(async ({ path, body, timeoutMs }) => {
      try {
        // window.fetch is the official wrapper (see register); it adds the verification Prism requires.
        const response = await window.fetch(path, { method: 'POST', credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' }, body, signal: AbortSignal.timeout(timeoutMs) });
        return { status: response.status, text: await response.text() };
      } catch { return { status: 0, text: '' }; }
    }, { path, body, timeoutMs });
  }
  // The headers the resident fetch above sets itself. The browser adds its own (host, cookie, user-agent,
  // sec-fetch-*, ...) to both pages alike, so only the page script's explicit headers can differ.
  static RESIDENT_SET_HEADERS = ['content-type'];
  // Why did the resident page get refused? The editor page's own poll is accepted for the same turn,
  // so the difference matters. Only fixed, non-secret facts are recorded: the status, Prism's error
  // code (never its message or body), and which cookie NAMES the resident context holds.
  async diagnoseRefusal(path, result, turn) {
    let code;
    try {
      const parsed = JSON.parse(result.text);
      const value = parsed?.error?.code ?? parsed?.code ?? parsed?.detail?.code;
      if (typeof value === 'string' && /^[A-Za-z0-9_.-]{1,64}$/.test(value)) code = value;
    } catch {}
    let cookies;
    try { cookies = (await this.context?.cookies?.(this.origin) ?? []).map(item => item.name).filter(name => /^[A-Za-z0-9_.-]{1,64}$/.test(name)).sort(); }
    catch {}
    // What the editor page's accepted poll carried that the resident fetch does not set itself. The
    // browser-managed headers are common to both, so a name listed here is a candidate for the refusal.
    const native = Array.isArray(turn?.nativeStatusHeaderNames) ? turn.nativeStatusHeaderNames : undefined;
    const browserManaged = new Set(['host', 'connection', 'content-length', 'accept-encoding', 'accept-language', 'user-agent',
      'cookie', 'origin', 'referer', 'accept', 'priority', 'sec-ch-ua', 'sec-ch-ua-mobile', 'sec-ch-ua-platform',
      'sec-fetch-site', 'sec-fetch-mode', 'sec-fetch-dest']);
    const editorOnly = native?.filter(name => !AccountPageMultiplexer.RESIDENT_SET_HEADERS.includes(name) && !browserManaged.has(name));
    this.onAudit('resident_poll_refused', { path: path.split('?')[0], status: result.status, code,
      resident_cookie_names: cookies, resident_url_origin: this.page?.url?.().startsWith(this.origin) ? 'same_origin' : 'other',
      editor_poll_header_names: native, editor_only_header_names: editorOnly,
      resident_set_header_names: AccountPageMultiplexer.RESIDENT_SET_HEADERS });
  }
  async poll(session, turn) {
    const job = this.jobs.get(turn);
    const live = () => this.jobs.get(turn) === job && !job.cancelled && session.turn === turn &&
      !turn.completed && !turn.signal?.aborted && this.isAlive();
    while (live()) {
      const begun = performance.now();
      const body = JSON.stringify({ ...turn.statusTemplate, request_id: turn.requestId, turn_state: turn.turnState });
      turn.ownBodies.add(body); turn.ownPolls += 1;
      const result = await this.fetch('/api/llm/response_with_tools_status', body);
      if (!live()) break;
      let valid = false;
      if (result.status >= 200 && result.status < 300) { try { JSON.parse(result.text); valid = true; } catch {} }
      if (!valid) {
        turn.ownPollErrors += 1; turn.ownPollErrorTotal += 1;
        if (!turn.refusalDiagnosed) { turn.refusalDiagnosed = true; await this.diagnoseRefusal('/api/llm/response_with_tools_status', result, turn).catch(() => {}); }
        if ([401, 403].includes(result.status) || turn.ownPollErrors >= 3) {
          turn.ownPollFailed = true;
          const error = new PrismError([401, 403].includes(result.status) ? 'session_expired' : 'prism_upstream_http_error',
            [401, 403].includes(result.status) ? 401 : 502);
          error.transient = result.status >= 500 && result.status <= 599;
          throw error;
        }
      } else {
        this.markHealthy(session);
        const request = { url: () => `${this.origin}/api/llm/response_with_tools_status`, method: () => 'POST',
          postData: () => body, postDataJSON: () => JSON.parse(body) };
        await session.observe({ url: request.url, request: () => request, ok: () => true, status: () => result.status,
          json: async () => JSON.parse(result.text) });
      }
      const wait = turn.ownPollErrors ? Math.min(8000, this.pollMs * 2 ** turn.ownPollErrors) : this.pollMs - (performance.now() - begun);
      if (wait > 0 && live()) { try { await pause(wait, turn.signal); } catch { break; } }
    }
  }
  async stop(turn) {
    if (!turn.requestId || !this.isAlive()) return false;
    const result = await this.fetch('/api/llm/response_with_tools_stop', JSON.stringify({ request_id: turn.requestId,
      conversation_id: turn.conversationId, turn_state: turn.turnState }), 10000).catch(() => ({ status: 0 }));
    return result.status >= 200 && result.status < 300;
  }
  cancel(turn) { const job = this.jobs.get(turn); if (job) job.cancelled = true; this.jobs.delete(turn); }
  failJobs(error) { for (const [turn] of this.jobs) turn.reject(error); this.jobs.clear(); }
  async close() {
    this.closed = true;
    clearInterval(this.healthTimer); this.healthTimer = null;
    this.sessions.clear();
    this.failJobs(new PrismError('browser_session_closed', 503));
    for (const waiter of this.waiters.splice(0)) { waiter.signal?.removeEventListener('abort', waiter.abort); waiter.reject(new PrismError('browser_session_closed', 503)); }
    const context = this.context; this.context = null; this.page = null;
    await context?.close().catch(() => {});
  }
  status() { return { resident_alive: this.isAlive(), in_flight: this.jobs.size, submission_busy: this.submitting,
    submission_queued: this.waiters.length, isolation: 'independent_contexts' }; }
}
