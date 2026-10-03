import { fork } from 'node:child_process';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { RESTART_EXIT_CODE } from './restart.mjs';

// Only this repository-owned entrypoint is launched. No CLI arguments, HTTP
// fields, shell strings or client-supplied executable paths are forwarded.
function launchServer() {
  return fork(fileURLToPath(new URL('./server.mjs', import.meta.url)), [], {
    stdio: ['inherit', 'inherit', 'inherit', 'ipc'],
    env: { ...process.env, PRISM_SUPERVISED: '1' },
  });
}

export function supervise({ launch = launchServer, runtime = process, stopTimeoutMs = 12000 } = {}) {
  let child, stopping = false, accepted = false, finished = false, deadline;
  const finish = code => {
    if (finished) return;
    finished = true;
    clearTimeout(deadline);
    runtime.off('SIGTERM', terminate); runtime.off('SIGINT', interrupt);
    runtime.exitCode = code;
  };
  const stop = signal => {
    if (stopping || finished) return;
    stopping = true;
    child?.kill(signal);
    deadline = setTimeout(() => { child?.kill('SIGKILL'); }, stopTimeoutMs);
  };
  const terminate = () => stop('SIGTERM');
  const interrupt = () => stop('SIGINT');
  const start = () => {
    accepted = false;
    try { child = launch(); }
    catch { finish(1); return; }
    const current = child;
    current.on('message', value => {
      if (stopping || finished || current !== child || !current.connected ||
        typeof value?.nonce !== 'string' || value.nonce.length !== 36 ||
        !['prism_supervisor_hello', 'prism_supervisor_restart'].includes(value.type)) return;
      if (value.type === 'prism_supervisor_restart') accepted = true;
      try { current.send({ type: value.type + '_accepted', nonce: value.nonce }, () => {}); }
      catch { /* A disappearing child is handled by its close event. */ }
    });
    current.once('error', () => { current.kill(); finish(1); });
    current.once('close', (code, signal) => {
      if (finished) return;
      if (!stopping && accepted && code === RESTART_EXIT_CODE && !signal) start();
      else finish(stopping ? 0 : Number.isInteger(code) && code !== RESTART_EXIT_CODE ? code : 1);
    });
  };
  runtime.on('SIGTERM', terminate); runtime.on('SIGINT', interrupt);
  start();
  return { stop };
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) supervise();
