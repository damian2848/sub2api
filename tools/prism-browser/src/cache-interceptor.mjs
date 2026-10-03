// Opt-in: the default keeps the Playwright route guard that production runs today. The CDP path
// (static-asset caching) is only validated against a local mock until it is canaried on a real account.
export function httpCacheEnabled(value = process.env.PRISM_HTTP_CACHE) {
  return ['true', '1', 'on'].includes(String(value ?? '').trim().toLowerCase());
}

// Playwright page.route/context.route disable HTTP cache for the context, even with a narrow glob.
// Chromium CDP Fetch preserves cache for non-intercepted static assets. All same-origin API calls
// remain subject to the existing BrowserSession.route guard (projects, native uploads, and starts).
export async function installCachePreservingInterceptor(context, page, { origin, route, observe, current = () => true,
  onMode = () => {}, enabled = true }) {
  if (!enabled || typeof context.newCDPSession !== 'function') {
    await page.route(`${origin}/**`, item => current() ? route(item).catch(() => item.abort().catch(() => {})) : item.abort().catch(() => {}));
    page.on('response', response => { if (current()) observe(response).catch(() => {}); });
    onMode('playwright_route_cache_disabled');
    return null;
  }
  // Preserve the old all-response observer for traffic outside the CDP watched endpoints.
  // Only watched native starts/status use synthetic request identity; observing those twice
  // would claim an unmodified Playwright body or duplicate acceptance callbacks.
  const watchedResponse = url => {
    try {
      const target = new URL(url);
      return target.origin === origin && (target.pathname.startsWith('/api/llm/') ||
        /^\/api\/.*\/(?:heartbeat|wait-for-sync)/.test(target.pathname));
    } catch { return false; }
  };
  page.on?.('response', response => {
    if (current() && !watchedResponse(response.url())) observe(response).catch(() => {});
  });
  const cdp = await context.newCDPSession(page);
  const requests = new Map();
  const fetchIds = new Map();
  cdp.on('Fetch.requestPaused', async event => {
    if (event.responseStatusCode !== undefined || event.responseErrorReason) {
      // Read watched API response bodies at Fetch's response stage: fetch consumers need not
      // drain them before Chromium fires loadingFinished (heartbeat often only checks .ok).
      const wrapped = requests.get(event.networkId) || requests.get(`fetch:${event.requestId}`);
      let text = '';
      try {
        const body = await cdp.send('Fetch.getResponseBody', { requestId: event.requestId });
        text = body.base64Encoded ? Buffer.from(body.body, 'base64').toString('utf8') : body.body;
      } catch {}
      try { await cdp.send('Fetch.continueRequest', { requestId: event.requestId }); } catch { return; }
      if (event.networkId) requests.delete(event.networkId);
      requests.delete(`fetch:${event.requestId}`);
      if (wrapped && current()) {
        const status = event.responseStatusCode || 0;
        try { await observe({ url: wrapped.url, request: () => wrapped, status: () => status,
          ok: () => status >= 200 && status < 300, json: async () => JSON.parse(text) }); } catch {}
      }
      return;
    }
    const request = event.request;
    const headers = Object.fromEntries(Object.entries(request.headers || {}).map(([key, value]) => [key.toLowerCase(), String(value)]));
    const wrapped = { url: () => request.url, method: () => request.method, headers: () => headers,
      postData: () => request.postData ?? null, postDataJSON: () => request.postData ? JSON.parse(request.postData) : null };
    if (event.networkId) { requests.set(event.networkId, wrapped); fetchIds.set(event.networkId, event.requestId); }
    requests.set(`fetch:${event.requestId}`, wrapped);
    let settled = false;
    const adapter = { request: () => wrapped,
      async abort() { if (settled) return; settled = true; await cdp.send('Fetch.failRequest', { requestId: event.requestId, errorReason: 'Aborted' }); },
      async continue(options = {}) {
        if (settled) return; settled = true;
        // Network observation must see the body actually sent, not the placeholder composed by UI.
        if (options.postData !== undefined) request.postData = options.postData;
        await cdp.send('Fetch.continueRequest', { requestId: event.requestId,
          ...(options.postData !== undefined ? { postData: Buffer.from(options.postData).toString('base64') } : {}) });
      } };
    try { if (current()) await route(adapter); else await adapter.abort(); }
    catch { await adapter.abort().catch(() => {}); }
  });
  const forget = event => {
    requests.delete(event.requestId);
    const fetchId = fetchIds.get(event.requestId);
    if (fetchId) requests.delete(`fetch:${fetchId}`);
    fetchIds.delete(event.requestId);
  };
  cdp.on('Network.loadingFinished', forget);
  cdp.on('Network.loadingFailed', forget);
  await cdp.send('Network.enable');
  await cdp.send('Network.setCacheDisabled', { cacheDisabled: false });
  await cdp.send('Fetch.enable', { patterns: [{ urlPattern: `${origin}/api/*`, requestStage: 'Request' },
    ...['/api/llm/*', '/api/*/heartbeat*', '/api/*/wait-for-sync*'].map(path =>
      ({ urlPattern: origin + path, requestStage: 'Response' }))] });
  onMode('cdp_fetch_static_cache');
  return cdp;
}
