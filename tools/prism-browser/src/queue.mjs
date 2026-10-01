import { PrismError, aborted } from './errors.mjs';

export class AccountQueue {
  constructor(limit = 8, concurrency = 1) {
    this.limit = limit;
    this.concurrency = concurrency;
    this.jobs = [];
    this.active = 0;
    this.exclusiveActive = false;
    this.closed = false;
  }

  get running() { return this.active > 0; }
  get pending() { return this.jobs.filter(job => !job.exclusive).length; }

  run(work, signal, { exclusive = false, priority = false, canStart } = {}) {
    aborted(signal);
    if (this.closed) return Promise.reject(new PrismError('service_stopping', 503));
    const pending = exclusive ? this.jobs.filter(job => job.exclusive).length : this.pending;
    if (!priority && pending >= this.limit) return Promise.reject(new PrismError('account_queue_full', 429));
    return new Promise((resolve, reject) => {
      const job = { work, signal, resolve, reject, exclusive, canStart };
      job.onAbort = () => {
        const index = this.jobs.indexOf(job);
        if (index >= 0) {
          this.jobs.splice(index, 1);
          reject(signal.reason instanceof PrismError ? signal.reason : new PrismError('request_cancelled', 499));
          this.pump();
        }
      };
      signal?.addEventListener('abort', job.onAbort, { once: true });
      if (priority) this.jobs.unshift(job);
      else this.jobs.push(job);
      this.pump();
    });
  }

  exclusive(work, signal, { priority = false } = {}) {
    return this.run(work, signal, { exclusive: true, priority });
  }

  pump() {
    if (this.closed || this.exclusiveActive) return;
    while (this.active < this.concurrency && this.jobs.length) {
      const barrier = this.jobs.findIndex(job => job.exclusive);
      if (barrier === 0) {
        if (this.active) return;
        this.start(this.jobs.shift());
        return;
      }
      let selected = -1;
      for (let index = 0; index < (barrier < 0 ? this.jobs.length : barrier); index += 1) {
        const job = this.jobs[index];
        try {
          aborted(job.signal);
          if (!job.canStart || job.canStart()) { selected = index; break; }
        } catch (error) {
          this.jobs.splice(index, 1);
          job.signal?.removeEventListener('abort', job.onAbort);
          job.reject(error);
          this.pump();
          return;
        }
      }
      if (selected < 0) return;
      this.start(this.jobs.splice(selected, 1)[0]);
    }
  }

  start(job) {
    this.active += 1;
    this.exclusiveActive = job.exclusive;
    job.signal?.removeEventListener('abort', job.onAbort);
    let result;
    try {
      aborted(job.signal);
      // Acquire resources synchronously before considering the next waiting job.
      result = job.work();
    } catch (error) {
      result = Promise.reject(error);
    }
    Promise.resolve(result).then(job.resolve, job.reject).finally(() => {
      this.active -= 1;
      this.exclusiveActive = false;
      this.pump();
    });
  }

  cancelPending(error = new PrismError('session_revoked', 409)) {
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
