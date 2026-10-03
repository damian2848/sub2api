import test from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:net';
import { fork, spawn } from 'node:child_process';
import { once } from 'node:events';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { configurationFromEnvironment } from '../src/configuration.mjs';

const key = 'offline-restart-startup-key'.repeat(2);
async function environment(dataDir) {
  const listener = createServer(); listener.listen(0, '127.0.0.1'); await once(listener, 'listening');
  const port = listener.address().port; await new Promise(resolve => listener.close(resolve));
  return { ...Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith('PRISM_'))),
    PRISM_DATA_DIR: dataDir, PRISM_MANAGEMENT_KEY: key, PRISM_HOST: '127.0.0.1', PRISM_PORT: String(port) };
}
async function setup(t, entrypoint = 'supervisor.mjs') {
  const dataDir = await mkdtemp(join(tmpdir(), 'prism-restart-startup-'));
  const env = await environment(dataDir);
  const child = spawn(process.execPath, [fileURLToPath(new URL('../src/' + entrypoint, import.meta.url))], {
    env: { ...env, PRISM_SUPERVISED: '1' }, stdio: ['ignore', 'pipe', 'pipe'] });
  let output = '', errors = '';
  child.stdout.on('data', value => { output += value; }); child.stderr.on('data', value => { errors += value; });
  const exited = once(child, 'exit');
  const stop = async () => {
    if (child.exitCode !== null || child.signalCode !== null) return;
    child.kill('SIGTERM'); const deadline = setTimeout(() => child.kill('SIGKILL'), 15000);
    try { await exited; } finally { clearTimeout(deadline); }
  };
  t.after(async () => { await stop(); await rm(dataDir, { recursive: true, force: true }); });
  const url = `http://127.0.0.1:${env.PRISM_PORT}`;
  const call = (path, method = 'GET', body) => fetch(url + path, { method,
    headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' },
    signal: AbortSignal.timeout(3000), ...(body ? { body: JSON.stringify(body) } : {}) });
  const wait = async check => {
    const deadline = Date.now() + 10000;
    while (Date.now() < deadline) {
      if (child.exitCode !== null) throw new Error('offline fixture exited: ' + errors);
      try { const value = await check(); if (value) return value; } catch { /* expected while worker is restarting */ }
      await new Promise(resolve => setTimeout(resolve, 25));
    }
    throw new Error('offline fixture startup/restart timeout');
  };
  await wait(() => output.includes('"event":"listening"'));
  return { call, wait, stop, child, output: () => output, errors: () => errors };
}

test('actual supervisor replaces only the sidecar worker and verifies new boot plus saved configuration', { timeout: 20000 }, async t => {
  // Empty private data contains no account sessions: no browser, model request,
  // production credentials or external service is accessed in this test.
  const f = await setup(t);
  const old = await (await f.call('/internal/restart')).json();
  assert.equal(old.supported, true); assert.equal(old.state, 'ready');
  const pid = f.child.pid;
  const target = { ...configurationFromEnvironment({}), project_isolation: true, http_cache: true };
  assert.equal((await f.call('/internal/config', 'PUT', target)).status, 200);
  const accepted = await f.call('/internal/restart', 'POST', { expected_runtime_id: old.runtime_id, expected_configuration: target });
  assert.equal(accepted.status, 202);
  assert.deepEqual(await accepted.json(), { ...old, state: 'restarting' });
  const current = await f.wait(async () => {
    const response = await f.call('/internal/restart');
    const state = await response.json();
    return response.ok && state.runtime_id !== old.runtime_id && state.state === 'ready' && state;
  });
  assert.equal(current.supported, true); assert.equal(f.child.pid, pid);
  const config = await (await f.call('/internal/config')).json();
  assert.deepEqual(config.effective, target); assert.deepEqual(config.desired, target);
  assert.equal(config.restart_required, false);
  assert.equal(f.output().split('"event":"listening"').length - 1, 2);
  await f.stop(); assert.equal(f.child.exitCode, 0);
  assert.equal(f.errors(), '');
});

test('direct server cannot advertise restart even when a supervision environment marker is forged', { timeout: 15000 }, async t => {
  const f = await setup(t, 'server.mjs');
  const status = await (await f.call('/internal/restart')).json();
  assert.equal(status.supported, false);
  const desired = (await (await f.call('/internal/config')).json()).desired;
  const response = await f.call('/internal/restart', 'POST', { expected_runtime_id: status.runtime_id, expected_configuration: desired });
  assert.equal(response.status, 503);
  assert.equal((await response.json()).error.code, 'prism_restart_unsupported');
  assert.equal((await f.call('/health')).status, 200);
});

test('corrupt configuration terminates the supervisor without a boot retry loop or leaking the private content', { timeout: 10000 }, async t => {
  const dataDir = await mkdtemp(join(tmpdir(), 'prism-restart-bad-startup-'));
  t.after(() => rm(dataDir, { recursive: true, force: true }));
  await writeFile(join(dataDir, 'runtime-config.json'), 'secret-private-corrupt-content');
  const child = spawn(process.execPath, [fileURLToPath(new URL('../src/supervisor.mjs', import.meta.url))], {
    env: await environment(dataDir), stdio: ['ignore', 'pipe', 'pipe'] });
  let errors = ''; child.stderr.on('data', chunk => { errors += chunk; });
  const [code, signal] = await once(child, 'exit');
  assert.equal(code, 1); assert.equal(signal, null);
  assert.deepEqual(JSON.parse(errors), { event: 'startup_failed' });
  assert.ok(!errors.includes('secret-private-corrupt-content'));
});


test('a supervisor disconnect during startup prevents an orphan worker from listening', { timeout: 10000 }, async t => {
  const dataDir = await mkdtemp(join(tmpdir(), 'prism-restart-disconnect-'));
  t.after(() => rm(dataDir, { recursive: true, force: true }));
  const child = fork(fileURLToPath(new URL('../src/server.mjs', import.meta.url)), [], {
    env: { ...await environment(dataDir), PRISM_SUPERVISED: '1' }, stdio: ['ignore', 'pipe', 'pipe', 'ipc'] });
  let output = '', errors = '', acknowledged = false;
  child.stdout.on('data', chunk => { output += chunk; }); child.stderr.on('data', chunk => { errors += chunk; });
  child.on('message', message => {
    if (message.type !== 'prism_supervisor_hello') return;
    acknowledged = true;
    child.send({ type: message.type + '_accepted', nonce: message.nonce }, () => {
      if (child.connected) child.disconnect();
    });
  });
  const deadline = setTimeout(() => child.kill('SIGKILL'), 5000);
  const [code, signal] = await once(child, 'exit'); clearTimeout(deadline);
  assert.equal(acknowledged, true); assert.equal(code, 1); assert.equal(signal, null);
  assert.ok(!output.includes('"event":"listening"'));
  assert.equal(errors, '');
});
