import test from 'node:test';
import assert from 'node:assert/strict';
import { memorySnapshot, ResourceGuard, RuntimeMetrics } from '../src/resources.mjs';

const fixtureRead = files => async path => { if (!(path in files)) throw new Error('ENOENT'); return files[path]; };

test('memory sampling resolves Linux cgroup v2 process membership and includes browser memory', async () => {
  const memory = await memorySnapshot({ platform: 'linux', read: fixtureRead({ '/proc/self/cgroup': '0::/sidecar\n',
    '/sys/fs/cgroup/sidecar/memory.current': '701234567\n', '/sys/fs/cgroup/sidecar/memory.max': '1073741824\n' }),
    run: () => assert.fail('cgroup is authoritative') });
  assert.deepEqual(memory, { usedBytes: 701234567, limitBytes: 1073741824, source: 'cgroup_v2', includesChromium: true, degraded: false });
});

test('cgroup v1 fallback reads memory usage and ignores unlimited sentinel', async () => {
  const memory = await memorySnapshot({ platform: 'linux', read: fixtureRead({ '/proc/self/cgroup': '4:cpu:/sidecar\n5:memory:/prism\n',
    '/sys/fs/cgroup/memory/prism/memory.usage_in_bytes': '555', '/sys/fs/cgroup/memory/prism/memory.limit_in_bytes': '9223372036854771712' }) });
  assert.equal(memory.source, 'cgroup_v1'); assert.equal(memory.usedBytes, 555); assert.equal(memory.limitBytes, null);
});

test('local fallback accounts for Chromium descendants and labels sampling degraded', async () => {
  const memory = await memorySnapshot({ platform: 'darwin', pid: 100, run: async () => ({ stdout:
    '100 1 1000\n101 100 5000\n102 101 9000\n200 1 900000\n' }) });
  assert.deepEqual(memory, { usedBytes: 15000 * 1024, limitBytes: null, source: 'process_tree_rss', includesChromium: true, degraded: true });
});

test('unavailable local process tree explicitly falls back to Node RSS only', async () => {
  const memory = await memorySnapshot({ platform: 'win32', run: async () => { throw new Error('no ps'); }, rss: () => 123 });
  assert.equal(memory.source, 'node_rss_only'); assert.equal(memory.includesChromium, false); assert.equal(memory.degraded, true);
});

test('memory guard rejects new preparation under cgroup threshold without touching in-flight work', async () => {
  let used = 100; const events = [];
  const guard = new ResourceGuard({ limitBytes: 750, reserveBytes: 10, sample: async () => ({ usedBytes: used,
    limitBytes: 500, source: 'cgroup_v2', includesChromium: true, degraded: false }), onAudit: (...args) => events.push(args) });
  await guard.assertAdmission('context');
  used = 490;
  await assert.rejects(guard.assertAdmission('project'), error => error.code === 'browser_memory_pressure' && error.status === 429 && error.retryAfterSeconds === 10);
  assert.equal((await guard.snapshot()).rejections, 1); assert.equal(events[1][1].thresholdBytes, 500);
  used = 480; await guard.assertAdmission('page');
});

test('metrics are bounded and include no request content or credentials', () => {
  const metrics = new RuntimeMetrics({ capacity: 2 });
  metrics.record('page_load', 10, { worker: 0, secret: 'token', source: 'account-email' });
  metrics.record('page_load', 20, { worker: 1 }); metrics.record('page_load', 30, { multiplex: true });
  metrics.record('unsafe-secret', 100); metrics.record('invalid', -1);
  assert.deepEqual(metrics.snapshot(), { page_load: { count: 3, mean_ms: 20, p50_ms: 20, p95_ms: 30, last: { ms: 30, multiplex: true } } });
});

test('metrics sample capacity must be a positive bounded integer', () => {
  for (const capacity of [0, -1, 1.5, NaN, Infinity, 100001]) assert.throws(() => new RuntimeMetrics({ capacity }), /invalid_metrics_capacity/);
});
