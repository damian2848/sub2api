import test from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:net';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { fileURLToPath } from 'node:url';
import { configurationFromEnvironment } from '../src/configuration.mjs';

const managementKey = 'startup-fixture-management-key'.repeat(2);
const entrypoint = fileURLToPath(new URL('../src/server.mjs', import.meta.url));
const defaults = configurationFromEnvironment({});
const changed = { ...defaults, project_isolation: true, http_cache: true, memory_limit_mib: 2048,
  multiplex_pages: true, prewarm_chat: false, stream_reasoning: false };

async function childEnvironment(dataDir) {
  const listener = createServer();
  listener.listen(0, '127.0.0.1'); await once(listener, 'listening');
  const port = listener.address().port;
  await new Promise(resolve => listener.close(resolve));
  return { ...Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith('PRISM_'))),
    PRISM_DATA_DIR: dataDir, PRISM_HOST: '127.0.0.1', PRISM_PORT: String(port), PRISM_MANAGEMENT_KEY: managementKey };
}

async function launch(t, dataDir) {
  const env = await childEnvironment(dataDir);
  const child = spawn(process.execPath, [entrypoint], { env, stdio: ['ignore', 'pipe', 'pipe'] });
  let stdout = ''; let stderr = '';
  child.stdout.on('data', chunk => { stdout += chunk; });
  child.stderr.on('data', chunk => { stderr += chunk; });
  const exit = once(child, 'exit');
  const stop = async () => {
    if (child.exitCode !== null || child.signalCode !== null) return;
    child.kill('SIGTERM');
    const timer = setTimeout(() => child.kill('SIGKILL'), 5000);
    try { await exit; } finally { clearTimeout(timer); }
  };
  t.after(stop);
  await new Promise((resolve, reject) => {
    const timeout = setTimeout(() => reject(new Error('offline sidecar startup timeout')), 10000);
    const check = chunk => {
      if (stdout.includes('"event":"listening"')) { clearTimeout(timeout); child.stdout.off('data', check); resolve(); }
    };
    child.stdout.on('data', check);
    exit.then(() => { clearTimeout(timeout); reject(new Error('offline sidecar startup failed: ' + stderr)); });
  });
  const base = `http://127.0.0.1:${env.PRISM_PORT}`;
  const call = async (method = 'GET', body, path = '/internal/config') => {
    const response = await fetch(base + path, { method, headers: {
      Authorization: `Bearer ${managementKey}`, 'Content-Type': 'application/json' },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }), signal: AbortSignal.timeout(5000) });
    assert.equal(response.status, 200); return response.json();
  };
  return { stop, call };
}

test('the actual sidecar entrypoint keeps pending settings inactive, loads them at restart and resets at the next restart',
  { timeout: 20000 }, async t => {
    // Empty local data means no OAuth session exists and no browser or external network is used.
    const dataDir = await mkdtemp(join(tmpdir(), 'prism-startup-config-test-'));
    t.after(() => rm(dataDir, { recursive: true, force: true }));
    const initial = await launch(t, dataDir);
    assert.deepEqual(await initial.call(), { effective: defaults, desired: defaults,
      restart_required: false, source: 'environment', apply_mode: 'restart' });
    assert.deepEqual((await initial.call('PUT', changed)).effective, defaults);
    const pendingResources = await initial.call('GET', undefined, '/internal/resources');
    assert.equal(pendingResources.multiplex_enabled, false);
    assert.equal(pendingResources.memory.thresholdBytes, 0);
    await initial.stop();
    const saved = await launch(t, dataDir);
    assert.deepEqual(await saved.call(), { effective: changed, desired: changed,
      restart_required: false, source: 'saved', apply_mode: 'restart' });
    const resources = await saved.call('GET', undefined, '/internal/resources');
    assert.equal(resources.multiplex_enabled, true);
    assert.ok(resources.memory.thresholdBytes > 0 && resources.memory.thresholdBytes <= 2048 * 1024 * 1024);
    assert.equal(resources.memory.reserveBytes, 32 * 1024 * 1024);
    assert.deepEqual(await saved.call('DELETE'), { effective: changed, desired: defaults,
      restart_required: true, source: 'environment', apply_mode: 'restart' });
    assert.equal((await saved.call('GET', undefined, '/internal/resources')).multiplex_enabled, true);
    await saved.stop();
    const reset = await launch(t, dataDir);
    assert.deepEqual(await reset.call(), { effective: defaults, desired: defaults,
      restart_required: false, source: 'environment', apply_mode: 'restart' });
    await reset.stop();
  });

test('the actual sidecar entrypoint fails closed and sanitizes corrupt saved startup configuration',
  { timeout: 10000 }, async t => {
    const dataDir = await mkdtemp(join(tmpdir(), 'prism-corrupt-config-test-'));
    t.after(() => rm(dataDir, { recursive: true, force: true }));
    await writeFile(join(dataDir, 'runtime-config.json'), 'secret-private-path-invalid-json');
    const env = await childEnvironment(dataDir);
    const child = spawn(process.execPath, [entrypoint], { env, stdio: ['ignore', 'pipe', 'pipe'] });
    let stdout = ''; let stderr = '';
    child.stdout.on('data', chunk => { stdout += chunk; });
    child.stderr.on('data', chunk => { stderr += chunk; });
    const [code, signal] = await once(child, 'exit');
    assert.equal(code, 1); assert.equal(signal, null);
    assert.equal(stdout, '');
    assert.deepEqual(JSON.parse(stderr), { event: 'startup_failed' });
    assert.ok(!stderr.includes('secret-private-path'));
  });
