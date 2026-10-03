import test from 'node:test';
import assert from 'node:assert/strict';
import { ResourceGuard } from '../src/resources.mjs';
import { httpCacheEnabled } from '../src/cache-interceptor.mjs';
import { projectIsolationEnabled } from '../src/projects.mjs';
import { multiplexEnabled } from '../src/page-multiplexer.mjs';
import { BrowserSession } from '../src/browser.mjs';
import { AccountPoolManager } from '../src/pool.mjs';

// The hardening features are opt-in: with no configuration the sidecar behaves as it did in production
// before they existed. Each switch is turned on only after its numbers are measured on a real account.

test('every hardening switch is off when nothing is configured', () => {
  for (const unset of [undefined, '', 'false', '0', 'off', 'no']) {
    assert.equal(httpCacheEnabled(unset), false);
    assert.equal(projectIsolationEnabled(unset), false);
    assert.equal(multiplexEnabled(unset), false);
  }
  for (const on of ['true', '1', 'on', ' TRUE ']) {
    assert.equal(httpCacheEnabled(on), true);
    assert.equal(projectIsolationEnabled(on), true);
  }
});

test('a default memory guard never refuses admission, samples nothing and audits nothing', async () => {
  let sampled = 0;
  let audited = 0;
  const guard = new ResourceGuard({ sample: async () => { sampled += 1; return { usedBytes: 64 * 1024 ** 3, limitBytes: null }; },
    onAudit: () => { audited += 1; } });
  for (const kind of ['context', 'page', 'project']) assert.equal(await guard.assertAdmission(kind), null);
  assert.equal(sampled, 0);
  assert.equal(audited, 0);
  assert.equal(guard.rejections, 0);
});

test('an explicitly configured memory guard still refuses admission under pressure', async () => {
  const guard = new ResourceGuard({ limitBytes: 100, reserveBytes: 10, sample: async () => ({ usedBytes: 95, limitBytes: null }) });
  await assert.rejects(guard.assertAdmission('page'), error => error.code === 'browser_memory_pressure');
  assert.equal(guard.rejections, 1);
});

test('the default browser context keeps its original options (no service-worker blocking)', async () => {
  let seen;
  const browser = { newContext: async options => { seen = options; throw new Error('stop after recording options'); } };
  const session = new BrowserSession(browser, () => {}, '32');
  await assert.rejects(session.authenticate({ access_token: 'token' }));
  assert.deepEqual(seen, { locale: 'en-US' });

  const cached = new BrowserSession(browser, () => {}, '32', 0, { httpCache: true });
  await assert.rejects(cached.authenticate({ access_token: 'token' }));
  assert.deepEqual(seen, { locale: 'en-US', serviceWorkers: 'block' });
});

test('a default pool does not isolate projects', () => {
  const manager = new AccountPoolManager({ dataDir: '/nonexistent-prism-defaults' });
  assert.equal(manager.projectIsolation, false);
  assert.equal(manager.multiplex, false);
});
