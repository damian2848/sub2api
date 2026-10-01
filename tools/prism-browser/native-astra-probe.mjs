import { readFileSync, writeFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { createRequire } from 'node:module';
import { BrowserSession } from './src/browser.mjs';

const { chromium } = createRequire(import.meta.url)(process.env.PRISM_PROBE_PLAYWRIGHT || '/app/node_modules/playwright');
const input = JSON.parse(readFileSync(0, 'utf8'));
const effort = process.argv.find(value => value.startsWith('--effort='))?.slice(9) || 'low';
if (!['low', 'medium', 'high'].includes(effort)) throw new Error('invalid_effort');
const metadata = JSON.parse(readFileSync('/data/32.json', 'utf8'));
const browser = await chromium.launch({ headless: true });
const driver = new BrowserSession(browser, () => {}, '32');
const controller = new AbortController();
const deadline = setTimeout(() => controller.abort(), 210000);
const report = { checked_at: new Date().toISOString(), source_account: 32, production_modified: false,
  requested_model: 'gpt-6-astra', reasoning_effort: effort, model_starts: 0, no_fallback: true };
let phase = 'authentication';
let probing = false;
try {
  const userId = await driver.authenticate({ access_token: input.access_token,
    ...(input.expected_emails?.length ? { expected_email: input.expected_emails[0] } :
      { expected_user_id: input.expected_ids[0] }) }, controller.signal);
  const userHash = createHash('sha256').update(userId).digest('hex');
  if (userHash !== metadata.identity_hash) throw new Error('identity_mismatch');
  report.identity_verified = true;
  phase = 'existing_project';
  report.ui_catalog = await driver.initialize(metadata.project_id, () => { throw new Error('unexpected_project_create'); }, controller.signal);
  let capture;
  const captured = new Promise(resolve => { capture = resolve; });
  await driver.page.route('https://prism.openai.com/api/llm/**', async route => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    if (path.endsWith('/response_with_tools_start')) {
      const body = request.postDataJSON();
      if (!probing) { capture(body); return route.abort(); }
      if (report.model_starts || body.metadata?.model !== 'gpt-6-astra' || body.metadata?.projectId !== metadata.project_id) return route.abort();
      report.model_starts += 1;
      report.start_model = body.metadata.model;
      report.start_sentinel_present = Boolean(request.headers()['openai-sentinel-token']);
    }
    return route.continue();
  });
  phase = 'native_metadata';
  await driver.page.getByRole('button', { name: 'New chat tab', exact: true }).click();
  const composer = await driver.composer();
  await driver.select({ model: report.ui_catalog[0], effort });
  await composer.fill('Reply with only READY. Do not edit files or use tools.');
  await composer.press('Enter');
  const native = await captured;
  report.native_shape = { top_level_keys: Object.keys(native).sort(), input_type: Array.isArray(native.input) ? 'array' : typeof native.input,
    input_items: Array.isArray(native.input) ? native.input.map(item => ({ keys: Object.keys(item).sort(),
      role: item.role, type: item.type, content_type: Array.isArray(item.content) ? 'array' : typeof item.content,
      content_items: Array.isArray(item.content) ? item.content.map(part => ({ keys: Object.keys(part).sort(), type: part.type })) : [] })) : [],
    metadata_keys: Object.keys(native.metadata || {}).sort() };
  const shape = (value, depth = 0) => {
    if (depth > 3) return typeof value;
    if (Array.isArray(value)) return value.slice(0, 3).map(item => shape(item, depth + 1));
    if (value && typeof value === 'object') return Object.fromEntries(Object.entries(value).slice(0, 32)
      .map(([key, item]) => [/^[a-zA-Z_][a-zA-Z0-9_]{0,63}$/.test(key) ? key : '[dynamic_key]', shape(item, depth + 1)]));
    return value === null ? 'null' : typeof value;
  };
  report.native_shape.metadata_embedded_shapes = {};
  for (const field of ['codex_listen_snapshot', 'proxy_request_debug']) {
    try { report.native_shape.metadata_embedded_shapes[field] = shape(JSON.parse(native.metadata[field])); }
    catch { report.native_shape.metadata_embedded_shapes[field] = typeof native.metadata[field]; }
  }
  if (!native.metadata?.sandbox_url || !native.metadata?.sandbox_token || native.metadata.projectId !== metadata.project_id) {
    throw new Error('native_sandbox_metadata_missing');
  }
  probing = true;
  phase = 'astra_request';
  report.result = await driver.page.evaluate(async ({ native, effort }) => {
    const summary = {};
    let requestId;
    let turnState;
    let completed = false;
    const call = async (path, body, timeout) => {
      const response = await window.fetch(path, { method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body), signal: AbortSignal.timeout(timeout) });
      return { status: response.status, data: await response.json().catch(() => ({})) };
    };
    try {
      const start = await call('/api/llm/response_with_tools_start', { ...native,
        input: [{ type: 'message', role: 'user', content: [{ type: 'input_text',
          text: 'Reply with only READY. Do not edit files or use tools.' }] }],
        metadata: { ...native.metadata, model: 'gpt-6-astra', reasoning_effort: effort } }, 120000);
      summary.start_http_status = start.status;
      let data = start.data;
      requestId = data.request_id;
      turnState = data.turn_state;
      if (start.status !== 200) {
        summary.verification_failed = data.error === 'Request verification failed' || data.error === 'Request verification unavailable';
        return summary;
      }
      const expires = Date.now() + 150000;
      while (!['completed', 'error', 'failed'].includes(data.status) && Date.now() < expires) {
        if (!requestId || !turnState) { summary.invalid_start_payload = true; return summary; }
        await new Promise(resolve => setTimeout(resolve, 1000));
        const next = await call('/api/llm/response_with_tools_status', { request_id: requestId, turn_state: turnState,
          diff_format: 'structured_v1' }, Math.min(expires - Date.now(), 30000));
        summary.last_status_http_status = next.status;
        if (next.status !== 200) return summary;
        data = next.data;
        if (data.turn_state) turnState = data.turn_state;
      }
      completed = ['completed', 'error', 'failed'].includes(data.status);
      const payload = data.response?.payload || {};
      const text = (payload.output || []).filter(item => item.type === 'message')
        .flatMap(item => item.content || []).map(item => item.text || '').join('');
      summary.completed = completed;
      summary.response_status = data.response?.status;
      summary.output_matches_ready = data.response?.status === 'success' && text.trim() === 'READY';
      summary.reported_model = typeof payload.model === 'string' && /^gpt-[a-z0-9.-]+$/.test(payload.model) ? payload.model : null;
      summary.reason = typeof payload.reason === 'string' && /^[a-z_]{1,100}$/.test(payload.reason) ? payload.reason : undefined;
      summary.upstream_http_status = typeof payload.httpStatus === 'number' ? payload.httpStatus : undefined;
      summary.payload_keys = Object.keys(payload).sort();
      const debugBody = payload.codexRequestDebug?.error?.bodyText;
      if (typeof debugBody === 'string' || typeof payload.message === 'string' || typeof payload.rootCause === 'string') {
        let structured;
        if (typeof debugBody === 'string') try { const parsed = JSON.parse(debugBody); structured = parsed.error || parsed; } catch {}
        summary.debug_body_json = Boolean(structured);
        for (const field of ['code', 'type', 'param']) {
          const value = structured?.[field];
          if (typeof value === 'string' && /^[a-z][a-z0-9_.[\]-]{0,80}$/.test(value)) summary[`upstream_error_${field}`] = value;
        }
        const message = [structured?.message, debugBody, payload.message, payload.rootCause]
          .filter(value => typeof value === 'string').join(' ').toLowerCase();
        summary.error_concepts = ['model', 'unsupported', 'not supported', 'does not exist', 'permission', 'access',
          'organization', 'tier', 'reasoning', 'effort', 'input', 'role', 'invalid', 'required', 'restricted',
          'not allowed', 'unknown'].filter(concept => message.includes(concept));
      }
      return summary;
    } catch {
      summary.transport_failure = true;
      return summary;
    } finally {
      if (!completed && requestId && turnState) {
        try {
          const stopped = await call('/api/llm/response_with_tools_stop', { request_id: requestId,
            conversation_id: native.conversationId, turn_state: turnState }, 10000);
          summary.stop_http_status = stopped.status;
        } catch { summary.stop_failed = true; }
      }
    }
  }, { native, effort });
  report.success = report.model_starts === 1 && report.start_sentinel_present && report.result.output_matches_ready === true;
} catch (error) {
  report.success = false;
  report.failure_stage = phase;
  report.error_type = error.constructor.name;
} finally {
  clearTimeout(deadline);
  await driver.close();
  await browser.close();
  const rendered = JSON.stringify(report, null, 2);
  if (process.env.PRISM_PROBE_REPORT) writeFileSync(process.env.PRISM_PROBE_REPORT, rendered + '\n', { mode: 0o600 });
  console.log(rendered);
}
