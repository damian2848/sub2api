import test from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { supervise } from '../src/supervisor.mjs';
import { RESTART_EXIT_CODE } from '../src/restart.mjs';

function fixture() {
  const runtime = new EventEmitter(), children = [];
  const launch = () => {
    const child = new EventEmitter(); child.connected = true; child.signals = []; child.sent = [];
    child.kill = signal => { child.signals.push(signal); };
    child.send = (message, done) => { child.sent.push(message); done(); };
    children.push(child); return child;
  };
  const supervisor = supervise({ launch, runtime, stopTimeoutMs: 5 });
  return { runtime, children, supervisor };
}
const nonce = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';

test('supervisor acknowledges private capability and relaunches only an explicitly requested restart', () => {
  const f = fixture(), child = f.children[0];
  child.emit('message', { type: 'prism_supervisor_hello', nonce });
  assert.deepEqual(child.sent[0], { type: 'prism_supervisor_hello_accepted', nonce });
  child.emit('message', { type: 'prism_supervisor_restart', nonce });
  child.emit('close', RESTART_EXIT_CODE, null);
  assert.equal(f.children.length, 2);
  f.children[1].emit('close', 1, null);
  assert.equal(f.runtime.exitCode, 1);
  assert.equal(f.runtime.listenerCount('SIGTERM'), 0);
});

test('reserved exit code alone, arbitrary IPC and normal failures never create a restart loop', () => {
  for (const code of [RESTART_EXIT_CODE, 1, 0]) {
    const f = fixture();
    f.children[0].emit('message', { type: 'run', command: 'anything', nonce });
    f.children[0].emit('close', code, null);
    assert.equal(f.children.length, 1);
    assert.equal(f.runtime.exitCode, code === RESTART_EXIT_CODE ? 1 : code);
  }
});

test('external stop forwards the signal and suppresses an already acknowledged restart', () => {
  for (const signal of ['SIGTERM', 'SIGINT']) {
    const f = fixture();
    f.children[0].emit('message', { type: 'prism_supervisor_restart', nonce });
    f.runtime.emit(signal);
    assert.deepEqual(f.children[0].signals, [signal]);
    f.children[0].emit('close', RESTART_EXIT_CODE, null);
    assert.equal(f.children.length, 1);
    assert.equal(f.runtime.exitCode, 0);
  }
});

test('a worker that refuses shutdown is killed after the bounded supervisor deadline', async () => {
  const f = fixture();
  f.runtime.emit('SIGTERM');
  await new Promise(resolve => setTimeout(resolve, 15));
  assert.deepEqual(f.children[0].signals, ['SIGTERM', 'SIGKILL']);
  f.children[0].emit('close', null, 'SIGKILL');
  assert.equal(f.runtime.exitCode, 0);
});

test('worker spawn errors are handled without leaking diagnostics or retrying forever', () => {
  const f = fixture();
  f.children[0].emit('error', new Error('private-path'));
  assert.equal(f.runtime.exitCode, 1);
  assert.equal(f.children.length, 1);
});
