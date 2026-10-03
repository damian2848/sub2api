import test from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createPrismServer } from '../src/server.mjs';
import { PrismConfigurationStore, configurationFromEnvironment } from '../src/configuration.mjs';
import { connectRestartSupervisor, PrismRestartController } from '../src/restart.mjs';

const key = 'restart-management-fixture'.repeat(3);
const defaults = configurationFromEnvironment({});
const changed = { ...defaults, project_isolation: true, http_cache: true };
async function fixture(t, { supported = true, prepareRestart } = {}) {
  const dataDir = await mkdtemp(join(tmpdir(), 'prism-restart-'));
  const configuration = new PrismConfigurationStore({ dataDir, environment: {} });
  await configuration.init();
  const observed = { prepared: 0, restarted: 0, generated: 0 };
  const restart = new PrismRestartController({ configuration,
    ...(supported ? { prepareRestart: prepareRestart || (async () => { observed.prepared++; }),
      onRestart: () => { observed.restarted++; } } : {}) });
  const server = createPrismServer({ configuration, restart, managementKey: key,
    manager: { authenticateKey() {}, status() { return { ready: true, models: [] }; },
      async generate() { observed.generated++; return 'offline'; } } });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(async () => { server.closeAllConnections(); await new Promise(resolve => server.close(resolve));
    await rm(dataDir, { recursive: true, force: true }); });
  const base = `http://127.0.0.1:${server.address().port}`;
  const call = (path, method = 'GET', body, bearer = key) => fetch(base + path, { method,
    headers: { Authorization: `Bearer ${bearer}`, 'Content-Type': 'application/json' },
    ...(body === undefined ? {} : { body: typeof body === 'string' ? body : JSON.stringify(body) }) });
  const target = () => ({ expected_runtime_id: restart.runtimeId, expected_configuration: configuration.snapshot().desired });
  return { restart, configuration, dataDir, observed, call, target };
}

test('restart is management-only and direct unsupervised servers fail closed', async t => {
  const f = await fixture(t, { supported: false });
  for (const method of ['GET', 'POST']) {
    const r = await f.call('/internal/restart', method, method === 'POST' ? f.target() : undefined, 'wrong');
    assert.equal(r.status, 401);
  }
  assert.equal((await (await f.call('/internal/restart')).json()).supported, false);
  assert.equal((await f.call('/internal/restart', 'POST', f.target())).status, 503);
  assert.equal((await f.call('/internal/restart', 'PUT', {})).status, 405);
  assert.equal(f.observed.restarted, 0);
  assert.equal(f.restart.pending, false);
});

test('only exact saved target and boot may restart; duplicates are rejected at every depth', async t => {
  const f = await fixture(t);
  const valid = f.target();
  for (const body of [null, [], {}, { ...valid, command: 'docker restart any' },
    { ...valid, expected_runtime_id: 'not-a-uuid' }, { ...valid, expected_configuration: {} },
    { ...valid, expected_configuration: { ...defaults, http_cache: 'true' } }]) {
    assert.equal((await f.call('/internal/restart', 'POST', body)).status, 400);
  }
  const raw = JSON.stringify(valid);
  for (const body of [raw.replace('{', '{"expected_runtime_id":"' + valid.expected_runtime_id + '",'),
    raw.replace('"http_cache":false', '"http_cache":false,"http_cache":true'),
    raw.replace('"http_cache":false', '"http_cache":false,"http_\\u0063ache":true')]) {
    assert.equal((await f.call('/internal/restart', 'POST', body)).status, 400);
  }
  assert.equal((await f.call('/internal/restart', 'POST', raw + ' '.repeat(4096))).status, 413);
  assert.equal((await f.call('/internal/restart', 'POST', { ...valid,
    expected_runtime_id: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa' })).status, 409);
  assert.equal((await f.call('/internal/restart', 'POST', { ...valid, expected_configuration: changed })).status, 409);
  assert.equal(f.observed.prepared, 0);
  assert.equal(f.restart.pending, false);
});

test('202 precedes shutdown, identical retry is idempotent, and pending restart freezes writes and traffic', async t => {
  const f = await fixture(t);
  await f.configuration.put(changed);
  const target = f.target();
  const result = await f.call('/internal/restart', 'POST', target);
  assert.equal(result.status, 202);
  assert.deepEqual(await result.json(), { supported: true, runtime_id: f.restart.runtimeId, state: 'restarting' });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(f.observed.restarted, 1);
  assert.equal((await f.call('/internal/restart', 'POST', target)).status, 202);
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(f.observed.restarted, 1);
  assert.equal(f.observed.prepared, 1);
  assert.equal((await f.call('/internal/config', 'PUT', defaults)).status, 503);
  assert.equal((await f.call('/internal/config', 'DELETE')).status, 503);
  assert.deepEqual(f.configuration.snapshot().desired, changed);
  assert.equal((await f.call('/accounts/32/v1/responses', 'POST', { input: 'not sent' })).status, 503);
  assert.equal(f.observed.generated, 0);
  const next = new PrismConfigurationStore({ dataDir: f.dataDir, environment: {} });
  await next.init();
  assert.deepEqual(next.effective, changed);
  assert.equal(next.snapshot().restart_required, false);
  assert.notEqual(new PrismRestartController({ configuration: next }).runtimeId, f.restart.runtimeId);
});

test('a failed supervisor preparation does not freeze config or announce a restart', async t => {
  const f = await fixture(t, { prepareRestart: async () => { throw new Error('private-token-and-path'); } });
  const response = await f.call('/internal/restart', 'POST', f.target());
  assert.equal(response.status, 502);
  assert.ok(!(await response.text()).includes('private-token-and-path'));
  assert.equal(f.restart.pending, false);
  await f.configuration.put(changed);
  assert.deepEqual(f.configuration.snapshot().desired, changed);
});

test('serialized configuration writes cannot race a reserved restart snapshot', async t => {
  let preparing, release;
  const entered = new Promise(resolve => { preparing = resolve; });
  const waiting = new Promise(resolve => { release = resolve; });
  const f = await fixture(t, { prepareRestart: async () => { preparing(); await waiting; } });
  const accepting = f.restart.accept(f.target());
  await entered;
  const writing = f.configuration.put(changed);
  const writeCheck = assert.rejects(writing, { code: 'prism_restarting' });
  release(); await accepting; await writeCheck;
  assert.deepEqual(f.configuration.snapshot().desired, defaults);
});

test('restart callback waits for the response and cannot freeze forever after client disconnect', async t => {
  const f = await fixture(t);
  await f.restart.accept(f.target());
  const response = new EventEmitter();
  f.restart.afterResponse(response);
  assert.equal(f.observed.restarted, 0);
  response.emit('finish'); response.emit('close');
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(f.observed.restarted, 1);
  const other = await fixture(t);
  await other.restart.accept(other.target());
  const disconnected = new EventEmitter(); disconnected.destroyed = true;
  other.restart.afterResponse(disconnected);
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(other.observed.restarted, 1);
});

test('capability requires acknowledged IPC; environment alone cannot enable restart', async () => {
  assert.equal(await connectRestartSupervisor({ runtime: { env: { PRISM_SUPERVISED: '1' } } }), null);
  const runtime = new EventEmitter(); runtime.env = { PRISM_SUPERVISED: '1' }; runtime.connected = true;
  runtime.send = (value, callback) => { callback(); setImmediate(() => runtime.emit('message', { type: value.type + '_accepted', nonce: value.nonce })); };
  const connection = await connectRestartSupervisor({ runtime });
  assert.ok(connection);
  await connection.prepareRestart();
  assert.equal(runtime.listenerCount('message'), 0);
  assert.equal(runtime.listenerCount('disconnect'), 0);
  runtime.send = (_, callback) => { callback(); };
  assert.equal(await connectRestartSupervisor({ runtime, timeoutMs: 5 }), null);
  runtime.send = (_, callback) => { callback(new Error('secret')); };
  assert.equal(await connectRestartSupervisor({ runtime }), null);
});
