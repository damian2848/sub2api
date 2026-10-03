import test from 'node:test';
import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { AccountPoolManager } from '../src/pool.mjs';
import { ProjectRegistry, requestProjectScope } from '../src/projects.mjs';
import { PrismError, aborted } from '../src/errors.mjs';

const model = 'gpt-5.6-sol';
const credentials = { access_token: 't'.repeat(40), api_key: 'k'.repeat(40), expected_email: 'one@example.com',
  expires_at: Math.floor(Date.now() / 1000) + 3600 };
const scope = (key = 'a', session = 'b') => requestProjectScope({ 'x-prism-key-scope': key.repeat(64),
  'x-prism-session-scope': session.repeat(64) });
const request = (marker, projectScope = scope()) => ({ marker, projectScope, model, effort: 'low', input: [] });
function deferred() {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return { promise, resolve };
}

// Unlike the older pool/lifecycle fixtures, this keeps projectIsolation at its
// default true and gives every newly created native project a distinct UUID.
async function fixture(t, { concurrency = 2, generate, transientRetries = 1 } = {}) {
  const dataDir = await mkdtemp(join(tmpdir(), 'prism-isolated-lifecycle-'));
  const cleanup = [];
  const log = { drivers: [], initializations: [], userCalls: [], probes: [] };
  const manager = new AccountPoolManager({ dataDir, concurrency, transientRetries, projectIsolation: true,
    transientRetryDelayMs: 0, transientRetryWaitMs: 1000,
    browserFactory: async () => ({ on() {}, async close() {} }),
    sessionFactory: (_, heartbeat, source, slot) => {
      const closing = deferred();
      const driver = { source, slot, closed: false, projectId: null, lastHeartbeat: 0,
        page: { isClosed: () => driver.closed },
        async authenticate() { return 'stable-oauth-user'; },
        async initialize(existing, created, signal) {
          aborted(signal);
          this.projectId = existing || randomUUID();
          log.initializations.push({ slot, existing, project: this.projectId });
          if (!existing) await created(this.projectId);
          aborted(signal);
          if (this.closed) throw new PrismError('browser_session_closed', 503);
          this.lastHeartbeat = Math.floor(Date.now() / 1000);
          heartbeat(this.lastHeartbeat);
          return [model];
        },
        async generate(value, signal, progress) {
          aborted(signal);
          if (!value.marker) {
            log.probes.push({ slot, project: this.projectId });
            return 'READY';
          }
          const call = { slot, marker: value.marker, scope: value.projectScope.id, project: this.projectId };
          log.userCalls.push(call);
          return Promise.race([Promise.resolve().then(() => generate?.({ driver, request: value, signal, progress, call })
            ?? `reply:${value.marker}`), closing.promise.then(() => { throw new PrismError('session_closed', 503); })]);
        },
        async close() { this.closed = true; closing.resolve(); },
        isAlive() { return !this.closed; },
      };
      log.drivers.push(driver);
      return driver;
    } });
  t.after(async () => {
    for (const release of cleanup) release();
    await manager.close();
    await rm(dataDir, { recursive: true, force: true });
  });
  await manager.init();
  await manager.provision('32', credentials);
  await manager.bootstrap('32');
  return { manager, registry: manager.projects, dataDir, log, cleanup };
}

async function restoredEntries(dataDir) {
  const restored = new ProjectRegistry({ dataDir });
  await restored.init();
  return restored.entries('32');
}

test('retryConversation refreshes exactly the isolated user project, never a readiness project', { timeout: 5000 }, async t => {
  let attempts = 0;
  const { manager, log } = await fixture(t, { concurrency: 1, generate() {
    if (++attempts === 1) throw Object.assign(new PrismError('prism_generation_failed'), { retryConversation: true });
    return 'Final verified reply';
  } });
  const runtime = manager.primary.get('32');
  const before = structuredClone(runtime.metadata);
  assert.equal(await manager.generate('32', request('retry')), 'Final verified reply');
  assert.equal(log.userCalls.length, 2);
  const userProject = log.userCalls[0].project;
  assert.notEqual(userProject, before.project_id);
  assert.equal(log.userCalls[1].project, userProject);
  assert.equal(log.initializations.at(-1).existing, userProject);
  assert.equal(log.probes.length, 1, 'refresh is not another readiness probe');
  for (const field of ['project_id', 'verified_project', 'probe_attempted', 'readiness_probe_count']) {
    assert.equal(runtime.metadata[field], before[field], field);
  }
  assert.equal(manager.status('32').ready, true);
  assert.equal(manager.accounts.get('32').activeScopes.size, 0);
});

test('transient retry changes workers but keeps the scope project and the scope lock until the final result', { timeout: 5000 }, async t => {
  const firstFailed = deferred();
  const retried = deferred();
  const releaseRetry = deferred();
  let first = true;
  const active = new Map();
  let peak = 0;
  const { manager, log, cleanup } = await fixture(t, { generate: async ({ request: value, call }) => {
    active.set(call.scope, (active.get(call.scope) || 0) + 1);
    if (value.projectScope.id === scope().id) peak = Math.max(peak, active.get(call.scope));
    try {
      if (value.marker === 'first' && first) {
        first = false;
        firstFailed.resolve();
        throw Object.assign(new PrismError('prism_upstream_http_error'), { transient: true });
      }
      if (value.marker === 'first') { retried.resolve(); await releaseRetry.promise; }
      return `reply:${value.marker}`;
    } finally { active.set(call.scope, active.get(call.scope) - 1); }
  } });
  cleanup.push(releaseRetry.resolve);
  const firstRequest = manager.generate('32', request('first'));
  await firstFailed.promise;
  const same = manager.generate('32', request('same'));
  await retried.promise;
  assert.equal(log.userCalls.filter(call => call.marker === 'same').length, 0);
  const calls = log.userCalls.filter(call => call.marker === 'first');
  assert.equal(calls.length, 2);
  assert.notEqual(calls[0].slot, calls[1].slot);
  assert.equal(calls[0].project, calls[1].project);
  assert.ok(!log.probes.some(probe => probe.project === calls[0].project));
  assert.equal(manager.accounts.get('32').activeScopes.has(scope().id), true);
  assert.equal(await manager.generate('32', request('other', scope('c'))), 'reply:other');
  assert.equal(log.userCalls.filter(call => call.marker === 'same').length, 0, 'another scope completing must not release this lock');
  releaseRetry.resolve();
  assert.deepEqual(await Promise.all([firstRequest, same]), ['reply:first', 'reply:same']);
  assert.equal(log.userCalls.find(call => call.marker === 'same').project, calls[0].project);
  assert.equal(peak, 1, 'no two native turns for one scope can overlap');
  assert.equal(manager.accounts.get('32').activeScopes.size, 0);
});

test('a failed scoped turn releases its lock and permits the queued same-scope request', { timeout: 5000 }, async t => {
  const { manager, log } = await fixture(t, { generate({ request: value }) {
    if (value.marker === 'fails') throw new PrismError('prism_empty_output');
    return `reply:${value.marker}`;
  } });
  const failure = manager.generate('32', request('fails'));
  const rejected = assert.rejects(failure, error => error.code === 'prism_empty_output');
  const next = manager.generate('32', request('next'));
  await rejected;
  assert.equal(await next, 'reply:next');
  assert.equal(log.userCalls[0].project, log.userCalls[1].project);
  assert.equal(manager.accounts.get('32').activeScopes.size, 0);
});

test('cancelling a queued same-scope request never removes the active lock or blocks another scope', { timeout: 5000 }, async t => {
  const started = deferred();
  const release = deferred();
  const { manager, log, cleanup } = await fixture(t, { generate: async ({ request: value }) => {
    if (value.marker === 'active') { started.resolve(); await release.promise; }
    return `reply:${value.marker}`;
  } });
  cleanup.push(release.resolve);
  const active = manager.generate('32', request('active'));
  await started.promise;
  const controller = new AbortController();
  const cancelled = manager.generate('32', request('cancelled'), controller.signal);
  const rejected = assert.rejects(cancelled, error => error.code === 'request_cancelled');
  controller.abort();
  await rejected;
  assert.equal(manager.accounts.get('32').activeScopes.has(scope().id), true);
  assert.equal(await manager.generate('32', request('other', scope('c'))), 'reply:other');
  assert.ok(!log.userCalls.some(call => call.marker === 'cancelled'));
  release.resolve();
  assert.equal(await active, 'reply:active');
  assert.equal(await manager.generate('32', request('later')), 'reply:later');
  assert.equal(manager.accounts.get('32').activeScopes.size, 0);
});

// Pause after the actual atomic write, rather than just before initialize(). This
// catches races where cancellation/revocation arrives after a ready:true record
// reaches disk but before the post-persist ownership/version check.
for (const interruption of ['cancel', 'revoke']) {
  for (const ready of [false, true]) {
    test(`${interruption} during ${ready ? 'completion' : 'reservation'} persistence cannot generate or restore a ready project`,
      { timeout: 5000 }, async t => {
        const { manager, registry, dataDir, log, cleanup } = await fixture(t, { concurrency: 1 });
        const entered = deferred();
        const release = deferred();
        cleanup.push(release.resolve);
        const identity = scope();
        const persist = registry.persist.bind(registry);
        let paused = false;
        registry.persist = async (source, entries) => {
          await persist(source, entries);
          if (!paused && entries.get(identity.id)?.ready === ready) {
            paused = true;
            entered.resolve();
            await release.promise;
          }
        };
        const controller = new AbortController();
        const pending = manager.generate('32', request('interrupted', identity), controller.signal);
        const rejected = assert.rejects(pending, error => error.code === (interruption === 'cancel' ? 'request_timeout' : 'session_revoked'));
        await entered.promise;
        assert.equal(log.userCalls.length, 0);
        const revoked = interruption === 'revoke' ? manager.revoke('32') : null;
        if (interruption === 'cancel') controller.abort(new PrismError('request_timeout', 504));
        release.resolve();
        await rejected;
        await revoked;
        assert.equal(log.userCalls.length, 0, 'no native start may follow lost request/session ownership');
        assert.notEqual((await registry.entries('32')).get(identity.id)?.ready, true);
        assert.equal((await restoredEntries(dataDir)).has(identity.id), false, 'uncommitted preparation must not survive restart');
        assert.equal(manager.accounts.get('32').activeScopes.size, 0);
        if (interruption === 'revoke') {
          assert.equal(manager.primary.get('32').metadata.key_hash, null);
          assert.equal(manager.primary.get('32').driver, null);
          assert.equal(manager.status('32').ready, false);
        }
      });
  }
}

test('a failed completion persistence cannot expose a ready in-memory project to the next request', { timeout: 5000 }, async t => {
  const { manager, registry, dataDir, log } = await fixture(t, { concurrency: 1 });
  const identity = scope();
  const persist = registry.persist.bind(registry);
  let failed = false;
  registry.persist = async (source, entries) => {
    if (!failed && entries.get(identity.id)?.ready === true) {
      failed = true;
      throw new PrismError('project_persistence_failed');
    }
    return persist(source, entries);
  };
  await assert.rejects(manager.generate('32', request('failed-persistence', identity)), error => error.code === 'project_persistence_failed');
  assert.equal(log.userCalls.length, 0);
  assert.notEqual((await registry.entries('32')).get(identity.id)?.ready, true);
  assert.equal((await restoredEntries(dataDir)).has(identity.id), false);
  const failedProject = log.drivers[0].projectId;
  assert.equal(await manager.generate('32', request('retry-persistence', identity)), 'reply:retry-persistence');
  assert.notEqual(log.userCalls[0].project, failedProject, 'failed preparation cannot take the warm-page shortcut');
  assert.equal(manager.accounts.get('32').activeScopes.size, 0);
});

test('consecutive requests of one scope stay on the worker that already holds its project', { timeout: 5000 }, async t => {
  const { manager, log } = await fixture(t);
  assert.equal(await manager.generate('32', request('first')), 'reply:first');
  const holder = log.userCalls[0].slot;
  const project = log.userCalls[0].project;
  const initializations = log.initializations.length;
  for (const marker of ['second', 'third', 'fourth']) assert.equal(await manager.generate('32', request(marker)), `reply:${marker}`);
  assert.deepEqual(log.userCalls.map(call => call.slot), [holder, holder, holder, holder], 'no worker switch');
  assert.ok(log.userCalls.every(call => call.project === project));
  assert.equal(log.initializations.length, initializations, 'the prepared project is reused, not reloaded');
});

test('another scope is not pinned to that worker, and a busy holder falls back to a worker that loads the same project', { timeout: 5000 }, async t => {
  const { manager, log } = await fixture(t);
  await manager.generate('32', request('first'));
  const holder = log.userCalls[0].slot;
  const project = log.userCalls[0].project;
  const workers = manager.accounts.get('32').workers;
  // Anonymous traffic keeps rotating over the workers.
  const anonymous = [];
  for (const marker of ['a1', 'a2', 'a3', 'a4']) {
    await manager.generate('32', request(marker, requestProjectScope()));
    anonymous.push(log.userCalls.at(-1).slot);
  }
  assert.equal(new Set(anonymous).size, 2, 'unscoped requests still use every worker');
  // The holder is occupied elsewhere: the scope moves to the other worker and loads its own project there.
  workers[holder].busy = true;
  const before = log.initializations.length;
  assert.equal(await manager.generate('32', request('moved')), 'reply:moved');
  workers[holder].busy = false;
  const moved = log.userCalls.at(-1);
  assert.notEqual(moved.slot, holder);
  assert.equal(moved.project, project, 'same project, loaded by the other worker');
  assert.deepEqual(log.initializations.slice(before).map(item => [item.slot, item.existing]), [[moved.slot, project]]);
  // Once the holder is free the scope goes to whichever worker holds the project now: the one it moved to.
  assert.equal(await manager.generate('32', request('again')), 'reply:again');
  assert.equal(log.userCalls.at(-1).slot, moved.slot);
});

test('without project isolation worker choice stays plain round-robin', { timeout: 5000 }, async t => {
  const dataDir = await mkdtemp(join(tmpdir(), 'prism-no-affinity-'));
  const slots = [];
  const manager = new AccountPoolManager({ dataDir, concurrency: 2, projectIsolation: false,
    browserFactory: async () => ({ on() {}, async close() {} }),
    sessionFactory: (_, heartbeat, source, slot) => ({ source, slot, projectId: null, lastHeartbeat: 0, page: { isClosed: () => false },
      async authenticate() { return 'stable-oauth-user'; },
      async initialize(existing, created) { this.projectId = existing || randomUUID(); if (!existing) await created(this.projectId);
        this.lastHeartbeat = Math.floor(Date.now() / 1000); heartbeat(this.lastHeartbeat); return [model]; },
      async generate(value) { if (!value.marker) return 'READY'; slots.push(slot); return 'ok'; },
      async close() {}, isAlive() { return true; } }) });
  t.after(async () => { await manager.close(); await rm(dataDir, { recursive: true, force: true }); });
  await manager.init(); await manager.provision('32', credentials); await manager.bootstrap('32');
  for (const marker of ['1', '2', '3', '4']) await manager.generate('32', { marker, model, effort: 'low', input: [] });
  assert.equal(new Set(slots).size, 2);
});
