import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { AccountPoolManager } from '../src/pool.mjs';
import { RuntimeMetrics } from '../src/resources.mjs';
import { PrismError } from '../src/errors.mjs';

const creds = { access_token: 'a'.repeat(40), api_key: 'k'.repeat(40), expected_email: 'one@example.com',
  expires_at: Math.floor(Date.now() / 1000) + 3600 };
async function fixture(t, options = {}) {
  const dataDir = await mkdtemp(join(tmpdir(), 'prism-pool-resources-'));
  t.after(() => rm(dataDir, { recursive: true, force: true }));
  const sessions = [];
  const browsers = [];
  const metrics = new RuntimeMetrics();
  const manager = new AccountPoolManager({ dataDir, concurrency: 2, projectIsolation: false, metrics,
    browserFactory: async () => {
      const browser = { closed: false, on() {}, async close() { this.closed = true; } };
      browsers.push(browser);
      return browser;
    },
    sessionFactory: (_, heartbeat, source, slot, browserOptions) => {
      const value = { source, slot, browserOptions, lastHeartbeat: 0, closed: false,
        async authenticate() { return 'stable-source-user'; },
        async initialize(project, reserve) {
          if (!project) await reserve(randomUUID());
          this.lastHeartbeat = Math.floor(Date.now() / 1000); heartbeat(this.lastHeartbeat);
          return ['gpt-5.6-sol'];
        },
        async generate() { return 'READY'; },
        async close() { this.closed = true; }, isAlive() { return !this.closed; } };
      sessions.push(value); return value;
    }, ...options });
  await manager.init();
  t.after(() => manager.close());
  return { manager, sessions, browsers, metrics };
}

test('disabling the pool releases browser resources and enabling it initializes a fresh browser', async t => {
  const { manager, sessions, browsers } = await fixture(t);
  await manager.provision('32', creds);
  assert.equal(browsers.length, 1);
  assert.equal((await manager.resources()).contexts, 2);

  await manager.setEnabled(false);
  assert.equal(manager.isEnabled(), false);
  assert.equal(browsers[0].closed, true);
  assert.equal(sessions.every(session => session.closed), true);
  assert.equal((await manager.resources()).contexts, 0);
  await assert.rejects(manager.provision('32', creds), error => error.code === 'prism_disabled');

  await manager.setEnabled(true);
  assert.equal(manager.isEnabled(), true);
  assert.equal(browsers.length, 2);
  await manager.provision('32', creds);
  assert.equal((await manager.resources()).contexts, 2);
  assert.equal(sessions.slice(-2).every(session => !session.closed), true);
});

test('rapid toggles serialize and the final switch state wins', async t => {
  let release;
  const { manager } = await fixture(t, { browserFactory: async () => ({ on() {}, async close() {} }) });
  const originalRelease = manager.releaseResources.bind(manager);
  manager.releaseResources = async () => {
    await new Promise(resolve => { release = resolve; });
    return originalRelease();
  };
  const disabling = manager.setEnabled(false);
  const enabling = manager.setEnabled(true);
  const disablingAgain = manager.setEnabled(false);
  await new Promise(resolve => setImmediate(resolve));
  release();
  await Promise.all([disabling, enabling, disablingAgain]);
  assert.equal(manager.isEnabled(), false);
  assert.equal(manager.browserPromise, null);
});

test('resident reservations count toward the global context capacity before authentication', async t => {
  const { manager, sessions } = await fixture(t, { multiplex: true, maxWorkers: 2 });
  await manager.provision('32', creds);
  assert.equal(sessions.length, 1, 'one worker + one reserved resident consumes both slots');
  assert.equal(sessions[0].browserOptions.multiplex, true);
  assert.equal((await manager.resources()).reserved_contexts, 2);
  await manager.bootstrap('32');
  assert.equal(manager.status('32').ready_workers, 1);
  await assert.rejects(manager.provision('33', creds), error => error.code === 'browser_capacity_full');
  assert.equal(sessions.length, 1);
});

test('workers share only their source poller, and revoke/reprovision cannot reuse a closed poller', async t => {
  const { manager, sessions } = await fixture(t, { multiplex: true });
  await manager.provision('32', creds);
  await manager.provision('33', creds);
  const first = sessions.find(value => value.source === '32').browserOptions.multiplexer;
  assert.equal(sessions.filter(value => value.source === '32').every(value => value.browserOptions.multiplexer === first), true);
  assert.notEqual(sessions.find(value => value.source === '33').browserOptions.multiplexer, first);
  await manager.revoke('32');
  assert.equal(first.closed, true);
  await manager.provision('32', creds);
  assert.notEqual(sessions.filter(value => value.source === '32').at(-1).browserOptions.multiplexer, first);
});

test('queue metrics record first admission once, not retry generation as additional queue wait', async t => {
  let tries = 0;
  const { manager, metrics } = await fixture(t, { transientRetryDelayMs: 0, transientRetryWaitMs: 0 });
  await manager.provision('32', creds);
  await manager.bootstrap('32');
  for (const runtime of manager.managers) {
    const browser = runtime.get('32').driver;
    const original = browser.generate;
    browser.generate = async (request, ...args) => {
      if (++tries === 1) { const error = new PrismError('prism_upstream_http_error'); error.transient = true; throw error; }
      return original(request, ...args);
    };
  }
  assert.equal(await manager.generate('32', { model: 'gpt-5.6-sol', input: [] }), 'READY');
  assert.equal(metrics.snapshot().queue_wait_ms.count, 1);
  assert.equal(metrics.snapshot().worker_turn_ms.count, 2);
});
