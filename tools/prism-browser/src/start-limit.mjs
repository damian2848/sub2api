import { PrismError, aborted } from './errors.mjs';

// Prism refused a start on this source: the request goes back as 429 with the time left, so the
// gateway cools this account down and sends the request to another one right away.
export function startLimitedError(remainingMs) {
  const error = new PrismError('prism_start_limited', 429);
  error.retryAfterSeconds = Math.max(1, Math.ceil(remainingMs / 1000));
  return error;
}

export class NativeStartLimiter {
  constructor({ limit = 0, windowMs = 65000, now = () => performance.now(),
    setTimer = setTimeout, clearTimer = clearTimeout } = {}) {
    if (!Number.isInteger(limit) || limit < 0 || limit > 120) throw new RangeError('invalid_native_start_limit');
    if (!Number.isInteger(windowMs) || windowMs <= 0 || windowMs > 3600000) {
      throw new RangeError('invalid_native_start_window');
    }
    if (![now, setTimer, clearTimer].every(value => typeof value === 'function')) {
      throw new TypeError('invalid_native_start_clock');
    }
    this.limit = limit;
    this.windowMs = windowMs;
    this.now = now;
    this.setTimer = setTimer;
    this.clearTimer = clearTimer;
    this.starts = [];
    this.jobs = [];
    this.timer = null;
    this.closed = false;
    this.blockedUntil = 0;
  }

  // After Prism refused a start, no start is attempted on this source for `ms`: queued and new
  // requests are refused at once instead of waiting, so they can be served by another account.
  cooldown(ms) {
    if (!(ms > 0)) return;
    this.blockedUntil = Math.max(this.blockedUntil, this.now() + ms);
    this.cancelPending(startLimitedError(this.blockedUntil - this.now()));
  }

  cooling() { return Math.max(0, this.blockedUntil - this.now()); }

  acquire(signal) {
    if (this.closed) return Promise.reject(new PrismError('service_stopping', 503));
    try { aborted(signal); } catch (error) { return Promise.reject(error); }
    const cooling = this.cooling();
    if (cooling > 0) return Promise.reject(startLimitedError(cooling));
    if (!this.limit) return Promise.resolve(0);
    return new Promise((resolve, reject) => {
      const job = { signal, resolve, reject, queuedAt: this.now() };
      job.onAbort = () => {
        const index = this.jobs.indexOf(job);
        if (index < 0) return;
        this.jobs.splice(index, 1);
        signal.removeEventListener('abort', job.onAbort);
        reject(signal.reason instanceof PrismError ? signal.reason : new PrismError('request_cancelled', 499));
        this.drain();
      };
      signal?.addEventListener('abort', job.onAbort, { once: true });
      this.jobs.push(job);
      if (signal?.aborted) job.onAbort();
      else this.drain();
    });
  }

  stopTimer() {
    if (this.timer !== null) this.clearTimer(this.timer);
    this.timer = null;
  }

  drain() {
    this.stopTimer();
    if (this.closed) return;
    const current = this.now();
    while (this.starts.length && this.starts[0] + this.windowMs <= current) this.starts.shift();
    while (this.jobs.length && this.starts.length < this.limit) {
      const job = this.jobs.shift();
      job.signal?.removeEventListener('abort', job.onAbort);
      try { aborted(job.signal); } catch (error) { job.reject(error); continue; }
      const grantedAt = this.now();
      // Granted starts remain charged even when their request later fails or is cancelled.
      this.starts.push(grantedAt);
      job.resolve(Math.max(0, grantedAt - job.queuedAt));
    }
    if (this.jobs.length) {
      const delay = Math.max(0, this.starts[0] + this.windowMs - this.now());
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
