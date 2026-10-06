import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { request as httpRequest } from 'node:http';
import { PrismConfigurationStore, configurationFromEnvironment } from '../src/configuration.mjs';
import { createPrismServer } from '../src/server.mjs';
import { PrismError } from '../src/errors.mjs';

const managementKey = 'management-fixture'.repeat(4);
const userKey = 'user-fixture'.repeat(4);
const defaults = configurationFromEnvironment({});
const changed = { ...defaults, project_isolation: true, http_cache: true, memory_limit_mib: 2048,
  multiplex_pages: true, prewarm_chat: false, stream_reasoning: false };

async function fixture(t, options = {}) {
  const dataDir = await mkdtemp(join(tmpdir(), 'prism-config-http-test-'));
  const configuration = options.configuration || new PrismConfigurationStore({ dataDir, environment: {} });
  if (!options.configuration) await configuration.init();
  const manager = { status() { return { ready: true, models: ['gpt-6.1-sol'] }; },
    authenticateKey(source, key) { if (source !== '32' || key !== userKey) throw new PrismError('invalid_api_key', 401); },
    async generate(_, request) { request.onAccepted?.(); request.onReasoning?.('offline reasoning'); return 'offline answer'; },
    ...options.manager };
  const server = createPrismServer({ manager, managementKey, configuration,
    ...(options.promptCache ? { promptCache: options.promptCache } : {}),
    projectIsolation: defaults.project_isolation, streamReasoning: defaults.stream_reasoning });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(async () => { server.closeAllConnections(); await new Promise(resolve => server.close(resolve));
    await rm(dataDir, { recursive: true, force: true }); });
  const base = `http://127.0.0.1:${server.address().port}`;
  const call = (method, body, key = managementKey, headers = {}) => fetch(base + '/internal/config', {
    method, headers: { ...(key ? { Authorization: `Bearer ${key}` } : {}), 'Content-Type': 'application/json', ...headers },
    ...(body === undefined ? {} : { body: typeof body === 'string' ? body : JSON.stringify(body) }) });
  return { base, call, configuration };
}

test('config access is management-only and authentication happens before any config read or write', async t => {
  let calls = 0;
  const configuration = { snapshot() { calls++; }, async put() { calls++; }, async reset() { calls++; } };
  const { base, call } = await fixture(t, { configuration });
  for (const method of ['GET', 'PUT', 'DELETE']) {
    for (const key of ['', userKey, 'wrong-management-key']) {
      const response = await call(method, method === 'PUT' ? changed : undefined, key);
      assert.equal(response.status, 401);
      assert.equal((await response.json()).error.code, 'invalid_management_key');
    }
  }
  assert.equal(calls, 0);
  assert.deepEqual(await (await fetch(base + '/health')).json(), { status: 'ok' });
  assert.equal(calls, 0);
  const response = await fetch(base + '/accounts/32/v1/models', { headers: { Authorization: `Bearer ${managementKey}` } });
  assert.equal(response.status, 401, 'a management credential never authorizes a user request');
});

test('effective master switch blocks Prism traffic while retaining configuration and journal reconciliation', async t => {
  const configuration = { effective: { ...defaults, enabled: false }, snapshot() {
      return { effective: { ...defaults, enabled: false }, desired: { ...defaults, enabled: false },
        restart_required: false, source: 'saved', apply_mode: 'restart' };
    }, async put() { return this.snapshot(); }, async reset() { return this.snapshot(); } };
  const { base, call } = await fixture(t, { configuration, manager: {
    pending() { return []; }, resolvePending() { return { removed: true }; },
  } });
  const blocked = await fetch(base + '/accounts/32/v1/models', { headers: { Authorization: `Bearer ${userKey}` } });
  assert.equal(blocked.status, 503);
  assert.equal((await blocked.json()).error.code, 'prism_disabled');
  const invalidUser = await fetch(base + '/accounts/32/v1/models', { headers: { Authorization: 'Bearer wrong-user-key' } });
  assert.equal(invalidUser.status, 401);
  assert.equal((await invalidUser.json()).error.code, 'invalid_api_key');
  const invalidManagement = await fetch(base + '/internal/accounts/32/status', { headers: { Authorization: 'Bearer wrong-management-key' } });
  assert.equal(invalidManagement.status, 401);
  assert.equal((await invalidManagement.json()).error.code, 'invalid_management_key');
  const disabledStatus = await fetch(base + '/internal/accounts/32/status', { headers: { Authorization: `Bearer ${managementKey}` } });
  assert.equal(disabledStatus.status, 503);
  assert.equal((await disabledStatus.json()).error.code, 'prism_disabled');
  assert.equal((await call('GET')).status, 200);
  const pending = await fetch(base + '/internal/accounts/32/pending', { headers: { Authorization: `Bearer ${managementKey}` } });
  assert.equal(pending.status, 200);
  assert.deepEqual(await pending.json(), { pending_turns: [] });
});

test('live master switch releases resources on disable and initializes them on enable', async t => {
  const lifecycle = [];
  let cacheClears = 0;
  const { base, call, configuration } = await fixture(t, { promptCache: { clear() { cacheClears += 1; } }, manager: {
    async setEnabled(enabled) { lifecycle.push(enabled); },
  } });
  const disabled = await call('PUT', { ...changed, enabled: false });
  assert.equal(disabled.status, 200);
  assert.deepEqual(lifecycle, [false]);
  assert.equal(cacheClears, 1);
  const blocked = await fetch(base + '/accounts/32/v1/models', { headers: { Authorization: `Bearer ${userKey}` } });
  assert.equal(blocked.status, 503);
  assert.equal((await blocked.json()).error.code, 'prism_disabled');

  const enabled = await call('PUT', { ...changed, enabled: true });
  assert.equal(enabled.status, 200);
  assert.deepEqual(lifecycle, [false, true]);
  assert.equal(cacheClears, 1);
  assert.equal((await fetch(base + '/accounts/32/v1/models', { headers: { Authorization: `Bearer ${userKey}` } })).status, 200);
  assert.equal(configuration.snapshot().desired.enabled, true);
});

test('failed enable keeps the live gate closed until resource initialization succeeds', async t => {
  let failEnable = false;
  const { base, call } = await fixture(t, { manager: {
    async setEnabled(enabled) {
      if (enabled && failEnable) throw new PrismError('browser_operation_failed', 502);
    },
  } });
  assert.equal((await call('PUT', { ...changed, enabled: false })).status, 200);
  failEnable = true;
  const failed = await call('PUT', { ...changed, enabled: true });
  assert.equal(failed.status, 502);
  assert.equal((await failed.json()).error.code, 'browser_operation_failed');
  const blocked = await fetch(base + '/accounts/32/v1/models', { headers: { Authorization: `Bearer ${userKey}` } });
  assert.equal(blocked.status, 503);
  assert.equal((await blocked.json()).error.code, 'prism_disabled');
});

test('GET PUT DELETE expose the exact startup contract and preserve no-store responses', async t => {
  const { call } = await fixture(t);
  const initial = await call('GET');
  assert.equal(initial.status, 200);
  assert.equal(initial.headers.get('cache-control'), 'no-store');
  assert.deepEqual(await initial.json(), { effective: defaults, desired: defaults, restart_required: false,
    source: 'environment', apply_mode: 'restart' });
  const saved = await call('PUT', changed);
  assert.equal(saved.status, 200);
  assert.deepEqual(await saved.json(), { effective: defaults, desired: changed, restart_required: true,
    source: 'saved', apply_mode: 'restart' });
  assert.deepEqual((await (await call('GET')).json()).desired, changed);
  const reset = await call('DELETE');
  assert.equal(reset.status, 200);
  assert.deepEqual(await reset.json(), { effective: defaults, desired: defaults, restart_required: false,
    source: 'environment', apply_mode: 'restart' });
  const wrongMethod = await call('POST', {});
  assert.equal(wrongMethod.status, 405);
  assert.equal((await wrongMethod.json()).error.code, 'method_not_allowed');
});

test('config PUT rejects unknown, missing, coerced, malformed and oversize payloads without persistence', async t => {
  const { call, configuration } = await fixture(t);
  const { prewarm_chat: omitted, ...missing } = changed;
  for (const value of [null, [], {}, missing, { ...changed, api_key: 'must-not-be-persisted' },
    { ...changed, http_cache: 'true' }, { ...changed, memory_limit_mib: '2048' },
    { ...changed, memory_reserve_mib: 2048 }, { ...changed, memory_limit_mib: -1 }]) {
    const response = await call('PUT', value);
    assert.equal(response.status, 400);
    assert.equal((await response.json()).error.code, 'invalid_prism_configuration');
  }
  assert.equal((await call('PUT', '{bad-json')).status, 400);
  assert.equal((await call('PUT', JSON.stringify(changed) + ' '.repeat(4096))).status, 413);
  assert.equal((await call('PUT', changed, managementKey, { 'Content-Encoding': 'gzip' })).status, 415);
  assert.equal((await call('PUT', changed, managementKey, { 'Content-Type': 'text/plain' })).status, 415);
  assert.deepEqual(configuration.snapshot().desired, defaults);
});

test('chunked config PUT bodies without a content length are also bounded', async t => {
  const { base, configuration } = await fixture(t);
  const response = await new Promise((resolve, reject) => {
    const req = httpRequest(base + '/internal/config', { method: 'PUT', headers: {
      Authorization: `Bearer ${managementKey}`, 'Content-Type': 'application/json' } }, res => {
      let body = ''; res.on('data', chunk => { body += chunk; });
      res.on('end', () => resolve({ status: res.statusCode, body }));
    });
    req.on('error', reject);
    req.write(JSON.stringify(changed)); req.end(' '.repeat(5000));
  });
  assert.equal(response.status, 413);
  assert.equal(JSON.parse(response.body).error.code, 'request_body_too_large');
  assert.deepEqual(configuration.snapshot().desired, defaults);
});

test('configuration exceptions remain sanitized and cannot reveal storage paths or credentials', async t => {
  const secret = 'secret-management-token-and-private-directory';
  const configuration = { snapshot() { throw new Error(secret); },
    async put() { throw new Error(secret); }, async reset() { throw new Error(secret); } };
  const { call } = await fixture(t, { configuration });
  for (const method of ['GET', 'PUT', 'DELETE']) {
    const response = await call(method, method === 'PUT' ? changed : undefined);
    assert.equal(response.status, 502);
    const body = await response.text();
    assert.equal(JSON.parse(body).error.code, 'browser_operation_failed');
    assert.ok(!body.includes(secret));
  }
});

test('saving or resetting pending config leaves active requests and server reasoning/scope behavior unchanged', async t => {
  let started;
  const accepted = new Promise(resolve => { started = resolve; });
  let release;
  const paused = new Promise(resolve => { release = resolve; });
  const seen = [];
  const { base, call, configuration } = await fixture(t, { manager: {
    async generate(_, request) {
      seen.push(request); started(); await paused;
      request.onAccepted?.(); request.onReasoning?.('still-enabled reasoning'); return 'unchanged answer';
    },
  } });
  const originalEnvironment = Object.fromEntries(Object.entries(process.env).filter(([key]) => key.startsWith('PRISM_')));
  const request = fetch(base + '/accounts/32/v1/responses', { method: 'POST', headers: {
    Authorization: `Bearer ${userKey}`, 'Content-Type': 'application/json',
    'X-Prism-Key-Scope': 'ignored-while-isolation-is-disabled' },
    body: JSON.stringify({ model: 'gpt-6.1-sol', input: 'offline test', stream: true }) });
  await accepted;
  assert.equal((await call('PUT', changed)).status, 200);
  assert.deepEqual(configuration.effective, defaults);
  release();
  const response = await request;
  assert.equal(response.status, 200);
  const stream = await response.text();
  assert.match(stream, /still-enabled reasoning/);
  assert.match(stream, /unchanged answer/);
  assert.equal(seen[0].projectScope, undefined);
  const after = await fetch(base + '/accounts/32/v1/responses', { method: 'POST', headers: {
    Authorization: `Bearer ${userKey}`, 'Content-Type': 'application/json',
    'X-Prism-Key-Scope': 'still-ignored' }, body: JSON.stringify({ model: 'gpt-6.1-sol', input: 'next', stream: true }) });
  assert.equal(after.status, 200);
  assert.match(await after.text(), /still-enabled reasoning/);
  assert.equal(seen[1].projectScope, undefined);
  assert.equal((await call('DELETE')).status, 200);
  assert.deepEqual(Object.fromEntries(Object.entries(process.env).filter(([key]) => key.startsWith('PRISM_'))), originalEnvironment);
});
