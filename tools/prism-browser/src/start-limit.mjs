import { PrismError, aborted } from './errors.mjs';

export function startLimitedError(remainingMs) {
  const error = new PrismError('prism_start_limited', 429);
  error.retryAfterSeconds = Math.max(1, Math.ceil(remainingMs / 1000));
  return error;
}

export function nativeStartSettings(env = process.env) {
  const number = (name, fallback, min, max, integer = false) => {
    const value = env[name] === undefined || env[name] === '' ? fallback : Number(env[name]);
    if (!Number.isFinite(value) || value < min || value > max || (integer && !Number.isInteger(value))) {
      throw new Error(`invalid ${name}`);
    }
    return value;
  };
  const limit = number('PRISM_ACCOUNT_START_LIMIT', 0, 0, 120, true);
  const windowMs = number('PRISM_START_WINDOW_SECONDS', 65, 1, 3600, true) * 1000;
  const burst = number('PRISM_START_BURST', limit, 0, 120, true);
  const refillMs = number('PRISM_START_REFILL_SECONDS', windowMs / (burst || 1) / 1000, 0.001, 3600) * 1000;
  const refillMinMs = number('PRISM_START_REFILL_MIN_SECONDS', refillMs / 1000, 0.001, 3600) * 1000;
  const refillMaxMs = number('PRISM_START_REFILL_MAX_SECONDS', 90, 0.001, 3600) * 1000;
  const maxWaitMs = number('PRISM_START_MAX_WAIT_SECONDS', Math.max(15, refillMs / 1000), 0, 3600) * 1000;
  const settings = { limit, windowMs, burst, refillMs, refillMinMs, refillMaxMs, maxWaitMs };
  new NativeStartLimiter(settings).close();
  return settings;
}

export class NativeStartLimiter {
  constructor({ limit = 0, windowMs = 65000, burst = limit, refillMs = windowMs / (burst || 1),
    refillMinMs = refillMs, refillMaxMs = 90000, maxWaitMs = Math.max(15000, refillMs), onAudit = () => {},
    now = () => performance.now(), setTimer = setTimeout, clearTimer = clearTimeout } = {}) {
    if (!Number.isInteger(limit) || limit < 0 || limit > 120) throw new RangeError('invalid_native_start_limit');
    if (!Number.isInteger(windowMs) || windowMs <= 0 || windowMs > 3600000) {
      throw new RangeError('invalid_native_start_window');
    }
    if (![now, setTimer, clearTimer].every(value => typeof value === 'function')) {
      throw new TypeError('invalid_native_start_clock');
    }
    if (!Number.isInteger(burst) || burst < 0 || burst > 120) throw new RangeError('invalid_native_start_burst');
    if (![refillMs, refillMinMs, refillMaxMs].every(value => Number.isFinite(value) && value > 0 && value <= 3600000) ||
      refillMinMs > refillMaxMs || (burst > 0 && (refillMs < refillMinMs || refillMs > refillMaxMs))) {
      throw new RangeError('invalid_native_start_refill');
    }
    if (!Number.isFinite(maxWaitMs) || maxWaitMs < 0 || maxWaitMs > 3600000) {
      throw new RangeError('invalid_native_start_max_wait');
    }
    this.burst = burst;
    this.refillMs = refillMs;
    this.refillMinMs = refillMinMs;
    this.refillMaxMs = refillMaxMs;
    this.maxWaitMs = maxWaitMs;
    this.onAudit = onAudit;
    this.now = now;
    this.setTimer = setTimer;
    this.clearTimer = clearTimer;
    this.tokens = burst;
    this.updatedAt = now();
    this.successes = 0;
    this.jobs = [];
    this.timer = null;
    this.closed = false;
    this.blockedUntil = 0;
  }

  cooldown(ms) {
    if (!(ms > 0)) return;
    this.blockedUntil = Math.max(this.blockedUntil, this.now() + ms);
    for (const job of [...this.jobs]) {
      if (job.failover !== 'none') this.rejectJob(job, startLimitedError(this.waitMs()));
    }
    this.drain();
  }

  cooling() { return Math.max(0, this.blockedUntil - this.now()); }

  refill() {
    const current = this.now();
    this.tokens = Math.min(this.burst, this.tokens + Math.max(0, current - this.updatedAt) / this.refillMs);
    this.updatedAt = current;
  }

  waitMs(ahead = 0) {
    this.refill();
    const cooling = this.cooling();
    if (!this.burst) return cooling;
    const tokens = Math.min(this.burst, this.tokens + cooling / this.refillMs);
    return cooling + Math.max(0, ahead + 1 - tokens) * this.refillMs;
  }

  status() {
    this.refill();
    return { tokens: this.tokens, refill_seconds: this.refillMs / 1000, cooling_ms: this.cooling() };
  }

  adjust(refillMs) {
    this.refill();
    if (refillMs === this.refillMs) return;
    this.refillMs = refillMs;
    this.onAudit('start_bucket_adjusted', { refill_seconds: refillMs / 1000 });
    this.drain();
  }

  accepted() {
    if (++this.successes < 4) return;
    this.successes = 0;
    this.adjust(Math.max(this.refillMinMs, this.refillMs * 0.9));
  }

  rejected(cooldownMs) {
    this.refill();
    this.tokens = 0;
    this.successes = 0;
    this.blockedUntil = Math.max(this.blockedUntil, this.now() + cooldownMs);
    this.adjust(Math.min(this.refillMaxMs, this.refillMs * 1.5));
    this.onAudit('start_bucket_rejected', this.status());
    this.cooldown(cooldownMs);
    this.drain();
  }

  acquire(signal, { failover = 'available' } = {}) {
    if (this.closed) return Promise.reject(new PrismError('service_stopping', 503));
    try { aborted(signal); } catch (error) { return Promise.reject(error); }
    const wait = this.waitMs(this.jobs.length);
    if (wait > this.maxWaitMs && failover !== 'none') return Promise.reject(startLimitedError(wait));
    return new Promise((resolve, reject) => {
      const job = { signal, resolve, reject, failover, queuedAt: this.now() };
      job.onAbort = () => {
        const index = this.jobs.indexOf(job);
        if (index < 0) return;
        this.rejectJob(job, signal.reason instanceof PrismError ? signal.reason : new PrismError('request_cancelled', 499));
        this.drain();
      };
      signal?.addEventListener('abort', job.onAbort, { once: true });
      this.jobs.push(job);
      if (signal?.aborted) job.onAbort();
      else this.drain();
    });
  }

  rejectJob(job, error) {
    this.jobs.splice(this.jobs.indexOf(job), 1);
    job.signal?.removeEventListener('abort', job.onAbort);
    job.reject(error);
  }

  stopTimer() {
    if (this.timer !== null) this.clearTimer(this.timer);
    this.timer = null;
  }

  drain() {
    this.stopTimer();
    if (this.closed) return;
    this.refill();
    while (this.jobs.length && this.waitMs() < 0.000001) {
      const job = this.jobs.shift();
      job.signal?.removeEventListener('abort', job.onAbort);
      try { aborted(job.signal); } catch (error) { job.reject(error); continue; }
      const grantedAt = this.now();
      if (this.burst) this.tokens = Math.max(0, this.tokens - 1);
      job.resolve(Math.max(0, grantedAt - job.queuedAt));
    }
    if (this.jobs.length) {
      const delay = Math.max(1, Math.ceil(this.waitMs()));
      this.timer = this.setTimer(() => {
        this.timer = null;
        this.drain();
      }, delay);
    }
  }

  cancelPending(error = new PrismError('session_revoked', 409)) {
    this.stopTimer();
    for (const job of this.jobs.splice(0)) {
      job.signal?.removeEventListener('abort', job.onAbort);
      job.reject(error);
    }
  }

  close() {
    this.closed = true;
    this.cancelPending(new PrismError('service_stopping', 503));
  }
}
