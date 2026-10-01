import { join } from 'node:path';
import { AccountManager } from './accounts.mjs';
import { AccountQueue } from './queue.mjs';
import { BrowserSession, launchBrowser } from './browser.mjs';
import { PrismError, aborted } from './errors.mjs';
import { NativeStartLimiter } from './start-limit.mjs';

export class AccountPoolManager {
  constructor({ dataDir, concurrency = 2, maxWorkers = 32, queueLimit = 8, maxAccounts = 16,
    startLimit = 0, startWindowMs = 65000,
    startLimiterFactory = () => new NativeStartLimiter({ limit: startLimit, windowMs: startWindowMs }),
    browserFactory = launchBrowser,
    sessionFactory = (browser, heartbeat, source, slot) => new BrowserSession(browser, heartbeat, source, slot) }) {
    if (!Number.isInteger(concurrency) || concurrency < 1 || concurrency > 4 ||
      !Number.isInteger(maxWorkers) || maxWorkers < 1 || maxWorkers > 1024) throw new Error('invalid_worker_capacity');
    if (!Number.isInteger(startLimit) || startLimit < 0 || startLimit > 120 ||
      !Number.isInteger(startWindowMs) || startWindowMs < 1 || startWindowMs > 3600000) {
      throw new Error('invalid_native_start_limit');
    }
    this.concurrency = concurrency;
    this.maxWorkers = maxWorkers;
    this.queueLimit = queueLimit;
    this.browserFactory = browserFactory;
    this.sessionFactory = sessionFactory;
    this.startLimiterFactory = startLimiterFactory;
    this.startLimiters = new Map();
    this.browserPromise = null;
    this.drivers = new Set();
    this.accounts = new Map();
    this.stopping = false;
    this.managers = Array.from({ length: concurrency }, (_, slot) => new AccountManager({
      dataDir: slot === 0 ? dataDir : join(dataDir, 'workers', String(slot)), queueLimit, maxAccounts,
      ownsBrowser: false,
      browserFactory: () => this.getBrowser(),
      sessionFactory: (browser, heartbeat, source) => this.createDriver(browser, heartbeat, source, slot),
    }));
  }

  get primary() { return this.managers[0]; }
  async init() { await Promise.all(this.managers.map(manager => manager.init())); }

  getBrowser() {
    if (this.stopping) return Promise.reject(new PrismError('service_stopping', 503));
    if (!this.browserPromise) this.browserPromise = this.browserFactory().then(browser => {
      if (this.stopping) return browser.close().then(() => { throw new PrismError('service_stopping', 503); });
      browser.on?.('disconnected', () => { this.browserPromise = null; this.drivers.clear(); });
      return browser;
    }).catch(error => { this.browserPromise = null; throw error; });
    return this.browserPromise;
  }

  createDriver(browser, heartbeat, source, slot) {
    if (this.stopping) throw new PrismError('service_stopping', 503);
    if (this.drivers.size >= this.maxWorkers) throw new PrismError('browser_capacity_full', 429);
    const driver = this.sessionFactory(browser, heartbeat, source, slot);
    let limiter = this.startLimiters.get(source);
    if (!limiter) {
      limiter = this.startLimiterFactory();
      this.startLimiters.set(source, limiter);
    }
    const generate = driver.generate.bind(driver);
    driver.generate = async (request, signal, progress) => {
      const waited = await limiter.acquire(signal);
      aborted(signal);
      if (this.stopping) throw new PrismError('service_stopping', 503);
      const account = this.managers[slot].accounts.get(source);
      if (account?.driver !== driver) throw new PrismError('session_revoked', 409);
      if (!account.expiresAt || account.expiresAt <= Date.now() / 1000) {
        throw new PrismError('account_not_ready', 503);
      }
      if (driver.isAlive?.() === false) throw new PrismError('browser_session_closed', 503);
      if (waited > 0 && process.env.PRISM_AUDIT_REQUESTS === 'true') {
        console.log(JSON.stringify({ event: 'native_start_wait', source, worker: slot, wait_ms: Math.round(waited) }));
      }
      return generate(request, signal, progress);
    };
    this.drivers.add(driver);
    const close = driver.close.bind(driver);
    driver.close = async () => {
      try { await close(); }
      finally { if (driver.isAlive?.() !== true) this.drivers.delete(driver); }
    };
    return driver;
  }

  runtime(source) {
    this.primary.get(source);
    let account = this.accounts.get(source);
    if (!account) {
      account = { queue: new AccountQueue(this.queueLimit, this.concurrency), version: 0, session: null,
        workers: this.managers.map((manager, slot) => ({ manager, slot, busy: false })), next: 0 };
      this.accounts.set(source, account);
    }
    return account;
  }

  workerStatus(source, worker) {
    try { return worker.manager.status(source); }
    catch (error) {
      if (error instanceof PrismError && error.code === 'account_not_found') return { ready: false, models: [] };
      throw error;
    }
  }

  usable(source, worker) {
    const status = this.workerStatus(source, worker);
    if (status.ready) return true;
    const account = worker.manager.accounts.get(source);
    return Boolean(worker.busy && status.phase === 'initializing' && account?.driver?.isAlive?.() !== false &&
      account?.expiresAt > Date.now() / 1000);
  }

  status(source) {
    const base = this.primary.status(source);
    const account = this.runtime(source);
    const ready = account.workers.filter(worker => this.usable(source, worker));
    const models = [...new Set(ready.flatMap(worker => this.workerStatus(source, worker).models))];
    const heartbeat = Math.max(0, ...ready.map(worker => this.workerStatus(source, worker).last_heartbeat_at || 0));
    return { ...base, ready: ready.length > 0, models, ...(ready.length ? { phase: 'ready', error_code: undefined } : {}),
      last_heartbeat_at: heartbeat || base.last_heartbeat_at,
      concurrency: ready.length, pool_size: this.concurrency, ready_workers: ready.length,
      busy_workers: ready.filter(worker => worker.busy).length, queued: account.queue.pending };
  }

  authenticateKey(source, key) { return this.primary.authenticateKey(source, key); }

  async provision(source, body, signal) {
    this.primary.validateSession(body);
    this.primary.get(source, true);
    const account = this.runtime(source);
    const version = account.version;
    this.assertCurrent(account, version, signal);
    const sameSession = account.session && ['access_token', 'api_key', 'expected_email', 'expected_user_id']
      .every(key => account.session[key] === body[key]);
    if (sameSession && !account.queue.exclusiveActive && !account.queue.jobs.some(job => job.exclusive) &&
      account.workers.every(worker => {
        const runtime = worker.manager.accounts.get(source);
        return runtime?.driver && runtime.driver.isAlive?.() !== false && runtime.accessHash;
      })) {
      await Promise.all(account.workers.map(worker => worker.manager.provision(source, body, signal)));
      this.assertCurrent(account, version, signal);
      account.session = { ...body };
      return this.status(source);
    }
    return account.queue.exclusive(async () => {
      this.assertCurrent(account, version, signal);
      const warm = account.workers.map(worker => {
        const metadata = worker.manager.accounts.get(source)?.metadata;
        return Boolean(metadata?.project_id && metadata.verified_project === metadata.project_id);
      });
      await this.primary.provision(source, body, signal);
      this.assertCurrent(account, version, signal);
      account.session = { ...body };
      if (warm[0]) await this.primary.bootstrap(source, signal);
      // Primary authentication establishes the stable identity before more contexts open.
      await Promise.allSettled(account.workers.slice(1).map(async worker => {
        await this.provisionWorker(source, worker, body, signal);
        if (warm[worker.slot]) await worker.manager.bootstrap(source, signal);
      }));
      this.assertCurrent(account, version, signal);
      return this.status(source);
    }, signal);
  }

  async provisionWorker(source, worker, body, signal) {
    await worker.manager.provision(source, body, signal);
    const identity = this.primary.get(source).metadata.identity_hash;
    if (worker.manager.get(source).metadata.identity_hash !== identity) {
      await worker.manager.revoke(source);
      throw new PrismError('source_identity_changed', 409);
    }
  }

  async bootstrap(source, signal, options = {}) {
    const account = this.runtime(source);
    const version = account.version;
    this.assertCurrent(account, version, signal);
    if (!options.retry_probe && !account.queue.exclusiveActive && !account.queue.jobs.some(job => job.exclusive) &&
      account.workers.every(worker => this.workerStatus(source, worker).ready)) return this.status(source);
    return account.queue.exclusive(async () => {
      this.assertCurrent(account, version, signal);
      const outcomes = await Promise.allSettled(account.workers.map(async worker => {
        const runtime = worker.manager.accounts.get(source);
        if ((!runtime?.driver || runtime.driver.isAlive?.() === false) && account.session) {
          await this.provisionWorker(source, worker, account.session, signal);
        }
        return worker.manager.bootstrap(source, signal, options);
      }));
      this.assertCurrent(account, version, signal);
      const status = this.status(source);
      if (!status.ready) throw outcomes.find(outcome => outcome.status === 'rejected')?.reason ||
        new PrismError('account_not_ready', 503);
      return status;
    }, signal);
  }

  assertCurrent(account, version, signal) {
    aborted(signal);
    if (this.stopping || account.version !== version) throw new PrismError('session_revoked', 409);
  }

  available(source, account, model) {
    const matching = account.workers.filter(worker => this.usable(source, worker) &&
      this.workerStatus(source, worker).models.includes(model));
    if (!matching.length) throw new PrismError(this.status(source).ready ? 'model_not_available' : 'account_not_ready',
      this.status(source).ready ? 400 : 503);
    for (let offset = 0; offset < account.workers.length; offset += 1) {
      const worker = account.workers[(account.next + offset) % account.workers.length];
      if (!worker.busy && matching.includes(worker)) return worker;
    }
    return null;
  }

  async generate(source, request, signal, onText) {
    const account = this.runtime(source);
    const version = account.version;
    const queuedAt = performance.now();
    return account.queue.run(async () => {
      this.assertCurrent(account, version, signal);
      const worker = this.available(source, account, request.model);
      if (!worker) throw new PrismError('account_busy', 409);
      worker.busy = true;
      account.next = (worker.slot + 1) % account.workers.length;
      const startedAt = performance.now();
      try {
        const text = await worker.manager.generate(source, request, signal, onText ? value => {
          this.assertCurrent(account, version, signal);
          onText(value);
        } : undefined);
        this.assertCurrent(account, version, signal);
        return text;
      } finally {
        worker.busy = false;
        if (process.env.PRISM_AUDIT_REQUESTS === 'true') console.log(JSON.stringify({ event: 'request_timing',
          source, worker: worker.slot, model: request.model, queue_wait_ms: Math.round(startedAt - queuedAt),
          generation_ms: Math.round(performance.now() - startedAt) }));
      }
    }, signal, { canStart: () => { this.assertCurrent(account, version, signal); return Boolean(this.available(source, account, request.model)); } });
  }

  async revoke(source, signal) {
    const account = this.runtime(source);
    account.version += 1;
    account.session = null;
    account.queue.cancelPending(new PrismError('session_revoked', 409));
    this.startLimiters.get(source)?.cancelPending(new PrismError('session_revoked', 409));
    const revocations = Promise.all(account.workers.filter(worker => worker.manager.accounts.has(source))
      .map(worker => worker.manager.revoke(source)));
    revocations.catch(() => {});
    return account.queue.exclusive(async () => {
      await revocations;
      aborted(signal);
      return this.status(source);
    }, undefined, { priority: true });
  }

  async close() {
    this.stopping = true;
    for (const limiter of this.startLimiters.values()) limiter.close();
    for (const account of this.accounts.values()) { account.session = null; account.queue.close(); }
    await Promise.allSettled(this.managers.map(manager => manager.close()));
    const browser = await this.browserPromise?.catch(() => null);
    await browser?.close();
    this.browserPromise = null;
    this.drivers.clear();
  }
}
