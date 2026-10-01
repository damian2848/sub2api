import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm, stat } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { AccountManager } from '../src/accounts.mjs';
import { createPrismServer } from '../src/server.mjs';
import { AccountQueue } from '../src/queue.mjs';
import { PrismError } from '../src/errors.mjs';

const session = (token = 'a') => ({ access_token: `${token}`.repeat(40), api_key: 'k'.repeat(40),
  expected_email: 'one@example.com', expires_at: Math.floor(Date.now() / 1000) + 3600 });
const project = '01234567-89ab-4cde-8123-0123456789ab';
function fakeFactory(log, { probeResult = 'READY' } = {}) {
  return (_, heartbeat) => ({
    lastHeartbeat: 0, closed: false,
    async authenticate(body) { log.auth.push(body); return body.access_token.startsWith('b') ? 'different-user' : 'stable-user'; },
    async initialize(existing, created, signal, previous) {
      log.projects.push(existing);
      log.previous.push(previous);
      if (!existing) await created(project);
      this.lastHeartbeat = Math.floor(Date.now() / 1000);
      heartbeat(this.lastHeartbeat);
      return ['gpt-6.1-sol', 'gpt-6-luna'];
    },
    async generate(request) { log.generations.push(request); return probeResult; },
    async close() { this.closed = true; log.closed += 1; },
    isAlive() { return !this.closed; },
  });
}
async function fixture(t, options = {}) {
  const dataDir = await mkdtemp(join(tmpdir(), 'prism-browser-test-'));
  t.after(() => rm(dataDir, { recursive: true, force: true }));
  const log = { auth: [], projects: [], previous: [], generations: [], closed: 0 };
  const settings = { dataDir, browserFactory: async () => ({ async close() {} }), sessionFactory: fakeFactory(log, options) };
  const manager = new AccountManager(settings);
  await manager.init();
  t.after(() => manager.close());
  return { manager, log, dataDir, settings };
}

test('session synchronization sends no model call; ready project survives refresh and restart', async t => {
  const { manager, log, dataDir, settings } = await fixture(t);
  await manager.provision('32', session());
  await manager.provision('32', session());
  assert.equal(log.auth.length, 1);
  assert.equal(log.generations.length, 0);
  assert.equal(manager.status('32').ready, false);
  await manager.bootstrap('32');
  assert.equal(log.generations.length, 1);
  assert.equal(manager.status('32').ready, true);
  await manager.bootstrap('32');
  assert.equal(log.generations.length, 1);
  await manager.bootstrap('32', undefined, { retry_probe: true });
  assert.equal(log.generations.length, 1);
  assert.equal(log.projects.length, 2);
  await manager.provision('32', session('c'));
  await manager.bootstrap('32');
  assert.equal(log.generations.length, 1);
  assert.deepEqual(log.projects, [null, project, project]);
  const persisted = await readFile(join(dataDir, '32.json'), 'utf8');
  assert.ok(!persisted.includes(session().access_token));
  assert.ok(!persisted.includes(session().api_key));
  assert.ok(!persisted.includes(session().expected_email));
  assert.equal((await stat(join(dataDir, '32.json'))).mode & 0o777, 0o600);
  assert.equal((await stat(dataDir)).mode & 0o777, 0o700);
  const restarted = new AccountManager(settings);
  await restarted.init();
  t.after(() => restarted.close());
  assert.equal(restarted.status('32').ready, false);
  await restarted.provision('32', session('d'));
  await restarted.bootstrap('32');
  assert.equal(log.generations.length, 1);
  assert.equal(log.projects.at(-1), project);
});

test('stable actual Prism identity binds source; changing expected email is not a new identity', async t => {
  const { manager } = await fixture(t);
  await manager.provision('32', session());
  await manager.provision('32', { ...session('c'), expected_email: 'renamed@example.com' });
  await assert.rejects(manager.provision('32', session('b')), error => error.code === 'source_identity_changed');
  assert.equal(manager.status('32').ready, false);
});

test('account API keys are isolated, revoked keys stop working, and project is preserved', async t => {
  const { manager } = await fixture(t);
  await manager.provision('32', session());
  await manager.bootstrap('32');
  await manager.provision('33', { ...session(), api_key: 'z'.repeat(40) });
  assert.throws(() => manager.authenticateKey('33', session().api_key), error => error.code === 'invalid_api_key');
  manager.authenticateKey('32', session().api_key);
  await manager.revoke('32');
  assert.throws(() => manager.authenticateKey('32', session().api_key), error => error.code === 'account_not_ready' && error.status === 503);
  assert.equal(manager.status('32').project_id, project);
  assert.deepEqual(Object.keys(manager.status('32')).sort(), ['models', 'phase', 'project_id', 'ready']);
});

test('failed readiness probe is not retried in the background, explicit retry is once', async t => {
  const { manager, log } = await fixture(t, { probeResult: 'unexpected output' });
  await manager.provision('32', session());
  await assert.rejects(manager.bootstrap('32'), error => error.code === 'readiness_probe_failed');
  await assert.rejects(manager.bootstrap('32'), error => error.code === 'readiness_probe_requires_reauthorization');
  assert.equal(log.generations.length, 1);
  await assert.rejects(manager.bootstrap('32', undefined, { retry_probe: true }), error => error.code === 'readiness_probe_failed');
  assert.equal(log.generations.length, 2);
});

test('bounded account queue serializes calls, rejects overflow and removes cancelled waiters', async () => {
  const queue = new AccountQueue(1);
  let release;
  const running = queue.run(() => new Promise(resolve => { release = resolve; }));
  const controller = new AbortController();
  const queued = queue.run(async () => assert.fail('cancelled job ran'), controller.signal);
  await assert.rejects(queue.run(async () => null), error => error.code === 'account_queue_full');
  controller.abort(new PrismError('request_timeout', 504));
  await assert.rejects(queued, error => error.code === 'request_timeout');
  let didRun = false;
  const next = queue.run(async () => { didRun = true; });
  assert.equal(didRun, false);
  release();
  await running;
  await next;
  assert.equal(didRun, true);
});

test('revoke cancels authentication already running and authentication queued before revocation', async t => {
  const { manager, log } = await fixture(t);
  let release;
  let entered;
  const ready = new Promise(resolve => { entered = resolve; });
  const wait = new Promise(resolve => { release = resolve; });
  manager.sessionFactory = () => ({
    async authenticate(body) { log.auth.push(body); entered(); await wait; return 'stable-user'; },
    async close() { log.closed += 1; },
  });
  const first = manager.provision('32', session());
  await ready;
  const second = manager.provision('32', session('c'));
  const revoked = manager.revoke('32');
  const firstRejected = assert.rejects(first, error => error.code === 'session_revoked');
  const secondRejected = assert.rejects(second, error => error.code === 'session_revoked');
  release();
  await firstRejected;
  await secondRejected;
  await revoked;
  assert.equal(log.auth.length, 1);
  assert.equal(log.closed, 1);
  assert.equal(manager.get('32').driver, null);
  assert.equal(manager.status('32').ready, false);
});

test('unchanged token synchronization does not wait behind a running generation', async t => {
  const { manager } = await fixture(t);
  await manager.provision('32', session());
  await manager.bootstrap('32');
  let release;
  let entered;
  const started = new Promise(resolve => { entered = resolve; });
  manager.get('32').driver.generate = async () => { entered(); return new Promise(resolve => { release = resolve; }); };
  const generation = manager.generate('32', { model: 'gpt-6.1-sol' });
  await started;
  const refreshed = await manager.provision('32', { ...session(), expires_at: Math.floor(Date.now() / 1000) + 7200 });
  assert.equal(refreshed.ready, true);
  assert.equal(manager.get('32').queue.jobs.length, 0);
  release('completed');
  assert.equal(await generation, 'completed');
});

test('bootstrap hands the persisted catalog to the browser so a collapsed menu can be detected', async t => {
  const { manager, log, settings } = await fixture(t);
  await manager.provision('32', session());
  await manager.bootstrap('32');
  await manager.bootstrap('32', undefined, { retry_probe: true });
  assert.deepEqual(log.previous, [[], ['gpt-6.1-sol', 'gpt-6-luna']]);
  const restarted = new AccountManager(settings);
  await restarted.init();
  t.after(() => restarted.close());
  await restarted.provision('32', session('d'));
  await restarted.bootstrap('32');
  assert.deepEqual(log.previous.at(-1), ['gpt-6.1-sol', 'gpt-6-luna']);
});

test('one bad request does not take a live account offline', async t => {
  const { manager } = await fixture(t);
  await manager.provision('32', session());
  await manager.bootstrap('32');
  const account = manager.get('32');
  const request = { model: 'gpt-6.1-sol', effort: 'low', input: [] };
  const codes = ['prism_generation_failed', 'prism_empty_output', 'prism_invalid_output', 'upstream_model_mismatch',
    'model_not_available', 'request_cancelled', 'request_timeout', 'prism_upstream_http_error', 'image_input_not_supported',
    'browser_model_selection_mismatch', 'account_busy'];
  for (const code of codes) {
    account.driver.generate = async () => { throw new PrismError(code, 400); };
    await assert.rejects(manager.generate('32', request), error => error.code === code);
    assert.equal(manager.status('32').ready, true, code);
    assert.equal(manager.status('32').phase, 'ready', code);
    assert.equal(manager.status('32').error_code, undefined, code);
  }
  account.driver.generate = async () => { throw new Error('unexpected failure carrying a secret'); };
  await assert.rejects(manager.generate('32', request), /unexpected failure/);
  assert.equal(manager.status('32').ready, true);
  account.driver.generate = async () => 'served';
  assert.equal(await manager.generate('32', request), 'served');
  // A request for a model outside the catalog is refused without touching the browser.
  await assert.rejects(manager.generate('32', { ...request, model: 'gpt-9' }), error => error.code === 'model_not_available');
  assert.equal(manager.status('32').ready, true);
});

test('a dead browser or a session-level failure takes the account offline until it is bootstrapped again', async t => {
  const { manager } = await fixture(t);
  await manager.provision('32', session());
  await manager.bootstrap('32');
  const account = manager.get('32');
  const request = { model: 'gpt-6.1-sol', effort: 'low', input: [] };
  const codes = ['session_expired', 'browser_session_closed', 'session_closed', 'browser_ui_composer_failed',
    'browser_ui_model_selection_failed', 'browser_ui_new_chat_failed', 'browser_project_mismatch', 'browser_previous_context_present'];
  for (const code of codes) {
    account.driver.generate = async () => { throw new PrismError(code, 503); };
    await assert.rejects(manager.generate('32', request), error => error.code === code);
    assert.equal(manager.status('32').ready, false, code);
    assert.equal(manager.status('32').error_code, code);
    await assert.rejects(manager.generate('32', request), error => error.code === 'account_not_ready');
    await manager.bootstrap('32');
    assert.equal(manager.status('32').ready, true, code);
  }
  // A dead driver makes even an ordinary per-request error fatal for readiness.
  account.driver.generate = async () => { account.driver.closed = true; throw new PrismError('prism_generation_failed'); };
  await assert.rejects(manager.generate('32', request), error => error.code === 'prism_generation_failed');
  assert.equal(manager.status('32').ready, false);
  assert.equal(manager.status('32').error_code, 'browser_session_closed');
});

test('user-route authentication: only a wrong key against a provisioned hash is a 401, everything else is not ready', async t => {
  const { manager, settings } = await fixture(t);
  const code = (source, key) => { try { manager.authenticateKey(source, key); return 'ok'; } catch (error) { return `${error.status} ${error.code}`; } };
  // Unknown source (for example a fresh data directory) and an unprovisioned one.
  assert.equal(code('99', session().api_key), '503 account_not_ready');
  assert.equal(code('99', 'anything'), '503 account_not_ready');
  assert.equal(code('99', undefined), '503 account_not_ready');
  assert.equal(code('0', 'x'), '400 invalid_source_id');
  await manager.provision('32', session());
  assert.equal(code('32', session().api_key), 'ok');
  assert.equal(code('32', 'w'.repeat(40)), '401 invalid_api_key');
  assert.equal(code('32', ''), '401 invalid_api_key');
  assert.equal(code('32', undefined), '401 invalid_api_key');
  // Revoked: the hash is gone, so even the old key is only "not ready".
  await manager.revoke('32');
  assert.equal(manager.get('32').metadata.key_hash, null);
  assert.equal(code('32', session().api_key), '503 account_not_ready');
  assert.equal(code('32', 'w'.repeat(40)), '503 account_not_ready');
  // The same after a restart, from the persisted metadata.
  const restarted = new AccountManager(settings);
  await restarted.init();
  t.after(() => restarted.close());
  assert.throws(() => restarted.authenticateKey('32', session().api_key), error => error.code === 'account_not_ready');
  // Reprovisioning brings the key back; management lookups of an unknown source still say 404.
  await manager.provision('32', session());
  assert.equal(code('32', session().api_key), 'ok');
  assert.throws(() => manager.status('99'), error => error.status === 404 && error.code === 'account_not_found');
});

test('over HTTP an unknown, unprovisioned or revoked source is 503 and only a wrong key is 401', async t => {
  const { manager } = await fixture(t);
  const server = createPrismServer({ manager, managementKey: 'm'.repeat(40) });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(async () => { server.closeAllConnections(); await new Promise(resolve => server.close(resolve)); });
  const base = `http://127.0.0.1:${server.address().port}`;
  const call = async (source, key, path = 'responses') => {
    const response = await fetch(`${base}/accounts/${source}/v1/${path}`, { method: path === 'models' ? 'GET' : 'POST',
      headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' },
      ...(path === 'models' ? {} : { body: JSON.stringify({ model: 'gpt-6.1-sol', input: 'hi' }) }) });
    return [response.status, (await response.json()).error?.code];
  };
  const key = session().api_key;
  for (const path of ['responses', 'chat/completions', 'models']) assert.deepEqual(await call('32', key, path), [503, 'account_not_ready']);
  await manager.provision('32', session());
  assert.deepEqual(await call('32', key), [503, 'account_not_ready']); // provisioned but not bootstrapped
  assert.deepEqual(await call('32', 'w'.repeat(40)), [401, 'invalid_api_key']);
  await manager.bootstrap('32');
  assert.deepEqual(await call('32', key, 'models'), [200, undefined]);
  assert.deepEqual(await call('32', 'w'.repeat(40), 'models'), [401, 'invalid_api_key']);
  assert.deepEqual(await call('32', key), [200, undefined]);
  await manager.revoke('32');
  assert.deepEqual(await call('32', key), [503, 'account_not_ready']);
  assert.deepEqual(await call('32', 'w'.repeat(40), 'models'), [503, 'account_not_ready']);
  // Management routes are unchanged: a bad management key is 401, an unknown source status is 404.
  const status = await fetch(`${base}/internal/accounts/99/status`, { headers: { Authorization: `Bearer ${'m'.repeat(40)}` } });
  assert.equal(status.status, 404);
  assert.equal((await fetch(`${base}/internal/accounts/32/status`, { headers: { Authorization: `Bearer ${key}` } })).status, 401);
});
