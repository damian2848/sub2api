import test from 'node:test';
import assert from 'node:assert/strict';
import { createPrismServer } from '../src/server.mjs';
import { PrismError } from '../src/errors.mjs';

const managementKey = 'm'.repeat(40);
const userKey = 'u'.repeat(40);
const model = 'gpt-5.6-sol';
async function fixture(t, overrides = {}) {
  const manager = {
    status() { return { phase: 'ready', ready: true, models: [model] }; },
    authenticateKey(source, key) { if (source !== '32' || key !== userKey) throw new PrismError('invalid_api_key', 401); },
    async generate(_, request) { request.onAccepted?.(); return 'ok'; },
    ...overrides,
  };
  const server = createPrismServer({ manager, managementKey });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(async () => { server.closeAllConnections(); await new Promise(resolve => server.close(resolve)); });
  const base = `http://127.0.0.1:${server.address().port}`;
  const get = (path, key) => fetch(base + path, { headers: key ? { Authorization: `Bearer ${key}` } : {} });
  const post = (path, body, key = userKey) => fetch(base + path, { method: 'POST', headers: {
    Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  return { get, post };
}

test('resource diagnostics are management-only, never exposed through health or a user bearer', async t => {
  let resourceCalls = 0;
  const snapshot = { memory: { usedBytes: 1024, thresholdBytes: 2048, source: 'cgroup_v2', pressured: false },
    timings: { queue_wait_ms: { count: 2, mean_ms: 3, p95_ms: 4 } }, contexts: 2, accounts: [{ source: '32', busy_workers: 1 }] };
  const { get, post } = await fixture(t, { async resources() { resourceCalls++; return snapshot; } });
  for (const key of [undefined, userKey, 'bad-management-key']) {
    const response = await get('/internal/resources', key);
    assert.equal(response.status, 401);
    assert.equal((await response.json()).error.code, 'invalid_management_key');
  }
  assert.equal(resourceCalls, 0);
  const health = await get('/health');
  assert.equal(health.status, 200);
  assert.deepEqual(await health.json(), { status: 'ok' });
  assert.equal(resourceCalls, 0);
  const allowed = await get('/internal/resources', managementKey);
  assert.equal(allowed.status, 200);
  assert.equal(allowed.headers.get('cache-control'), 'no-store');
  assert.deepEqual(await allowed.json(), snapshot);
  assert.equal(resourceCalls, 1);
  const method = await post('/internal/resources', {}, managementKey);
  assert.equal(method.status, 404);
  assert.equal(resourceCalls, 1, 'only the documented GET diagnostics endpoint executes');
  assert.equal((await get('/accounts/32/v1/models', managementKey)).status, 401,
    'diagnostic authorization must not double as a user credential');
});

test('diagnostics without a resource sampler use the explicit unavailable response', async t => {
  const { get } = await fixture(t);
  const response = await get('/internal/resources', managementKey);
  assert.equal(response.status, 200);
  assert.deepEqual(await response.json(), { available: false });
});

test('unexpected resource sampler exceptions retain the fixed safe error envelope', async t => {
  const { get } = await fixture(t, { async resources() { throw new Error('secret-access-token-in-resource-sampler'); } });
  const response = await get('/internal/resources', managementKey);
  assert.equal(response.status, 502);
  const wire = await response.text();
  assert.equal(JSON.parse(wire).error.code, 'browser_operation_failed');
  assert.ok(!wire.includes('secret-access-token'));
});

test('memory admission pressure stays a safe retryable JSON 429 before stream commitment', async t => {
  let calls = 0;
  const { post } = await fixture(t, { async generate() {
    calls++;
    throw Object.assign(new PrismError('browser_memory_pressure', 429), { retryAfterSeconds: 10 });
  } });
  for (const stream of [false, true]) {
    const response = await post('/accounts/32/v1/responses', { model, input: 'Private prompt', stream });
    assert.equal(response.status, 429);
    assert.equal(response.headers.get('retry-after'), '10');
    assert.match(response.headers.get('content-type'), /application\/json/);
    assert.deepEqual(await response.json(), { error: {
      message: 'Prism browser memory is under pressure; try again shortly',
      type: 'rate_limit_exceeded', code: 'browser_memory_pressure', resets_in_seconds: 10,
    } });
  }
  assert.equal(calls, 2);
});
