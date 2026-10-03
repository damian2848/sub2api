import { randomUUID } from 'node:crypto';
import { validateConfiguration } from './configuration.mjs';
import { PrismError } from './errors.mjs';

export const RESTART_EXIT_CODE = 75;
export const RESTART_BODY_LIMIT = 4096;
const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const unavailable = () => new PrismError('prism_restart_unsupported', 503);
const sameConfiguration = (a, b) => Object.keys(a).every(key => a[key] === b[key]);

// A process environment flag alone must never authorize exiting a standalone
// server. The fixed supervisor must acknowledge both capability and intent over
// its private IPC channel before HTTP can accept a restart.
export async function connectRestartSupervisor({ runtime = process, timeoutMs = 1500 } = {}) {
  if (runtime.env.PRISM_SUPERVISED !== '1' || !runtime.connected || typeof runtime.send !== 'function') return null;
  const exchange = type => new Promise((resolve, reject) => {
    const nonce = randomUUID();
    let timer;
    const cleanup = () => { clearTimeout(timer); runtime.off('message', message); runtime.off('disconnect', disconnected); };
    const done = (error) => { cleanup(); error ? reject(unavailable()) : resolve(); };
    const message = value => { if (value?.type === type + '_accepted' && value.nonce === nonce) done(); };
    const disconnected = () => done(unavailable());
    runtime.on('message', message); runtime.once('disconnect', disconnected);
    timer = setTimeout(() => done(unavailable()), timeoutMs);
    try { runtime.send({ type, nonce }, error => { if (error) done(error); }); }
    catch (error) { done(error); }
  });
  try { await exchange('prism_supervisor_hello'); }
  catch { return null; }
  return { prepareRestart: () => exchange('prism_supervisor_restart') };
}

export class PrismRestartController {
  #pending = false;
  #scheduled = false;
  constructor({ configuration, prepareRestart, onRestart, runtimeId = randomUUID() }) {
    this.configuration = configuration;
    this.prepareRestart = prepareRestart;
    this.onRestart = onRestart;
    this.runtimeId = runtimeId;
  }
  get pending() { return this.#pending; }
  snapshot() {
    return { supported: !!(this.configuration && this.prepareRestart && this.onRestart),
      runtime_id: this.runtimeId, state: this.#pending ? 'restarting' : 'ready' };
  }
  async accept(body) {
    if (!body || Array.isArray(body) || Object.keys(body).length !== 2 ||
      !Object.hasOwn(body, 'expected_runtime_id') || !Object.hasOwn(body, 'expected_configuration') ||
      typeof body.expected_runtime_id !== 'string' || !uuidPattern.test(body.expected_runtime_id)) {
      throw new PrismError('invalid_prism_restart', 400);
    }
    let target;
    try { target = validateConfiguration(body.expected_configuration); }
    catch { throw new PrismError('invalid_prism_restart', 400); }
    if (!this.snapshot().supported) throw unavailable();
    return this.configuration.beginRestart(async state => {
      if (body.expected_runtime_id !== this.runtimeId || !sameConfiguration(target, state.desired)) {
        throw new PrismError('prism_restart_conflict', 409);
      }
      if (!this.#pending) {
        await this.prepareRestart();
        this.#pending = true;
      }
      return this.snapshot();
    });
  }
  // Acknowledgement must be flushed before closing the HTTP server. Repeated
  // identical requests never schedule a second shutdown.
  afterResponse(response) {
    if (this.#scheduled) return;
    this.#scheduled = true;
    const schedule = () => {
      response.off('finish', schedule); response.off('close', schedule);
      setImmediate(() => this.onRestart());
    };
    response.once('finish', schedule); response.once('close', schedule);
    // Once intent was acknowledged we must not freeze forever if the caller
    // disconnects before receiving 202. The UI resolves this ambiguity by GET.
    if (response.destroyed) schedule();
  }
}
