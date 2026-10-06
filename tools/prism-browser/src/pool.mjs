import { join } from 'node:path';
import { AccountManager } from './accounts.mjs';
import { AccountQueue } from './queue.mjs';
import { BrowserSession, launchBrowser } from './browser.mjs';
import { PrismError, aborted, pause, projectRuntimeRateLimitedError } from './errors.mjs';
import { NativeStartLimiter } from './start-limit.mjs';
import { ProjectRegistry, freshProjectScope } from './projects.mjs';
import { AccountPageMultiplexer, multiplexEnabled } from './page-multiplexer.mjs';
import { TurnJournal } from './turn-journal.mjs';

export class AccountPoolManager {
  constructor({ dataDir, concurrency = 2, maxWorkers = 32, queueLimit = 8, maxAccounts = 16,
    startLimit = 0, startWindowMs = 65000, transientRetries = 1, transientRetryDelayMs = 4000, transientRetryWaitMs = 15000,
    startCooldownMs = 60000, runtimeCooldownMs = 60000, startOptions = {}, projectIsolation = false, projectRegistry, admissionGuard, metrics,
    browserOptions = {}, multiplex = multiplexEnabled(), enabled = true, disableDrainMs = 3000,
    startLimiterFactory = source => new NativeStartLimiter({ limit: startLimit, windowMs: startWindowMs, ...startOptions,
      onAudit: (event, fields) => {
        if (process.env.PRISM_AUDIT_REQUESTS === 'true') console.log(JSON.stringify({ event, source, ...fields }));
      } }),
    browserFactory = launchBrowser,
    sessionFactory = (browser, heartbeat, source, slot, options) => new BrowserSession(browser, heartbeat, source, slot, options) }) {
    if (!Number.isInteger(concurrency) || concurrency < 1 || concurrency > 4 ||
      !Number.isInteger(maxWorkers) || maxWorkers < 1 || maxWorkers > 1024) throw new Error('invalid_worker_capacity');
    if (!Number.isInteger(startLimit) || startLimit < 0 || startLimit > 120 ||
      !Number.isInteger(startWindowMs) || startWindowMs < 1 || startWindowMs > 3600000) {
      throw new Error('invalid_native_start_limit');
    }
    if (![0, 1].includes(transientRetries) || !Number.isInteger(transientRetryDelayMs) || transientRetryDelayMs < 0 ||
      transientRetryDelayMs > 60000 || !Number.isInteger(transientRetryWaitMs) || transientRetryWaitMs < 0 ||
      transientRetryWaitMs > 120000) throw new Error('invalid_transient_retry');
    if (!Number.isInteger(startCooldownMs) || startCooldownMs < 0 || startCooldownMs > 600000) {
      throw new Error('invalid_start_cooldown');
    }
    if (!Number.isInteger(runtimeCooldownMs) || runtimeCooldownMs < 0 || runtimeCooldownMs > 600000) {
      throw new Error('invalid_runtime_cooldown');
    }
    this.startCooldownMs = startCooldownMs;
    this.runtimeCooldownMs = runtimeCooldownMs;
    this.projectIsolation = projectIsolation;
    this.projects = projectRegistry || new ProjectRegistry({ dataDir });
    this.turnJournal = new TurnJournal({ dataDir });
    this.admissionGuard = admissionGuard;
    this.metrics = metrics;
    this.browserOptions = browserOptions;
    this.multiplex = multiplex;
    this.enabled = enabled !== false;
    this.disableDrainMs = Number.isFinite(disableDrainMs) && disableDrainMs >= 0 ? disableDrainMs : 3000;
    this.releasePromise = null;
    this.lifecyclePromise = Promise.resolve();
    this.lifecycleGeneration = 0;
    this.multiplexers = new Map();
    this.transientRetries = transientRetries;
    this.transientRetryDelayMs = transientRetryDelayMs;
    this.transientRetryWaitMs = transientRetryWaitMs;
    this.concurrency = concurrency;
    this.maxWorkers = maxWorkers;
    this.queueLimit = queueLimit;
    this.browserFactory = browserFactory;
    this.sessionFactory = sessionFactory;
    this.startLimiterFactory = startLimiterFactory;
    this.startLimiters = new Map();
    this.runtimeCooldowns = new Map();
    this.browserPromise = null;
    this.drivers = new Set();
    this.accounts = new Map();
    this.stopping = false;
    this.managers = Array.from({ length: concurrency }, (_, slot) => new AccountManager({
      dataDir: slot === 0 ? dataDir : join(dataDir, 'workers', String(slot)), queueLimit, maxAccounts,
      ownsBrowser: false, projectIsolation, projectRegistry: this.projects, admissionGuard, metrics,
      browserFactory: () => this.getBrowser(),
      sessionFactory: (browser, heartbeat, source) => this.createDriver(browser, heartbeat, source, slot),
    }));
  }

  get primary() { return this.managers[0]; }
  isEnabled() { return this.enabled; }
  async init() {
    await this.turnJournal.init();
    await Promise.all(this.managers.map(manager => manager.init()));
  }

  getBrowser() {
    if (this.stopping) return Promise.reject(new PrismError('service_stopping', 503));
    if (!this.enabled) return Promise.reject(new PrismError('prism_disabled', 503));
    if (!this.browserPromise) this.browserPromise = this.browserFactory().then(browser => {
      if (this.stopping) return browser.close().then(() => { throw new PrismError('service_stopping', 503); });
      browser.on?.('disconnected', () => {
        this.browserPromise = null; this.drivers.clear();
        for (const multiplexer of this.multiplexers.values()) multiplexer.close().catch(() => {});
        this.multiplexers.clear();
      });
      return browser;
    }).catch(error => { this.browserPromise = null; throw error; });
    return this.browserPromise;
  }

  createDriver(browser, heartbeat, source, slot) {
    if (this.stopping) throw new PrismError('service_stopping', 503);
    if (!this.enabled) throw new PrismError('prism_disabled', 503);
    const previousMultiplexer = this.multiplexers.get(source);
    if (previousMultiplexer?.closed) this.multiplexers.delete(source);
    const residentReservation = this.multiplex && !this.multiplexers.has(source) ? 1 : 0;
    if (this.drivers.size + this.multiplexers.size + residentReservation >= this.maxWorkers) throw new PrismError('browser_capacity_full', 429);
    let multiplexer;
    if (this.multiplex) {
      multiplexer = this.multiplexers.get(source);
      if (!multiplexer) {
        multiplexer = new AccountPageMultiplexer({ ...this.browserOptions, admissionGuard: this.admissionGuard,
          metrics: this.metrics, onAudit: (event, details) => {
            if (process.env.PRISM_AUDIT_REQUESTS === 'true') console.log(JSON.stringify({ event, source, ...details }));
          } });
        this.multiplexers.set(source, multiplexer);
      }
    }
    const driver = this.sessionFactory(browser, heartbeat, source, slot, { ...this.browserOptions,
      admissionGuard: this.admissionGuard, metrics: this.metrics, multiplex: this.multiplex, multiplexer });
    const runtimeCooldownRemaining = () => {
      const until = this.runtimeCooldowns.get(source) || 0;
      const remaining = Math.max(0, until - Date.now());
      if (!remaining) this.runtimeCooldowns.delete(source);
      return remaining;
    };
    const coolRuntime = error => {
      const now = Date.now();
      const activeUntil = this.runtimeCooldowns.get(source) || 0;
      const until = activeUntil > now ? activeUntil : now + this.runtimeCooldownMs;
      if (until > now) this.runtimeCooldowns.set(source, until);
      else this.runtimeCooldowns.delete(source);
      error.retryAfterSeconds = Math.max(1, Math.ceil((until - Date.now()) / 1000));
      if (process.env.PRISM_AUDIT_REQUESTS === 'true') console.log(JSON.stringify({ event: 'project_runtime_rate_limit_cooldown',
        source, worker: slot, cooldown_ms: until - Date.now() }));
      return error;
    };
    const initialize = driver.initialize?.bind(driver);
    if (initialize) {
      driver.initialize = async (...args) => {
        const remaining = runtimeCooldownRemaining();
        if (remaining) throw projectRuntimeRateLimitedError(Math.ceil(remaining / 1000));
        try { return await initialize(...args); }
        catch (error) {
          if (error instanceof PrismError && error.code === 'project_runtime_rate_limited') throw coolRuntime(error);
          throw error;
        }
      };
    }
    let limiter = this.startLimiters.get(source);
    if (!limiter) {
      limiter = this.startLimiterFactory(source);
      this.startLimiters.set(source, limiter);
    }
    const generate = driver.generate.bind(driver);
    driver.generate = async (request, signal, progress) => {
      const failover = request.failover === 'none' ? 'none' : 'available';
      for (let attempt = 0; attempt < 2; attempt += 1) {
        const runtimeRemaining = runtimeCooldownRemaining();
        if (runtimeRemaining) throw projectRuntimeRateLimitedError(Math.ceil(runtimeRemaining / 1000));
        const waited = await limiter.acquire(signal, { failover });
        aborted(signal);
        const runtimeAfterWait = runtimeCooldownRemaining();
        if (runtimeAfterWait) throw projectRuntimeRateLimitedError(Math.ceil(runtimeAfterWait / 1000));
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
        let accepted = false;
        const onAccepted = () => {
          if (accepted) return;
          accepted = true;
          limiter.accepted?.();
          request.onAccepted?.();
        };
        try {
          const result = await generate({ ...request, onAccepted }, signal, progress);
          if (!accepted) limiter.accepted?.();
          return result;
        } catch (error) {
          if (error instanceof PrismError && error.code === 'project_runtime_rate_limited') throw coolRuntime(error);
          if (error instanceof PrismError && error.code === 'prism_start_rejected') {
            limiter.rejected(this.startCooldownMs);
            error.retryAfterSeconds = Math.max(1, Math.ceil(limiter.waitMs() / 1000));
            if (process.env.PRISM_AUDIT_REQUESTS === 'true') console.log(JSON.stringify({ event: 'start_rejected_cooldown',
              source, worker: slot, cooldown_ms: this.startCooldownMs }));
            if (failover === 'none' && attempt === 0) continue;
          }
          throw error;
        }
      }
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
        workers: this.managers.map((manager, slot) => ({ manager, slot, busy: false })), next: 0, activeScopes: new Set() };
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
    const pendingTurns = this.turnJournal.initialized ? this.turnJournal.list(source) : [];
    return { ...base, ready: ready.length > 0, models, pending_turns: pendingTurns,
      ...(ready.length ? { phase: 'ready', error_code: undefined } : {}),
      last_heartbeat_at: heartbeat || base.last_heartbeat_at,
      concurrency: ready.length, pool_size: this.concurrency, ready_workers: ready.length,
      busy_workers: ready.filter(worker => worker.busy).length, queued: account.queue.pending,
      start_bucket: this.startLimiters.get(source)?.status?.() || null };
  }

  pending(source) { return this.turnJournal.list(source); }

  async resolvePending(source, id) { return this.turnJournal.resolve(source, id); }

  async resources() {
    return { memory: await this.admissionGuard?.snapshot() || null, timings: this.metrics?.snapshot() || {},
      contexts: this.drivers.size + [...this.multiplexers.values()].filter(value => value.isAlive()).length,
      reserved_contexts: this.drivers.size + this.multiplexers.size, multiplex_enabled: this.multiplex,
      multiplexers: [...this.multiplexers.values()].map(value => value.status()),
      accounts: [...this.accounts].map(([source, account]) => ({ source,
        busy_workers: account.workers.filter(value => value.busy).length, queued: account.queue.pending })) };
  }

  authenticateKey(source, key) { return this.primary.authenticateKey(source, key); }

  async provision(source, body, signal) {
    if (!this.enabled) throw new PrismError('prism_disabled', 503);
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
    if (!this.enabled) throw new PrismError('prism_disabled', 503);
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

  // With project isolation, a request whose scope already has a project goes to an idle worker that is
  // on that project, so consecutive turns of one conversation keep the prepared chat instead of reloading the
  // project on another worker. Everything else (no scope, no holder, holder busy) is plain round-robin.
  available(source, account, model, scope = null) {
    const matching = account.workers.filter(worker => this.usable(source, worker) &&
      this.workerStatus(source, worker).models.includes(model));
    if (!matching.length) throw new PrismError(this.status(source).ready ? 'model_not_available' : 'account_not_ready',
      this.status(source).ready ? 400 : 503);
    if (this.projectIsolation && scope?.reusable) {
      const known = this.projects.peek(source, scope.id);
      const holder = known && matching.find(worker => {
        const driver = worker.manager.accounts.get(source)?.driver;
        return !worker.busy && driver?.projectId === known.project_id && driver.isAlive?.() !== false;
      });
      if (holder) return holder;
    }
    for (let offset = 0; offset < account.workers.length; offset += 1) {
      const worker = account.workers[(account.next + offset) % account.workers.length];
      if (!worker.busy && matching.includes(worker)) return worker;
    }
    return null;
  }

  // An idle, usable worker for the one transient retry: another worker is preferred because a failed
  // turn usually closes its own browser context, but the failed one is fine if it is still usable.
  // Waits briefly for a busy worker to free up, then gives up (the caller surfaces the original error).
  async retryWorker(source, account, version, model, failed, signal) {
    const deadline = performance.now() + this.transientRetryWaitMs;
    for (;;) {
      this.assertCurrent(account, version, signal);
      const idle = account.workers.filter(worker => !worker.busy && this.usable(source, worker) &&
        this.workerStatus(source, worker).models.includes(model));
      const other = idle.find(worker => worker !== failed);
      if (other || idle.length) return other || idle[0];
      if (performance.now() >= deadline) return null;
      await pause(250, signal);
    }
  }

  async generate(source, request, signal, onText) {
    if (!this.enabled) throw new PrismError('prism_disabled', 503);
    const account = this.runtime(source);
    const version = account.version;
    if (this.projectIsolation) request.projectScope ||= freshProjectScope();
    const scope = request.projectScope?.id;
    const queuedAt = performance.now();
    let published = false;
    const progress = onText ? value => {
      this.assertCurrent(account, version, signal);
      onText(value);
      published = true;
    } : undefined;
    return account.queue.run(async () => {
      this.assertCurrent(account, version, signal);
      let worker = this.available(source, account, request.model, request.projectScope);
      if (!worker) throw new PrismError('account_busy', 409);
      // Reserve the selected worker before the durable write yields.  Otherwise a second
      // concurrent queue job could select the same worker during journal initialization.
      worker.busy = true;
      if (scope) account.activeScopes.add(scope);
      let turn;
      try { turn = await this.turnJournal.begin(source, request); }
      catch (error) { worker.busy = false; if (scope) account.activeScopes.delete(scope); throw error; }
      let submitted = false;
      const originalSubmitted = request.onSubmitted;
      const originalTurnState = request.onTurnState;
      const turnRequest = { ...request,
        onSubmitted: async details => {
          submitted = true;
          await this.turnJournal.submitted(turn, {
            request_id: details?.request_id,
            conversation_id: details?.conversation_id,
          });
          return originalSubmitted?.(details);
        },
        onTurnState: async details => {
          await this.turnJournal.running(turn, {
            request_id: details?.request_id,
            turn_state: details?.turn_state,
            conversation_id: details?.conversation_id,
          });
          return originalTurnState?.(details);
        },
      };
      const run = async worker => {
        worker.busy = true;
        account.next = (worker.slot + 1) % account.workers.length;
        const startedAt = performance.now();
        try {
          this.assertCurrent(account, version, signal);
          const text = await worker.manager.generate(source, turnRequest, signal, progress);
          this.assertCurrent(account, version, signal);
          return text;
        } finally {
          worker.busy = false;
          this.metrics?.record('worker_turn_ms', performance.now() - startedAt);
          if (process.env.PRISM_AUDIT_REQUESTS === 'true') console.log(JSON.stringify({ event: 'request_timing',
            source, worker: worker.slot, model: request.model, queue_wait_ms: Math.round(startedAt - queuedAt),
            generation_ms: Math.round(performance.now() - startedAt) }));
        }
      };
      this.metrics?.record('queue_wait_ms', performance.now() - queuedAt);
      try {
        for (let attempt = 0; ; attempt += 1) {
          try {
            const result = await run(worker);
            await this.turnJournal.complete(turn);
            return result;
          } catch (error) {
            // Prism's own servers failed (HTTP 5xx on start or status, or a terminal 5xx) before any text was
            // published: one resubmission, after a short pause, on another worker. Anything else, a second
            // failure, or a request that is already gone surfaces as it is.
            if (account.version !== version && error instanceof PrismError && error.code === 'account_not_ready') {
              error = new PrismError('session_revoked', 409);
            }
            if (attempt >= this.transientRetries || published || !(error instanceof PrismError) || error.transient !== true) {
              if (!submitted || error.code === 'prism_start_rejected') await this.turnJournal.clear(turn);
              else await this.turnJournal.fail(turn, error);
              throw error;
            }
            try {
              this.assertCurrent(account, version, signal);
              await pause(this.transientRetryDelayMs, signal);
              const next = await this.retryWorker(source, account, version, request.model, worker, signal);
              if (!next) {
                await this.turnJournal.fail(turn, error);
                throw error;
              }
              if (process.env.PRISM_AUDIT_REQUESTS === 'true') console.log(JSON.stringify({ event: 'transient_retry',
                source, model: request.model, from_worker: worker.slot, to_worker: next.slot, code: error.code }));
              await this.turnJournal.attempt(turn);
              worker = next;
            } catch (retryError) {
              // Cancellation, revocation or a retry wait failure after a native
              // submit is still an unknown upstream outcome. Preserve it for
              // operator reconciliation instead of allowing an unsafe replay.
              if (retryError !== error) {
                if (!submitted) await this.turnJournal.clear(turn);
                else await this.turnJournal.fail(turn, retryError);
              }
              throw retryError;
            }
          }
        }
      } finally {
        if (scope) account.activeScopes.delete(scope);
      }
    }, signal, { canStart: () => { this.assertCurrent(account, version, signal);
      return !(scope && account.activeScopes.has(scope)) && Boolean(this.available(source, account, request.model, request.projectScope)); } });
  }

  async revoke(source, signal) {
    const account = this.runtime(source);
    account.version += 1;
    account.session = null;
    account.queue.cancelPending(new PrismError('session_revoked', 409));
    this.startLimiters.get(source)?.cancelPending(new PrismError('session_revoked', 409));
    const multiplexer = this.multiplexers.get(source);
    this.multiplexers.delete(source);
    const multiplexerClosing = multiplexer?.close();
    const revocations = Promise.all(account.workers.filter(worker => worker.manager.accounts.has(source))
      .map(worker => worker.manager.revoke(source)));
    revocations.catch(() => {});
    return account.queue.exclusive(async () => {
      await revocations;
      await multiplexerClosing;
      aborted(signal);
      return this.status(source);
    }, undefined, { priority: true });
  }

  // Disable admission without permanently stopping the pool. Pending work is
  // cancelled immediately; accepted turns get a short drain window so toggling
  // the switch does not tear down their browser context halfway through a turn.
  // Once the window expires, revoke() closes the active sessions and the shared
  // browser. Account metadata remains loaded so a later provision can initialize
  // a fresh browser lazily.
  setEnabled(enabled) {
    const next = enabled !== false;
    const generation = ++this.lifecycleGeneration;
    const operation = this.lifecyclePromise.catch(() => {}).then(async () => {
      // A newer toggle supersedes queued work. An already-running disable is
      // allowed to finish its release before the newer request is applied.
      if (generation !== this.lifecycleGeneration || this.stopping) return;
      if (next) {
        this.enabled = true;
        try {
          // Recreate the shared browser eagerly when an operator turns Prism on;
          // account sessions are still provisioned with fresh credentials later.
          await this.getBrowser();
        } catch (error) {
          this.enabled = false;
          throw error;
        }
        return;
      }
      this.enabled = false;
      const release = this.releaseResources();
      this.releasePromise = release;
      try { await release; }
      finally { if (this.releasePromise === release) this.releasePromise = null; }
    });
    this.lifecyclePromise = operation.catch(() => {});
    return operation;
  }

  async releaseResources() {
    // Reject queued work while allowing currently running turns to finish.
    for (const account of this.accounts.values()) {
      account.queue.cancelPending(new PrismError('prism_disabled', 503));
    }
    const deadline = Date.now() + this.disableDrainMs;
    while ([...this.accounts.values()].some(account => account.queue.running) && Date.now() < deadline) {
      await new Promise(resolve => setTimeout(resolve, Math.min(25, Math.max(1, deadline - Date.now()))));
    }

    // Preserve the in-memory credentials long enough for the next provision.
    // revoke() intentionally clears them from the account runtime and metadata.
    const sessions = new Map([...this.accounts].map(([source, account]) => [source, account.session && { ...account.session }]));
    const sources = new Set(this.accounts.keys());
    for (const manager of this.managers) for (const source of manager.accounts.keys()) sources.add(source);
    await Promise.allSettled([...sources].map(source => this.revoke(source)));
    for (const [source, session] of sessions) {
      const account = this.accounts.get(source);
      if (account && session) account.session = session;
    }
    this.runtimeCooldowns.clear();
    for (const limiter of this.startLimiters.values()) limiter.close();
    this.startLimiters.clear();
    await Promise.allSettled([...this.multiplexers.values()].map(value => value.close()));
    this.multiplexers.clear();
    const browser = await this.browserPromise?.catch(() => null);
    await browser?.close();
    this.browserPromise = null;
    this.drivers.clear();
  }

  async close() {
    ++this.lifecycleGeneration;
    this.enabled = false;
    // Mark stopping before waiting on the serialized toggle chain so a shared
    // Chromium launch resolving during shutdown is closed and rejected.
    this.stopping = true;
    await this.lifecyclePromise.catch(() => {});
    await this.releasePromise?.catch(() => {});
    for (const limiter of this.startLimiters.values()) limiter.close();
    for (const account of this.accounts.values()) { account.session = null; account.queue.close(); }
    await Promise.allSettled(this.managers.map(manager => manager.close()));
    await Promise.allSettled([...this.multiplexers.values()].map(value => value.close()));
    this.multiplexers.clear();
    const browser = await this.browserPromise?.catch(() => null);
    await browser?.close();
    this.browserPromise = null;
    this.drivers.clear();
  }
}
