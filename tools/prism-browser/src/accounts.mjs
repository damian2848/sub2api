import { createHash, randomUUID, timingSafeEqual } from 'node:crypto';
import { mkdir, readdir, readFile, rename, writeFile, chmod } from 'node:fs/promises';
import { join } from 'node:path';
import { AccountQueue } from './queue.mjs';
import { PrismError, aborted } from './errors.mjs';
import { BrowserSession, launchBrowser } from './browser.mjs';

const sourcePattern = /^[1-9][0-9]{0,18}$/;
// Failures that mean the browser session itself is unusable, as opposed to one bad request.
const sessionErrors = new Set(['session_expired', 'browser_session_closed', 'session_closed',
  'browser_project_mismatch', 'browser_previous_context_present']);
export const sessionLevelError = code => sessionErrors.has(code) || /^browser_ui_.+_failed$/.test(code);
const hash = value => createHash('sha256').update(value).digest('hex');
export const validSource = source => sourcePattern.test(source);
export function keyMatches(value, expectedHash) {
  if (typeof value !== 'string' || !expectedHash) return false;
  const digest = hash(value);
  return digest.length === expectedHash.length && timingSafeEqual(Buffer.from(digest), Buffer.from(expectedHash));
}

export class AccountManager {
  constructor({ dataDir, queueLimit = 8, maxAccounts = 16, browserFactory = launchBrowser,
    sessionFactory = (browser, callback, source) => new BrowserSession(browser, callback, source) }) {
    this.dataDir = dataDir;
    this.queueLimit = queueLimit;
    this.maxAccounts = maxAccounts;
    this.browserFactory = browserFactory;
    this.sessionFactory = sessionFactory;
    this.accounts = new Map();
    this.browser = null;
    this.browserPromise = null;
    this.stopping = false;
  }

  async init() {
    await mkdir(this.dataDir, { recursive: true, mode: 0o700 });
    await chmod(this.dataDir, 0o700);
    for (const name of await readdir(this.dataDir)) {
      if (!/^[1-9][0-9]{0,18}\.json$/.test(name)) continue;
      const metadata = JSON.parse(await readFile(join(this.dataDir, name), 'utf8'));
      if (metadata.schema !== 1 || metadata.source !== name.slice(0, -5)) throw new Error('invalid_account_metadata');
      await chmod(join(this.dataDir, name), 0o600);
      this.accounts.set(metadata.source, this.runtime(metadata));
    }
  }

  runtime(metadata) {
    return { metadata, queue: new AccountQueue(this.queueLimit), driver: null, accessHash: null,
      expectedHash: null, version: 0, expiresAt: 0, phase: 'authentication_required', ready: false,
      lastHeartbeat: 0, errorCode: undefined };
  }

  get(source, create = false) {
    if (!validSource(source)) throw new PrismError('invalid_source_id', 400);
    if (this.stopping) throw new PrismError('service_stopping', 503);
    let account = this.accounts.get(source);
    if (!account && create) {
      if (this.accounts.size >= this.maxAccounts) throw new PrismError('account_limit_reached', 429);
      account = this.runtime({ schema: 1, source, project_id: null, key_hash: null, identity_hash: null,
        models: [], verified_project: null, probe_attempted: false, readiness_probe_count: 0 });
      this.accounts.set(source, account);
    }
    if (!account) throw new PrismError('account_not_found', 404);
    return account;
  }

  async persist(account) {
    const path = join(this.dataDir, `${account.metadata.source}.json`);
    const temporary = `${path}.${randomUUID()}.tmp`;
    await writeFile(temporary, JSON.stringify(account.metadata) + '\n', { mode: 0o600, flag: 'wx' });
    await rename(temporary, path);
  }

  async getBrowser() {
    if (!this.browserPromise) this.browserPromise = this.browserFactory().then(browser => {
      this.browser = browser;
      browser.on?.('disconnected', () => {
        this.browser = null;
        this.browserPromise = null;
        for (const account of this.accounts.values()) {
          account.ready = false;
          account.phase = 'authentication_required';
          account.errorCode = 'browser_disconnected';
          account.driver = null;
          account.accessHash = null;
        }
      });
      return browser;
    }).catch(error => { this.browserPromise = null; throw error; });
    return this.browserPromise;
  }

  status(source) {
    const account = this.get(source);
    if (account.driver?.isAlive?.() === false) {
      account.phase = 'authentication_required';
      account.ready = false;
      account.errorCode = 'browser_session_closed';
    }
    if (account.expiresAt && account.expiresAt <= Math.floor(Date.now() / 1000)) {
      account.phase = 'authentication_required';
      account.ready = false;
      account.errorCode = 'oauth_token_expired';
    }
    if (account.ready && Date.now() / 1000 - account.lastHeartbeat > 60) {
      account.ready = false;
      account.phase = 'heartbeat_stale';
      account.errorCode = 'heartbeat_stale';
    }
    return { phase: account.phase, ready: account.ready, models: account.metadata.models,
      ...(account.errorCode ? { error_code: account.errorCode } : {}),
      ...(account.lastHeartbeat ? { last_heartbeat_at: account.lastHeartbeat } : {}),
      ...(account.metadata.project_id ? { project_id: account.metadata.project_id } : {}) };
  }

  validateSession(body) {
    if (!body || typeof body !== 'object' || Array.isArray(body)) throw new PrismError('invalid_session', 400);
    const allowed = new Set(['access_token', 'api_key', 'expected_email', 'expected_user_id', 'expires_at']);
    for (const key of Object.keys(body)) if (!allowed.has(key)) throw new PrismError('unsupported_parameter', 400, key);
    if (typeof body.access_token !== 'string' || body.access_token.length < 20 || body.access_token.length > 32768) {
      throw new PrismError('invalid_access_token', 400);
    }
    if (typeof body.api_key !== 'string' || body.api_key.length < 32 || body.api_key.length > 256) {
      throw new PrismError('invalid_api_key', 400);
    }
    if (!Number.isInteger(body.expires_at) || body.expires_at <= Date.now() / 1000 + 10) {
      throw new PrismError('oauth_token_expired', 400);
    }
    if ((!body.expected_email && !body.expected_user_id) ||
      (body.expected_email !== undefined && (typeof body.expected_email !== 'string' || body.expected_email.length > 320 ||
        !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(body.expected_email))) ||
      (body.expected_user_id !== undefined && (typeof body.expected_user_id !== 'string' ||
        !body.expected_user_id.length || body.expected_user_id.length > 256))) {
      throw new PrismError('expected_oauth_identity_required', 400);
    }
    return hash(JSON.stringify([body.expected_email?.toLowerCase() || '', body.expected_user_id || '']));
  }

  async provision(source, body, signal) {
    const expectedHash = this.validateSession(body);
    const account = this.get(source, true);
    const accessHash = hash(body.access_token);
    const keyHash = hash(body.api_key);
    if (account.driver && account.driver.isAlive?.() !== false && account.accessHash === accessHash &&
      account.expectedHash === expectedHash && account.metadata.key_hash === keyHash) {
      account.expiresAt = body.expires_at;
      return this.status(source);
    }
    const version = account.version;
    return account.queue.run(async () => {
      if (version !== account.version) throw new PrismError('session_revoked', 409);
      if (account.driver && account.driver.isAlive?.() !== false && account.accessHash === accessHash && account.expectedHash === expectedHash) {
        account.expiresAt = body.expires_at;
        account.metadata.key_hash = keyHash;
        await this.persist(account);
        return this.status(source);
      }
      account.ready = false;
      account.phase = 'authenticating';
      account.errorCode = undefined;
      account.lastHeartbeat = 0;
      await account.driver?.close();
      account.driver = null;
      account.accessHash = null;
      const driver = this.sessionFactory(await this.getBrowser(), (timestamp, error) => {
        if (account.driver !== driver) return;
        if (timestamp) account.lastHeartbeat = timestamp;
        if (error) { account.ready = false; account.phase = 'authentication_required'; account.errorCode = error; }
      }, source);
      try {
        const actualUserId = await driver.authenticate(body, signal);
        aborted(signal);
        if (version !== account.version) throw new PrismError('session_revoked', 409);
        if (typeof actualUserId !== 'string' || !actualUserId.length) throw new PrismError('oauth_identity_unavailable', 403);
        const identityHash = hash(actualUserId);
        if (account.metadata.identity_hash && account.metadata.identity_hash !== identityHash) {
          throw new PrismError('source_identity_changed', 409);
        }
        account.driver = driver;
        account.accessHash = accessHash;
        account.expectedHash = expectedHash;
        account.expiresAt = body.expires_at;
        account.metadata.identity_hash = identityHash;
        account.metadata.key_hash = keyHash;
        await this.persist(account);
        account.phase = 'authenticated';
        return this.status(source);
      } catch (error) {
        await driver.close();
        account.phase = 'authentication_required';
        account.errorCode = error instanceof PrismError ? error.code : 'browser_authentication_failed';
        throw error;
      }
    }, signal);
  }

  async bootstrap(source, signal, { retry_probe = false } = {}) {
    const account = this.get(source);
    const version = account.version;
    return account.queue.run(async () => {
      if (version !== account.version) throw new PrismError('session_revoked', 409);
      if (this.status(source).ready && !retry_probe) return this.status(source);
      if (!account.driver || !account.expiresAt || account.expiresAt <= Date.now() / 1000) {
        throw new PrismError('authentication_required', 409);
      }
      account.phase = 'initializing';
      account.errorCode = undefined;
      try {
        const driver = account.driver;
        const models = await driver.initialize(account.metadata.project_id, async id => {
          if (version !== account.version) throw new PrismError('session_revoked', 409);
          account.metadata.project_id = id;
          await this.persist(account);
        }, signal, [...account.metadata.models]);
        aborted(signal);
        if (version !== account.version) throw new PrismError('session_revoked', 409);
        account.metadata.models = models;
        await this.persist(account);
        if (account.metadata.verified_project !== account.metadata.project_id) {
          if (account.metadata.probe_attempted && !retry_probe) throw new PrismError('readiness_probe_requires_reauthorization', 409);
          account.metadata.probe_attempted = true;
          account.metadata.readiness_probe_count = (account.metadata.readiness_probe_count || 0) + 1;
          await this.persist(account);
          account.phase = 'verifying_model';
          const text = await driver.generate({ model: models[0], effort: 'low', input: [{ type: 'message', role: 'user',
            content: [{ type: 'input_text', text: 'Reply with only the uppercase spelling of ready. Do not edit files or use tools.' }] }] }, signal);
          if (text.trim() !== 'READY') throw new PrismError('readiness_probe_failed');
          account.metadata.verified_project = account.metadata.project_id;
          await this.persist(account);
        }
        account.lastHeartbeat = driver.lastHeartbeat;
        if (!account.lastHeartbeat || Date.now() / 1000 - account.lastHeartbeat > 60) throw new PrismError('heartbeat_stale', 503);
        account.phase = 'ready';
        account.ready = true;
        return this.status(source);
      } catch (error) {
        account.ready = false;
        account.phase = 'bootstrap_failed';
        account.errorCode = error instanceof PrismError ? error.code : 'browser_bootstrap_failed';
        if (signal?.aborted) {
          await account.driver?.close();
          account.driver = null;
          account.accessHash = null;
        }
        throw error;
      }
    }, signal);
  }

  // User routes only. An unknown source, a revoked key (DELETE session) or an unprovisioned
  // account is "not ready" (503), never a 401: Sub2API treats an upstream 401 on an API-key
  // account as a dead credential and disables it permanently. 401 means a key hash exists
  // and the presented key does not match it.
  authenticateKey(source, apiKey) {
    if (!validSource(source)) throw new PrismError('invalid_source_id', 400);
    if (this.stopping) throw new PrismError('service_stopping', 503);
    const account = this.accounts.get(source);
    if (!account?.metadata.key_hash) throw new PrismError('account_not_ready', 503);
    if (!keyMatches(apiKey, account.metadata.key_hash)) throw new PrismError('invalid_api_key', 401);
    return account;
  }

  async generate(source, request, signal) {
    const account = this.get(source);
    const version = account.version;
    return account.queue.run(async () => {
      if (version !== account.version) throw new PrismError('session_revoked', 409);
      if (!this.status(source).ready || !account.driver) throw new PrismError('account_not_ready', 503);
      if (!account.metadata.models.includes(request.model)) throw new PrismError('model_not_available', 400, 'model');
      try {
        return await account.driver.generate(request, signal);
      } catch (error) {
        const code = error instanceof PrismError ? error.code : 'browser_generation_failed';
        // One bad request (Prism refused, empty output, cancelled, ...) must not take the
        // account offline. Only a dead browser or a session-level failure does.
        if (account.driver?.isAlive?.() === false || sessionLevelError(code)) {
          account.ready = false;
          account.phase = 'request_failed';
          account.errorCode = code;
        }
        throw error;
      }
    }, signal);
  }

  async revoke(source, signal) {
    const account = this.get(source);
    account.version += 1;
    // Closing immediately also stops an active user turn before revocation queues.
    const closing = account.driver?.close();
    account.driver = null;
    account.accessHash = null;
    account.ready = false;
    return account.queue.run(async () => {
      await closing;
      await account.driver?.close();
      account.driver = null;
      account.accessHash = null;
      account.phase = 'authentication_required';
      account.errorCode = undefined;
      account.expiresAt = 0;
      account.lastHeartbeat = 0;
      account.metadata.key_hash = null;
      account.metadata.probe_attempted = false;
      await this.persist(account);
      return this.status(source);
    }, signal);
  }

  async close() {
    this.stopping = true;
    for (const account of this.accounts.values()) account.queue.close();
    await Promise.allSettled([...this.accounts.values()].map(account => account.driver?.close()));
    await this.browser?.close();
  }
}
