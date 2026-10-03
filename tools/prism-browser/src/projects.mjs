import { createHash, randomUUID } from 'node:crypto';
import { mkdir, readFile, writeFile, rename, chmod } from 'node:fs/promises';
import { join } from 'node:path';
import { PrismError, aborted } from './errors.mjs';

const digest = value => createHash('sha256').update(value).digest('hex');
const HASH = /^[a-f0-9]{64}$/;
const UUID = /^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$/i;

// Only the trusted gateway can supply the downstream key namespace. The bearer on this
// hop belongs to an upstream account, NOT to the end user; never use it as a tenant key.
export function requestProjectScope(headers = {}) {
  const key = headers['x-prism-key-scope'];
  const session = headers['x-prism-session-scope'];
  for (const value of [key, session]) {
    if (value !== undefined && (typeof value !== 'string' || !HASH.test(value))) {
      throw new PrismError('invalid_project_scope', 400);
    }
  }
  const reusable = Boolean(key && session);
  return { id: reusable ? digest(`prism-project:v1:${key}:${session}`) : digest(randomUUID()), reusable };
}

export function freshProjectScope() { return requestProjectScope(); }

// Opt-in: off keeps the single shared managed project per worker (today's production behaviour).
export function projectIsolationEnabled(value = process.env.PRISM_PROJECT_ISOLATION) {
  return ['true', '1', 'on'].includes(String(value ?? '').trim().toLowerCase());
}

// Bounded, durable mapping of hashed tenant+conversation scopes to projects. Eviction
// forgets a mapping, never reassigns a project to another scope or deletes upstream files.
// Anonymous requests are never persisted or reused. A UUID is persisted before creation
// may escape the client, and only a successfully initialized project becomes reusable.
export class ProjectRegistry {
  constructor({ dataDir, maxSessions = 128, ttlMs = 24 * 60 * 60 * 1000, now = Date.now }) {
    if (!Number.isInteger(maxSessions) || maxSessions < 1 || maxSessions > 4096 ||
      !Number.isInteger(ttlMs) || ttlMs < 1) throw new Error('invalid_project_registry_settings');
    this.dataDir = join(dataDir, 'projects');
    this.maxSessions = maxSessions;
    this.ttlMs = ttlMs;
    this.now = now;
    this.sources = new Map();
    // Entries of sources whose file has finished loading, for the synchronous lookup used by worker choice.
    this.loaded = new Map();
    this.writes = new Map();
  }

  async init() {
    await mkdir(this.dataDir, { recursive: true, mode: 0o700 });
    await chmod(this.dataDir, 0o700);
  }

  async entries(source) {
    if (!/^[1-9][0-9]{0,18}$/.test(source)) throw new PrismError('invalid_source_id', 400);
    if (!this.sources.has(source)) {
      const loading = (async () => {
        let raw;
        try {
          raw = JSON.parse(await readFile(join(this.dataDir, `${source}.json`), 'utf8'));
          await chmod(join(this.dataDir, `${source}.json`), 0o600);
        } catch (error) { if (error.code !== 'ENOENT') throw error; }
        if (raw && (raw.schema !== 1 || raw.source !== source || !Array.isArray(raw.entries))) {
          throw new Error('invalid_project_registry');
        }
        const entries = new Map();
        for (const item of raw?.entries || []) {
          if (!HASH.test(item?.scope || '') || !UUID.test(item?.project_id || '') ||
            !Number.isSafeInteger(item.last_used) || typeof item.ready !== 'boolean') throw new Error('invalid_project_registry');
          if (item.ready && this.now() - item.last_used < this.ttlMs) entries.set(item.scope, item);
        }
        this.trim(entries);
        return entries;
      })();
      this.sources.set(source, loading);
      loading.then(entries => this.loaded.set(source, entries), () => {});
    }
    return this.sources.get(source);
  }

  // The ready project remembered for a scope, or null. Synchronous and read-only: it never loads, creates or
  // refreshes anything, so worker selection can use it without waiting.
  peek(source, scopeId) {
    const item = this.loaded.get(source)?.get(scopeId);
    return item?.ready && this.now() - item.last_used < this.ttlMs ? item : null;
  }

  trim(entries) {
    for (const [scope, item] of entries) if (this.now() - item.last_used >= this.ttlMs) entries.delete(scope);
    while (entries.size > this.maxSessions) {
      const oldest = [...entries].reduce((a, b) => a[1].last_used <= b[1].last_used ? a : b);
      entries.delete(oldest[0]);
    }
  }

  async persist(source, entries) {
    const previous = this.writes.get(source) || Promise.resolve();
    const writing = previous.catch(() => {}).then(async () => {
      this.trim(entries);
      const path = join(this.dataDir, `${source}.json`);
      const temporary = `${path}.${randomUUID()}.tmp`;
      await writeFile(temporary, JSON.stringify({ schema: 1, source, entries: [...entries.values()] }) + '\n',
        { mode: 0o600, flag: 'wx' });
      await rename(temporary, path);
    });
    this.writes.set(source, writing);
    await writing;
  }

  async prepare(source, scope, driver, { signal, models = [], assertCurrent = () => {}, admissionGuard } = {}) {
    if (!scope || !HASH.test(scope.id) || typeof scope.reusable !== 'boolean') throw new PrismError('invalid_project_scope', 400);
    const entries = await this.entries(source);
    this.trim(entries);
    const existing = scope.reusable ? entries.get(scope.id) : null;
    const reused = Boolean(existing?.ready);
    let projectId = existing?.ready ? existing.project_id : null;
    const current = () => { aborted(signal); assertCurrent(); };
    current();
    if (projectId && driver.projectId === projectId && driver.isAlive?.() === true &&
      driver.page && !driver.page.isClosed?.() &&
      driver.lastHeartbeat && this.now() / 1000 - driver.lastHeartbeat < 60) {
      existing.last_used = this.now();
      await this.persist(source, entries);
      current();
      return { projectId, models, reused: true };
    }
    await admissionGuard?.assertAdmission('project');
    current();
    // Mark a reusable project unready BEFORE loading it. A partial reload may set
    // projectId and receive a heartbeat but still fail sandbox sync or catalog discovery.
    if (existing?.ready) { existing.ready = false; await this.persist(source, entries); }
    current();
    try {
      const nextModels = await driver.initialize(projectId, async id => {
        current();
        if (projectId || !UUID.test(id || '')) throw new PrismError('browser_project_mismatch');
        projectId = id;
        if (scope.reusable) {
          entries.set(scope.id, { scope: scope.id, project_id: id, last_used: this.now(), ready: false });
          await this.persist(source, entries);
        }
        current();
      }, signal, models);
      current();
      if (!projectId) throw new PrismError('project_creation_failed');
      if (scope.reusable) {
        entries.set(scope.id, { scope: scope.id, project_id: projectId, last_used: this.now(), ready: true });
        await this.persist(source, entries);
        current();
      }
      return { projectId, models: nextModels, reused };
    } catch (error) {
      // Cancellation/revocation can arrive AFTER the atomic ready:true write. Never
      // leave a completion publication behind if the ownership check rejects it.
      const entry = scope.reusable ? entries.get(scope.id) : null;
      if (entry && entry.project_id === projectId) {
        entry.ready = false;
        await this.persist(source, entries).catch(() => {});
      }
      throw error;
    }
  }
}
