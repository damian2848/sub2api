import { readFileSync, writeFileSync } from 'node:fs';
import { randomBytes } from 'node:crypto';
import { execFileSync } from 'node:child_process';

// Isolated deployment verification only. Credentials enter through stdin, never argv/logs.
const environmentFile = process.env.PRISM_PROBE_ENV || '/opt/sub2api-gpt56/research/prism-browser-test.env';
if (process.argv.includes('--prepare')) {
  writeFileSync(environmentFile, `PRISM_MANAGEMENT_KEY=${randomBytes(32).toString('hex')}\nPRISM_TEST_API_KEY=${randomBytes(32).toString('hex')}\nPRISM_AUDIT_REQUESTS=true\n`, { mode: 0o600 });
  process.exit(0);
}
const environment = Object.fromEntries(readFileSync(environmentFile, 'utf8').trim().split('\n').map(line => line.split('=')));
if (process.argv.includes('--status')) {
  const response = await fetch('http://127.0.0.1:18319/internal/accounts/32/status', {
    headers: { Authorization: `Bearer ${environment.PRISM_MANAGEMENT_KEY}` } });
  const status = await response.json();
  console.log(JSON.stringify({ phase: status.phase, ready: status.ready, models: status.models, error_code: status.error_code }));
  process.exit(0);
}
const input = JSON.parse(readFileSync(0, 'utf8'));
const payload = JSON.parse(Buffer.from(input.access_token.split('.')[1], 'base64url').toString('utf8'));
const provision = { access_token: input.access_token, api_key: environment.PRISM_TEST_API_KEY, expires_at: payload.exp,
  ...(input.expected_emails?.length ? { expected_email: input.expected_emails[0] } : { expected_user_id: input.expected_ids[0] }) };
const base = process.env.PRISM_PROBE_BASE || 'http://127.0.0.1:18319';
const report = { checked_at: new Date().toISOString(), source_account: 32, production_modified: false, checks: [] };
let managedProject;
const check = (name, ok, extra = {}) => {
  report.checks.push({ name, ok, ...extra });
  if (!ok) throw new Error(name);
};
async function request(path, method, body, management = true) {
  const response = await fetch(base + path, { method, headers: { 'Content-Type': 'application/json',
    Authorization: `Bearer ${management ? environment.PRISM_MANAGEMENT_KEY : environment.PRISM_TEST_API_KEY}` },
    ...(body !== undefined ? { body: JSON.stringify(body) } : {}), signal: AbortSignal.timeout(250000) });
  if (response.headers.get('content-type')?.includes('text/event-stream')) return { status: response.status, text: await response.text() };
  const data = await response.json();
  if (data.project_id) managedProject = data.project_id;
  return { status: response.status, data };
}
const metadata = () => JSON.parse(readFileSync('/opt/sub2api-gpt56/research/prism-browser-test-data/32.json', 'utf8'));
try {
  let priorProbeCount = 0;
  try { priorProbeCount = metadata().readiness_probe_count || 0; } catch {}
  let result = await request('/internal/accounts/32/session', 'PUT', provision);
  check('oauth_identity_verified', result.status === 200 && ['authenticated', 'ready', 'request_failed'].includes(result.data.phase),
    { status: result.status, error_code: result.data.error?.code });
  check('provision_no_model_probe', metadata().readiness_probe_count === priorProbeCount);
  result = await request('/internal/accounts/32/bootstrap', 'POST', {});
  check('first_bootstrap_ready', result.status === 200 && result.data.ready,
    { status: result.status, error_code: result.data.error?.code, models: result.data.models });
  const model = result.data.models[0];
  const astra = result.data.models.find(id => id.endsWith('-astra'));
  if (astra) {
    result = await request('/accounts/32/v1/responses', 'POST', { model: astra, reasoning: { effort: 'low' },
      input: 'Reply with only READY. Do not edit files or use tools.' }, false);
    check('astra_exact_model_ready', result.status === 200 && result.data.output_text?.trim() === 'READY',
      { model: astra, status: result.status, error_code: result.data.error?.code });
  } else report.astra_unavailable_in_catalog = true;
  const firstProbeCount = metadata().readiness_probe_count;
  result = await request('/accounts/32/v1/models', 'GET', undefined, false);
  check('account_catalog_matches_bootstrap', result.status === 200 && result.data.data.some(item => item.id === model));
  result = await request('/accounts/32/v1/responses', 'POST', { model, input: [
    { role: 'system', content: 'Original rules' }, { role: 'user', content: 'Original question' },
  ] }, false);
  check('unsupported_history_is_rejected', result.status === 400 && result.data.error?.code === 'conversation_not_supported');
  result = await request('/accounts/32/v1/responses', 'POST', { model, stream: true, reasoning: { effort: 'low' },
    input: 'Reply with only SSEPASS. Do not use tools or edit files.' }, false);
  const events = result.text?.split('\n').filter(line => line.startsWith('data: ')).map(line => {
    try { return JSON.parse(line.slice(6)); } catch { return null; }
  }) || [];
  const completed = events.find(event => event?.type === 'response.completed');
  const streamError = events.find(event => event?.type === 'error');
  const roleOutput = completed?.response.output_text || '';
  const roleMatches = roleOutput.trim() === 'SSEPASS';
  check('responses_text_and_sse', result.status === 200 && roleMatches,
    { status: result.status, completed_event: Boolean(completed), error_code: result.data?.error?.code || streamError?.error?.code,
      estimated_usage: completed?.response.usage.estimation === 'character_based_estimate',
      safe_output_excerpt: roleOutput.replaceAll(input.access_token, '[token]')
        .replace(/[A-Za-z0-9_.+-]+@[A-Za-z0-9.-]+/g, '[email]')
        .replace(/\b[A-Za-z0-9_-]{24,}(?:\.[A-Za-z0-9_-]+)*\b/g, '[opaque]').slice(0, 250) });
  result = await request('/accounts/32/v1/chat/completions', 'POST', { model, reasoning_effort: 'low',
    messages: [{ role: 'user', content: 'Reply with only CHATPASS. Do not use tools or edit files.' }] }, false);
  check('chat_completion', result.status === 200 && result.data.choices?.[0].message.content.trim() === 'CHATPASS',
    { status: result.status, error_code: result.data.error?.code });
  result = await request('/internal/accounts/32/session', 'DELETE');
  check('revoke_preserves_managed_project', result.status === 200 && result.data.project_id === managedProject && !result.data.ready);
  result = await request('/internal/accounts/32/session', 'PUT', provision);
  check('access_token_resupply', result.status === 200);
  result = await request('/internal/accounts/32/bootstrap', 'POST', {});
  check('resupply_recovers_without_model_probe', result.status === 200 && result.data.ready &&
    metadata().readiness_probe_count === firstProbeCount, { status: result.status, error_code: result.data.error?.code });
  execFileSync('docker', ['restart', 'prism-browser-feature-e2e'], { stdio: 'ignore' });
  for (let attempt = 0; attempt < 60; attempt += 1) {
    try { if ((await fetch(base + '/health')).ok) break; } catch {}
    await new Promise(resolve => setTimeout(resolve, 500));
  }
  result = await request('/internal/accounts/32/status', 'GET');
  check('restart_requires_authentication', result.status === 200 && !result.data.ready);
  await request('/internal/accounts/32/session', 'PUT', provision);
  result = await request('/internal/accounts/32/bootstrap', 'POST', {});
  check('restart_recovers_saved_project_without_model_probe', result.status === 200 && result.data.ready &&
    metadata().readiness_probe_count === firstProbeCount, { status: result.status, error_code: result.data.error?.code });
  report.success = true;
} catch (error) {
  report.success = false;
  report.failure_check = typeof error.message === 'string' && /^[a-z_]+$/.test(error.message) ? error.message : 'probe_transport_failure';
  report.error_type = error.constructor.name;
} finally {
  if (!managedProject) {
    try { managedProject = metadata().project_id; } catch {}
  }
  if (managedProject) {
    try {
      if (process.argv.includes('--keep-project')) {
        report.temporary_project_preserved_for_debugging = true;
        console.log(JSON.stringify(report, null, 2));
        process.exit(0);
      }
      await request('/internal/accounts/32/session', 'DELETE');
      const cleanup = execFileSync('docker', ['run', '--rm', '-i', '--network', 'host',
        '-v', '/opt/sub2api-gpt56/research:/research:ro', '-e', 'PRISM_PROBE_PLAYWRIGHT=/app/node_modules/playwright',
        '--entrypoint', 'node', 'local/sub2api-prism-browser-test:0.1.0', '/research/probe_browser.cjs',
        `--cleanup-project=${managedProject}`], { input: JSON.stringify(input), encoding: 'utf8', timeout: 180000,
        stdio: ['pipe', 'pipe', 'pipe'] });
      const data = JSON.parse(cleanup.trim());
      report.cleanup = { delete_status: data.cleanup?.delete_status,
        temporary_project_absent: data.cleanup?.temporary_project_absent === true };
    } catch { report.cleanup = { temporary_project_absent: false }; }
  }
  console.log(JSON.stringify(report, null, 2));
}
