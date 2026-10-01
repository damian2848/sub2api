import { PrismError, aborted } from './errors.mjs';

export class AccountQueue {
  constructor(limit = 8) {
    this.limit = limit;
    this.jobs = [];
    this.running = false;
    this.closed = false;
  }

  run(work, signal) {
    aborted(signal);
    if (this.closed) return Promise.reject(new PrismError('service_stopping', 503));
    if (this.jobs.length >= this.limit) return Promise.reject(new PrismError('account_queue_full', 429));
    return new Promise((resolve, reject) => {
      const job = { work, signal, resolve, reject };
      job.onAbort = () => {
        const index = this.jobs.indexOf(job);
        if (index >= 0) {
          this.jobs.splice(index, 1);
          reject(signal.reason instanceof PrismError ? signal.reason : new PrismError('request_cancelled', 499));
        }
      };
      signal?.addEventListener('abort', job.onAbort, { once: true });
      this.jobs.push(job);
      this.pump();
    });
  }

  async pump() {
    if (this.running || this.closed) return;
    const job = this.jobs.shift();
    if (!job) return;
    this.running = true;
    job.signal?.removeEventListener('abort', job.onAbort);
    try {
      aborted(job.signal);
      job.resolve(await job.work());
    } catch (error) {
      job.reject(error);
    } finally {
      this.running = false;
      this.pump();
    }
  }

  close() {
    this.closed = true;
    for (const job of this.jobs.splice(0)) {
      job.signal?.removeEventListener('abort', job.onAbort);
      job.reject(new PrismError('service_stopping', 503));
    }
  }
}
