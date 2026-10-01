import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { AccountPoolManager } from '../src/pool.mjs';
import { AccountQueue } from '../src/queue.mjs';
import { PrismError } from '../src/errors.mjs';
import { NativeStartLimiter } from '../src/start-limit.mjs';

const model = 'gpt-5.6-sol';
const terra = 'gpt-5.6-terra';
const body = token => ({ access_token: (token || 'a').repeat(40), api_key: 'k'.repeat(40),
  expected_email: 'one@example.com', expires_at: Math.floor(Date.now() / 1000) + 3600 });
const request = marker => ({ model, effort: 'low', input: [], marker });
function deferred() {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return { promise, resolve };
}

async function fixture(t, { concurrency = 2, maxWorkers = 32, queueLimit = 8, models, generate, failSlot, identity,
  startLimiterFactory } = {}) {
  const dataDir = await mkdtemp(join(tmpdir(), 'prism-pool-test-'));
  t.after(() => rm(dataDir, { recursive: true, force: true }));
  const log = { drivers: [], projects: [], probes: [], generations: [], closes: 0 };
  const settings = { dataDir, concurrency, maxWorkers, queueLimit, startLimiterFactory,
    browserFactory: async () => ({ on() {}, async close() {} }),
    sessionFactory: (_, heartbeat, source, slot) => {
      const closing = deferred();
      const driver = { source, slot, closed: false, lastHeartbeat: 0,
        async authenticate() {
          if (slot === failSlot) throw new PrismError('oauth_session_rejected', 401);
          return identity?.(source, slot) || 'stable-user';
        },
        async initialize(existing, created) {
          const project = existing || `${String(slot).padStart(8, '0')}-89ab-4cde-8123-${source.padStart(12, '0')}`;
          if (!existing) await created(project);
          log.projects.push({ source, slot, project });
          this.lastHeartbeat = Math.floor(Date.now() / 1000);
          heartbeat(this.lastHeartbeat);
          return models?.(source, slot) || [model];
        },
        async generate(value, signal, progress) {
          if (!value.marker) { log.probes.push({ source, slot }); return 'READY'; }
          log.generations.push({ source, slot, marker: value.marker });
          return Promise.race([generate?.({ source, slot, request: value, signal, progress, driver }) ||
            Promise.resolve(`reply:${value.marker}`), closing.promise.then(() => { throw new PrismError('session_closed', 503); })]);
        },
        async close() { if (!this.closed) { this.closed = true; log.closes += 1; closing.resolve(); } },
        isAlive() { return !this.closed; },
      };
      log.drivers.push(driver);
      return driver;
    } };
  const manager = new AccountPoolManager(settings);
  await manager.init();
  t.after(() => manager.close());
  await manager.provision('32', body());
  await manager.bootstrap('32');
  return { manager, settings, dataDir, log };
}

test('workers use independent contexts and persisted projects, with one probe per project across restart', async t => {
  const { manager, log, dataDir, settings } = await fixture(t);
  assert.equal(log.drivers.length, 2);
  assert.equal(new Set(log.projects.map(item => item.project)).size, 2);
  const primary = JSON.parse(await readFile(join(dataDir, '32.json'), 'utf8'));
  const secondary = JSON.parse(await readFile(join(dataDir, 'workers', '1', '32.json'), 'utf8'));
  assert.notEqual(primary.project_id, secondary.project_id);
  assert.equal(primary.schema, 1);
  assert.equal(secondary.schema, 1);
  assert.equal(log.probes.length, 2);
  await manager.close();
  const restarted = new AccountPoolManager(settings);
  await restarted.init();
  t.after(() => restarted.close());
  await restarted.provision('32', body());
  assert.equal(restarted.status('32').concurrency, 2);
  assert.equal(log.probes.length, 2, 'restored verified projects must not repeat model probes');
});

test('two requests overlap on distinct workers and queued cancellation does not consume a slot', async t => {
  const both = deferred();
  const release = deferred();
  let entered = 0;
  const { manager, log } = await fixture(t, { generate: async ({ request }) => {
    if (++entered === 2) both.resolve();
    await release.promise;
    return `reply:${request.marker}`;
  } });
  const first = manager.generate('32', request('first'));
  const second = manager.generate('32', request('second'));
  await both.promise;
  assert.equal(manager.status('32').busy_workers, 2);
  assert.equal(new Set(log.generations.map(item => item.slot)).size, 2);
  const controller = new AbortController();
  const third = manager.generate('32', request('cancelled'), controller.signal);
  const rejected = assert.rejects(third, error => error.code === 'request_cancelled');
  assert.equal(manager.status('32').queued, 1);
  controller.abort();
  await rejected;
  assert.equal(manager.status('32').queued, 0);
  release.resolve();
  assert.deepEqual(await Promise.all([first, second]), ['reply:first', 'reply:second']);
  assert.equal(log.generations.length, 2);
});

test('native allowance includes probes and both workers, cancels waits on revoke, and isolates sources', async t => {
  let created = 0;
  const { manager, log } = await fixture(t, { startLimiterFactory: () => {
    created += 1;
    return new NativeStartLimiter({ limit: 2, windowMs: 60000 });
  } });
  assert.equal(created, 1);
  assert.equal(log.probes.length, 2);
  const controller = new AbortController();
  const first = manager.generate('32', request('cancelled'), controller.signal);
  const second = manager.generate('32', request('revoked'));
  const firstRejected = assert.rejects(first, error => error.code === 'request_cancelled');
  const secondRejected = assert.rejects(second, error => error.code === 'session_revoked');
  await new Promise(setImmediate);
  assert.equal(manager.status('32').busy_workers, 2);
  assert.equal(log.generations.length, 0, 'admission must precede native UI submission');
  controller.abort();
  await firstRejected;
  await manager.revoke('32');
  await secondRejected;
  await manager.provision('32', body());
  assert.equal(created, 1, 'reauthorization must retain the source allowance');
  const blocked = new AbortController();
  const later = manager.generate('32', request('still-limited'), blocked.signal);
  const laterRejected = assert.rejects(later, error => error.code === 'request_cancelled');
  await new Promise(setImmediate);
  assert.equal(log.generations.length, 0);
  blocked.abort();
  await laterRejected;
  await manager.provision('33', body());
  await manager.bootstrap('33');
  assert.equal(created, 2);
  assert.equal(log.probes.filter(item => item.source === '33').length, 2);
});

test('the existing single native resubmission consumes the same source allowance', async t => {
  let attempts = 0;
  const { manager, log } = await fixture(t, {
    startLimiterFactory: () => new NativeStartLimiter({ limit: 4, windowMs: 60000 }),
    generate: async () => {
      if (++attempts === 1) {
        const error = new PrismError('prism_generation_failed');
        error.retryConversation = true;
        throw error;
      }
      return 'recovered';
    },
  });
  assert.equal(await manager.generate('32', request('retry')), 'recovered');
  assert.equal(log.probes.length, 2);
  assert.equal(attempts, 2);
  const controller = new AbortController();
  const pending = manager.generate('32', request('after-retry'), controller.signal);
  const rejected = assert.rejects(pending, error => error.code === 'request_cancelled');
  await new Promise(setImmediate);
  assert.equal(attempts, 2);
  controller.abort();
  await rejected;
});

test('an OAuth token that expires during native admission is never submitted', async t => {
  const admission = deferred();
  const entered = deferred();
  let calls = 0;
  const { manager, log } = await fixture(t, { startLimiterFactory: () => ({
    acquire() {
      if (++calls <= 2) return Promise.resolve(0);
      entered.resolve();
      return admission.promise;
    },
    cancelPending() {}, close() {},
  }) });
  const pending = manager.generate('32', request('expired'));
  const rejected = assert.rejects(pending, error => error.code === 'account_not_ready');
  await entered.promise;
  manager.primary.get('32').expiresAt = Date.now() / 1000 - 1;
  admission.resolve(100);
  await rejected;
  assert.equal(log.generations.length, 0);
});

test('full queue revocation cancels active and pending work and durably removes every key', async t => {
  const entered = deferred();
  let started = 0;
  const { manager, dataDir } = await fixture(t, { queueLimit: 1, generate: async () => {
    if (++started === 2) entered.resolve();
    await new Promise(() => {});
  } });
  const first = manager.generate('32', request('first'));
  const second = manager.generate('32', request('second'));
  await entered.promise;
  const third = manager.generate('32', request('queued'));
  await assert.rejects(manager.generate('32', request('overflow')), error => error.code === 'account_queue_full');
  const pendingRejected = assert.rejects(third, error => error.code === 'session_revoked');
  const firstRejected = assert.rejects(first, error => ['session_revoked', 'session_closed'].includes(error.code));
  const secondRejected = assert.rejects(second, error => ['session_revoked', 'session_closed'].includes(error.code));
  await manager.revoke('32');
  await Promise.all([pendingRejected, firstRejected, secondRejected]);
  assert.throws(() => manager.authenticateKey('32', body().api_key), error => error.code === 'account_not_ready');
  assert.equal(manager.status('32').ready, false);
  assert.equal(manager.drivers.size, 0);
  for (const filename of [join(dataDir, '32.json'), join(dataDir, 'workers', '1', '32.json')]) {
    assert.equal(JSON.parse(await readFile(filename, 'utf8')).key_hash, null);
  }
  await manager.provision('32', body());
  assert.equal(manager.status('32').ready, true);
  assert.equal(manager.status('32').concurrency, 2);
});

test('worker resource limit includes authenticating contexts and partial pools remain usable', async t => {
  const { manager, log } = await fixture(t, { maxWorkers: 1 });
  assert.equal(manager.drivers.size, 1);
  assert.equal(log.drivers.length, 1);
  assert.equal(manager.status('32').concurrency, 1);
  assert.equal(await manager.generate('32', request('single')), 'reply:single');
  await manager.bootstrap('32');
  assert.equal(manager.drivers.size, 1);
  assert.equal(log.probes.length, 1);
});

test('secondary authentication failure does not disable the primary worker', async t => {
  const { manager } = await fixture(t, { failSlot: 1 });
  assert.equal(manager.status('32').concurrency, 1);
  assert.equal(await manager.generate('32', request('healthy')), 'reply:healthy');
});

test('a secondary identity mismatch is revoked without authorizing that worker', async t => {
  const { manager, dataDir } = await fixture(t, { identity: (_, slot) => slot ? 'another-user' : 'stable-user' });
  assert.equal(manager.status('32').concurrency, 1);
  assert.equal(manager.drivers.size, 1);
  const metadata = JSON.parse(await readFile(join(dataDir, 'workers', '1', '32.json'), 'utf8'));
  assert.equal(metadata.key_hash, null);
  assert.equal(await manager.generate('32', request('identity-safe')), 'reply:identity-safe');
});

test('expired leases reject waiting work without starting another native generation', async t => {
  const entered = deferred();
  const release = deferred();
  const { manager, log } = await fixture(t, { concurrency: 1, generate: async () => {
    entered.resolve(); await release.promise; return 'late';
  } });
  const first = manager.generate('32', request('active'));
  await entered.promise;
  const waiting = manager.generate('32', request('waiting'));
  const firstRejected = assert.rejects(first, error => error.code === 'account_not_ready');
  const waitingRejected = assert.rejects(waiting, error => error.code === 'account_not_ready');
  manager.primary.get('32').expiresAt = Math.floor(Date.now() / 1000) - 1;
  release.resolve();
  await Promise.all([firstRejected, waitingRejected]);
  assert.equal(log.generations.length, 1);
  assert.equal(manager.status('32').concurrency, 0);
});

test('published progress disables native resubmission even for the exact eligible terminal error', async t => {
  const seen = [];
  const { manager, log } = await fixture(t, { generate: async ({ progress }) => {
    progress('public text');
    const error = new PrismError('prism_generation_failed');
    error.retryConversation = true;
    throw error;
  } });
  await assert.rejects(manager.generate('32', request('published'), undefined, text => seen.push(text)),
    error => error.code === 'prism_generation_failed');
  assert.deepEqual(seen, ['public text']);
  assert.equal(log.generations.length, 1);
  assert.equal(log.projects.length, 2);
});

test('scheduler serves another model while its oldest waiter is waiting for a busy compatible worker', async t => {
  const held = deferred();
  const entered = deferred();
  const { manager, log } = await fixture(t, { models: (_, slot) => slot ? [terra] : [model],
    generate: async ({ request }) => {
      if (request.marker === 'held') { entered.resolve(); await held.promise; }
      return `reply:${request.marker}`;
    } });
  const first = manager.generate('32', request('held'));
  await entered.promise;
  const queued = manager.generate('32', request('queued'));
  const other = manager.generate('32', { ...request('terra'), model: terra });
  assert.equal(await other, 'reply:terra');
  assert.equal(log.generations.find(item => item.marker === 'terra').slot, 1);
  held.resolve();
  assert.deepEqual(await Promise.all([first, queued]), ['reply:held', 'reply:queued']);
});

test('credential rotation waits for active leases and warms existing projects without another model probe', async t => {
  const entered = deferred();
  const release = deferred();
  const { manager, log } = await fixture(t, { generate: async ({ request }) => {
    if (request.marker === 'old') { entered.resolve(); await release.promise; }
    return `reply:${request.marker}`;
  } });
  const first = manager.generate('32', request('old'));
  await entered.promise;
  const rotation = manager.provision('32', body('b'));
  const after = manager.generate('32', request('new'));
  assert.equal(log.closes, 0, 'credential rotation must not interrupt an active request');
  release.resolve();
  assert.equal(await first, 'reply:old');
  await rotation;
  assert.equal(await after, 'reply:new');
  assert.equal(log.probes.length, 2);
  assert.equal(manager.status('32').concurrency, 2);
  assert.equal(manager.drivers.size, 2);
});

test('same credentials refresh expiry without waiting for active generations', async t => {
  const entered = deferred();
  const release = deferred();
  const { manager, log } = await fixture(t, { generate: async () => {
    entered.resolve();
    await release.promise;
    return 'finished';
  } });
  const active = manager.generate('32', request('active'));
  await entered.promise;
  const updated = { ...body(), expires_at: Math.floor(Date.now() / 1000) + 7200 };
  assert.equal((await manager.provision('32', updated)).busy_workers, 1);
  assert.equal(log.closes, 0);
  assert.equal(log.drivers.length, 2);
  for (const worker of manager.managers) assert.equal(worker.get('32').expiresAt, updated.expires_at);
  release.resolve();
  assert.equal(await active, 'finished');
});

test('same credentials repair a dead worker and keep the verified project', async t => {
  const { manager, log } = await fixture(t);
  await manager.managers[1].get('32').driver.close();
  assert.equal(manager.status('32').concurrency, 1);
  await manager.provision('32', body());
  assert.equal(manager.status('32').concurrency, 2);
  assert.equal(log.drivers.length, 3);
  assert.equal(log.probes.length, 2);
});

test('periodic bootstrap of a healthy pool does not create a barrier behind active generations', async t => {
  const entered = deferred();
  const release = deferred();
  const { manager, log } = await fixture(t, { generate: async () => {
    entered.resolve(); await release.promise; return 'finished';
  } });
  const active = manager.generate('32', request('active'));
  await entered.promise;
  const status = await manager.bootstrap('32');
  assert.equal(status.busy_workers, 1);
  assert.equal(log.projects.length, 2);
  assert.equal(log.probes.length, 2);
  assert.equal(manager.runtime('32').queue.jobs.length, 0);
  release.resolve();
  assert.equal(await active, 'finished');
});

test('four-worker cap is shared across sources and revocation releases its contexts', async t => {
  const { manager, log } = await fixture(t, { concurrency: 4, maxWorkers: 4 });
  assert.equal(manager.status('32').concurrency, 4);
  assert.equal(new Set(log.projects.map(item => item.project)).size, 4);
  await assert.rejects(manager.provision('33', body()), error => error.code === 'browser_capacity_full');
  assert.equal(manager.drivers.size, 4);
  await manager.revoke('32');
  assert.equal(manager.drivers.size, 0);
  await manager.provision('33', body());
  await manager.bootstrap('33');
  assert.equal(manager.status('33').concurrency, 4);
  assert.equal(manager.status('32').concurrency, 0);
  assert.equal(await manager.generate('33', request('isolated')), 'reply:isolated');
});

for (const operation of ['revoke', 'close']) test(`${operation} during authentication cannot publish or leak the new session`, async t => {
  const { manager } = await fixture(t);
  const entered = deferred();
  const release = deferred();
  const original = manager.sessionFactory;
  manager.sessionFactory = (...args) => {
    const driver = original(...args);
    const authenticate = driver.authenticate.bind(driver);
    driver.authenticate = async (...values) => { entered.resolve(); await release.promise; return authenticate(...values); };
    return driver;
  };
  const rotation = manager.provision('32', body('b'));
  const rejected = assert.rejects(rotation, error => ['session_revoked', 'service_stopping'].includes(error.code));
  await entered.promise;
  const closing = operation === 'close' ? manager.close() : manager.revoke('32');
  release.resolve();
  await Promise.all([rejected, closing]);
  assert.equal(manager.drivers.size, 0);
  for (const worker of manager.managers) assert.equal(worker.accounts.get('32').driver, null);
  if (operation === 'revoke') assert.throws(() => manager.authenticateKey('32', body().api_key), error => error.code === 'account_not_ready');
});

test('shutdown closes a delayed shared Chromium launch and rejects future launches', async t => {
  const { settings } = await fixture(t);
  const launch = deferred();
  let closes = 0;
  const manager = new AccountPoolManager({ ...settings, browserFactory: () => launch.promise });
  const starting = manager.getBrowser();
  const rejected = assert.rejects(starting, error => error.code === 'service_stopping');
  const closing = manager.close();
  launch.resolve({ on() {}, async close() { closes += 1; } });
  await Promise.all([rejected, closing]);
  assert.equal(closes, 1);
  assert.equal(manager.browserPromise, null);
  await assert.rejects(manager.getBrowser(), error => error.code === 'service_stopping');
});

test('shutdown during initialization cannot publish late model metadata or a probe', async t => {
  const { manager, log, dataDir } = await fixture(t);
  const entered = deferred();
  const release = deferred();
  const driver = manager.primary.get('32').driver;
  driver.initialize = async () => { entered.resolve(); await release.promise; return [terra]; };
  const initializing = manager.bootstrap('32', undefined, { retry_probe: true });
  const rejected = assert.rejects(initializing, error => error.code === 'session_revoked');
  await entered.promise;
  await manager.close();
  release.resolve();
  await rejected;
  assert.deepEqual(JSON.parse(await readFile(join(dataDir, '32.json'), 'utf8')).models, [model]);
  assert.equal(log.probes.length, 2);
  assert.equal(manager.primary.accounts.get('32').ready, false);
});

test('one worker conversation recovery does not interrupt the other worker', async t => {
  const { manager } = await fixture(t);
  const failed = manager.managers[0].get('32').driver;
  const entered = deferred();
  const release = deferred();
  const originalInitialize = failed.initialize.bind(failed);
  const originalGenerate = failed.generate.bind(failed);
  let calls = 0;
  failed.generate = async (...args) => {
    if (++calls === 1) { const error = new PrismError('prism_generation_failed'); error.retryConversation = true; throw error; }
    return originalGenerate(...args);
  };
  failed.initialize = async (...args) => { entered.resolve(); await release.promise; return originalInitialize(...args); };
  const recovering = manager.generate('32', request('recovering'));
  await entered.promise;
  assert.equal(manager.status('32').ready, true);
  assert.equal(await manager.generate('32', request('healthy')), 'reply:healthy');
  release.resolve();
  assert.equal(await recovering, 'reply:recovering');
  assert.equal(calls, 2);
});

test('exclusive queue operations form a barrier while independent ready jobs can overlap', async () => {
  const queue = new AccountQueue(4, 2);
  const release = deferred();
  const order = [];
  const first = queue.run(async () => { order.push('first'); await release.promise; });
  const second = queue.run(async () => { order.push('second'); await release.promise; });
  const control = queue.exclusive(async () => { order.push('control'); });
  const last = queue.run(async () => { order.push('last'); });
  assert.deepEqual(order, ['first', 'second']);
  release.resolve();
  await Promise.all([first, second, control, last]);
  assert.deepEqual(order, ['first', 'second', 'control', 'last']);
  queue.close();
});
