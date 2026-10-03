import test from 'node:test';
import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { mkdtemp, readFile, rm, stat } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { ProjectRegistry, requestProjectScope, freshProjectScope } from '../src/projects.mjs';
import { AccountManager } from '../src/accounts.mjs';
import { AccountPoolManager } from '../src/pool.mjs';
import { PrismError } from '../src/errors.mjs';

const model = 'gpt-5.6-sol';
const scope = (key = 'a', session = 'b') => requestProjectScope({ 'x-prism-key-scope': key.repeat(64),
  'x-prism-session-scope': session.repeat(64) });
const creds = { access_token: 't'.repeat(40), api_key: 'k'.repeat(40), expected_email: 'one@example.com',
  expires_at: Math.floor(Date.now() / 1000) + 3600 };
const request = (projectScope, marker = 'user') => ({ projectScope, model, input: [], marker, effort: 'low' });
async function directory(t) {
  const path = await mkdtemp(join(tmpdir(), 'prism-projects-'));
  t.after(() => rm(path, { recursive: true, force: true }));
  return path;
}
function driver(heartbeat = () => {}) {
  return { page: { isClosed: () => false }, projectId: null, lastHeartbeat: 0, closed: false, initialized: [],
    async authenticate() { return 'stable-user'; },
    async initialize(id, reserve) {
      this.initialized.push(id);
      this.projectId = id || randomUUID();
      if (!id) await reserve(this.projectId);
      this.lastHeartbeat = Math.floor(Date.now() / 1000);
      heartbeat(this.lastHeartbeat);
      return [model];
    },
    async generate() { return 'READY'; },
    async close() { this.closed = true; }, isAlive() { return !this.closed; } };
}

test('reuse requires both hashed authenticated tenant and explicit session; anonymous requests never collide', () => {
  assert.equal(scope().id, scope().id);
  assert.notEqual(scope().id, scope('c').id);
  assert.notEqual(scope().id, scope('a', 'd').id);
  assert.notEqual(freshProjectScope().id, freshProjectScope().id);
  assert.equal(requestProjectScope({ 'x-prism-key-scope': 'a'.repeat(64) }).reusable, false);
  assert.equal(requestProjectScope({ 'x-prism-session-scope': 'b'.repeat(64) }).reusable, false);
  for (const headers of [{ 'x-prism-key-scope': 'raw-api-key' }, { 'x-prism-session-scope': ['a'.repeat(64)] }]) {
    assert.throws(() => requestProjectScope(headers), error => error.code === 'invalid_project_scope' && error.status === 400);
  }
});

test('registry reuses only the same source/key/session, persists hashes only, and keeps anonymous projects out', async t => {
  const dataDir = await directory(t);
  const registry = new ProjectRegistry({ dataDir });
  await registry.init();
  const browser = driver();
  const a = await registry.prepare('32', scope(), browser);
  const again = await registry.prepare('32', scope(), browser);
  assert.equal(again.projectId, a.projectId);
  assert.equal(browser.initialized.length, 1, 'same ready worker keeps its prewarmed chat and HTTP cache');
  for (const [source, identity] of [['32', scope('c')], ['32', scope('a', 'd')], ['33', scope()]]) {
    const other = await registry.prepare(source, identity, browser);
    assert.notEqual(other.projectId, a.projectId);
  }
  const anonymous = await registry.prepare('32', freshProjectScope(), browser);
  const anonymous2 = await registry.prepare('32', freshProjectScope(), browser);
  assert.notEqual(anonymous.projectId, anonymous2.projectId);
  const raw = await readFile(join(dataDir, 'projects', '32.json'), 'utf8');
  assert.equal(raw.includes(anonymous.projectId), false);
  assert.equal(raw.includes(anonymous2.projectId), false);
  assert.equal(raw.includes(creds.api_key), false);
  assert.equal(raw.includes(creds.access_token), false);
  assert.equal((await stat(join(dataDir, 'projects', '32.json'))).mode & 0o777, 0o600);
  const restarted = new ProjectRegistry({ dataDir });
  await restarted.init();
  assert.equal((await restarted.prepare('32', scope(), driver())).projectId, a.projectId);
});

test('failed/cancelled preparation never publishes a reusable project; no native request is replayed', async t => {
  const dataDir = await directory(t);
  const registry = new ProjectRegistry({ dataDir });
  await registry.init();
  const failed = driver();
  const initialize = failed.initialize;
  failed.initialize = async (...args) => { await initialize.apply(failed, args); throw new PrismError('sandbox_initialization_timeout', 504); };
  await assert.rejects(registry.prepare('32', scope(), failed), error => error.code === 'sandbox_initialization_timeout');
  const attempted = failed.projectId;
  const next = await registry.prepare('32', scope(), driver());
  assert.notEqual(next.projectId, attempted);
  const cancelled = new AbortController();
  cancelled.abort(new PrismError('request_timeout', 504));
  await assert.rejects(registry.prepare('32', scope('c'), driver(), { signal: cancelled.signal }), error => error.code === 'request_timeout');
});

test('registry TTL and LRU are bounded and evicted projects are never handed to another conversation', async t => {
  let now = Date.now();
  const registry = new ProjectRegistry({ dataDir: await directory(t), maxSessions: 2, ttlMs: 1000, now: () => now });
  await registry.init();
  const a = await registry.prepare('32', scope(), driver());
  now += 10;
  await registry.prepare('32', scope('c'), driver());
  now += 10;
  await registry.prepare('32', scope('d'), driver());
  assert.equal((await registry.entries('32')).size, 2);
  assert.notEqual((await registry.prepare('32', scope(), driver())).projectId, a.projectId);
  const b = await registry.prepare('32', scope('c'), driver());
  now += 1100;
  assert.notEqual((await registry.prepare('32', scope('c'), driver())).projectId, b.projectId);
});

test('AccountManager default user path cannot use legacy readiness project, including anonymous calls', async t => {
  const manager = new AccountManager({ dataDir: await directory(t), projectIsolation: true, browserFactory: async () => ({ async close() {} }),
    sessionFactory: (_, heartbeat) => driver(heartbeat) });
  await manager.init();
  t.after(() => manager.close());
  await manager.provision('32', creds);
  await manager.bootstrap('32');
  const runtime = manager.get('32');
  const bootstrap = runtime.metadata.project_id;
  await manager.generate('32', request(scope()));
  const first = runtime.driver.projectId;
  assert.notEqual(first, bootstrap);
  await manager.generate('32', request(scope()));
  assert.equal(runtime.driver.projectId, first);
  await manager.generate('32', request(undefined));
  assert.notEqual(runtime.driver.projectId, first);
  assert.notEqual(runtime.driver.projectId, bootstrap);
  assert.equal(runtime.metadata.project_id, bootstrap, 'legacy project is readiness-only');
});

test('pool serializes the same session across workers but permits unrelated sessions in parallel', { timeout: 5000 }, async t => {
  const pending = new Map();
  const seen = [];
  const started = new Map(['first', 'same', 'other'].map(marker => {
    let resolve;
    const promise = new Promise(done => { resolve = done; });
    return [marker, { promise, resolve }];
  }));
  const manager = new AccountPoolManager({ dataDir: await directory(t), concurrency: 2, projectIsolation: true,
    browserFactory: async () => ({ on() {}, async close() {} }),
    sessionFactory: (_, heartbeat, source, slot) => {
      const browser = driver(heartbeat);
      browser.generate = async value => {
        if (!value.marker) return 'READY';
        seen.push({ marker: value.marker, slot, scope: value.projectScope.id, project: browser.projectId });
        const result = new Promise(resolve => pending.set(value.marker, resolve));
        started.get(value.marker).resolve();
        return result;
      };
      return browser;
    } });
  await manager.init();
  t.after(() => manager.close());
  await manager.provision('32', creds);
  await manager.bootstrap('32');
  const first = manager.generate('32', request(scope(), 'first'));
  const same = manager.generate('32', request(scope(), 'same'));
  const other = manager.generate('32', request(scope('c'), 'other'));
  await Promise.all([started.get('first').promise, started.get('other').promise]);
  assert.equal(seen.length, 2);
  assert.notEqual(seen[0].scope, seen[1].scope);
  assert.notEqual(seen[0].project, seen[1].project);
  const project = seen.find(value => value.marker === 'first').project;
  pending.get('first')('one');
  await first;
  await started.get('same').promise;
  assert.equal(seen.length, 3);
  assert.equal(seen.find(value => value.marker === 'same').project, project, 'registry is shared across workers for one scope');
  pending.get('same')('done');
  pending.get('other')('done');
  await Promise.all([same, other]);
});

test('a partially failed reload cannot become reusable just because it set projectId and heartbeat', async t => {
  const registry = new ProjectRegistry({ dataDir: await directory(t) });
  await registry.init();
  const browser = driver();
  const first = await registry.prepare('32', scope(), browser);
  browser.projectId = randomUUID();
  const initialize = browser.initialize;
  browser.initialize = async (...args) => { await initialize.apply(browser, args); throw new PrismError('sandbox_initialization_timeout', 504); };
  await assert.rejects(registry.prepare('32', scope(), browser), error => error.code === 'sandbox_initialization_timeout');
  browser.initialize = initialize;
  const third = await registry.prepare('32', scope(), browser);
  assert.equal(browser.initialized.length, 3);
  assert.notEqual(third.projectId, first.projectId, 'failed project reload is abandoned, not adopted from a partial page');
});

test('high memory only blocks new preparation, not a ready same-session project', async t => {
  const registry = new ProjectRegistry({ dataDir: await directory(t) });
  await registry.init();
  const browser = driver();
  const first = await registry.prepare('32', scope(), browser);
  const admissionGuard = { async assertAdmission() { throw new PrismError('browser_memory_pressure', 429); } };
  assert.equal((await registry.prepare('32', scope(), browser, { admissionGuard })).projectId, first.projectId);
  await assert.rejects(registry.prepare('32', scope('c'), browser, { admissionGuard }), error => error.code === 'browser_memory_pressure');
});
