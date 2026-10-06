import { createHash, randomUUID } from 'node:crypto';
import { chmod, mkdir, open, readdir, readFile, rename, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { PrismError } from './errors.mjs';

// A turn is not replayed automatically after a process restart.  If a native start may have
// reached Prism, the entry remains on disk and the account is kept behind an explicit
// reconciliation endpoint.  Files contain only request metadata and opaque upstream ids;
// prompt text, tool arguments, credentials, and attachments are never written.
export class TurnJournal {
  constructor({ dataDir }) {
    if (typeof dataDir !== 'string' || !dataDir) throw new Error('invalid_prism_data_dir');
    this.root = join(dataDir, 'turns');
    this.entries = new Map();
    this.initialized = false;
  }

  async init() {
    await mkdir(this.root, { recursive: true, mode: 0o700 });
    await chmod(this.root, 0o700).catch(() => {});
    const sources = await readdir(this.root, { withFileTypes: true });
    for (const sourceDir of sources) {
      if (sourceDir.isSymbolicLink()) throw new Error(`prism_turn_journal_symlink:${sourceDir.name}`);
      if (!sourceDir.isDirectory()) throw new Error(`prism_turn_journal_invalid_entry:${sourceDir.name}`);
      if (!/^[1-9][0-9]{0,18}$/.test(sourceDir.name)) throw new Error(`prism_turn_journal_invalid_source:${sourceDir.name}`);
      const directory = join(this.root, sourceDir.name);
      await chmod(directory, 0o700).catch(() => {});
      const files = await readdir(directory, { withFileTypes: true });
      for (const file of files) {
        if (file.isSymbolicLink()) throw new Error(`prism_turn_journal_symlink:${file.name}`);
        if (!file.isFile() || !file.name.endsWith('.json')) throw new Error(`prism_turn_journal_invalid_entry:${file.name}`);
        const path = join(directory, file.name);
        let value;
        try { value = JSON.parse(await readFile(path, 'utf8')); }
        catch { throw new Error(`prism_turn_journal_corrupt:${path}`); }
        if (!value || value.version !== 1 || value.source !== sourceDir.name ||
          typeof value.id !== 'string' || value.id !== file.name.slice(0, -5)) {
          throw new Error(`prism_turn_journal_invalid:${path}`);
        }
        // Every file found while booting is presumed to have survived an interrupted turn,
        // even if its state was still "preparing" when the process died.
        this.entries.set(this.key(value.source, value.id), { ...value, recovered: true, path, write: Promise.resolve() });
      }
    }
    this.initialized = true;
  }

  key(source, id) { return `${source}:${id}`; }

  ensureInitialized() {
    if (!this.initialized) throw new Error('prism_turn_journal_not_initialized');
  }

  sourceEntries(source) {
    return [...this.entries.values()].filter(entry => entry.source === String(source));
  }

  list(source) {
    this.ensureInitialized();
    return this.sourceEntries(source).map(entry => this.publicEntry(entry));
  }

  hasRecovered(source) {
    this.ensureInitialized();
    return this.sourceEntries(source).some(entry => entry.recovered);
  }

  hasBlocking(source) {
    this.ensureInitialized();
    return this.sourceEntries(source).some(entry => entry.recovered || entry.state === 'unknown');
  }

  publicEntry(entry) {
    return {
      id: entry.id, source: entry.source, state: entry.state, attempt: entry.attempt,
      created_at: entry.created_at, updated_at: entry.updated_at,
      request_id: entry.request_id, conversation_id: entry.conversation_id,
      model: entry.model, effort: entry.effort, request_hash: entry.request_hash,
      error_code: entry.error_code,
    };
  }

  async begin(source, request) {
    this.ensureInitialized();
    source = String(source);
    if (this.hasBlocking(source)) throw new PrismError('pending_turn_reconciliation_required', 409);
    const id = randomUUID();
    const now = new Date().toISOString();
    const entry = {
      version: 1, id, source, state: 'preparing', attempt: 1, ever_submitted: false,
      created_at: now, updated_at: now, model: request?.model, effort: request?.effort,
      request_hash: requestHash(request), recovered: false,
      path: join(this.root, source, `${id}.json`), write: Promise.resolve(),
    };
    const directory = join(this.root, source);
    await mkdir(directory, { recursive: true, mode: 0o700 });
    await chmod(directory, 0o700).catch(() => {});
    await this.write(entry);
    this.entries.set(this.key(source, id), entry);
    return this.handle(entry);
  }

  handle(entry) { return { id: entry.id, source: entry.source, entry }; }

  lookup(handle) {
    this.ensureInitialized();
    const entry = handle?.entry || this.entries.get(this.key(handle?.source, handle?.id));
    if (!entry || !this.entries.has(this.key(entry.source, entry.id))) throw new PrismError('pending_turn_not_found', 404);
    return entry;
  }

  async submitted(handle, details = {}) {
    const entry = this.lookup(handle);
    return this.update(entry, { state: 'submitted', ever_submitted: true, ...opaqueDetails(details) });
  }

  async running(handle, details = {}) {
    const entry = this.lookup(handle);
    return this.update(entry, { state: 'running', ...opaqueDetails(details) });
  }

  async attempt(handle) {
    const entry = this.lookup(handle);
    return this.update(entry, { attempt: entry.attempt + 1, state: 'preparing', error_code: undefined });
  }

  async fail(handle, error) {
    const entry = this.lookup(handle);
    // An upstream start refusal is an explicit no-execution result.  It is safe to remove
    // the entry; every other post-submit error remains unknown until an operator resolves it.
    if (!entry.ever_submitted || error?.code === 'prism_start_rejected') {
      return this.clear(handle);
    }
    return this.update(entry, { state: 'unknown', error_code: safeCode(error) });
  }

  async complete(handle) {
    const entry = this.lookup(handle);
    await this.removeFile(entry);
    this.entries.delete(this.key(entry.source, entry.id));
  }

  async clear(handle) {
    const entry = this.lookup(handle);
    await this.removeFile(entry);
    this.entries.delete(this.key(entry.source, entry.id));
  }

  async resolve(source, id) {
    this.ensureInitialized();
    const entry = this.entries.get(this.key(String(source), String(id)));
    if (!entry) throw new PrismError('pending_turn_not_found', 404);
    if (!entry.recovered && ['preparing', 'submitted', 'running'].includes(entry.state)) {
      throw new PrismError('pending_turn_active', 409);
    }
    await this.removeFile(entry);
    this.entries.delete(this.key(entry.source, entry.id));
    return { resolved: true, id: entry.id, source: entry.source };
  }

  async update(entry, patch) {
    const value = { ...patch };
    for (const key of Object.keys(value)) if (value[key] === undefined) delete value[key];
    Object.assign(entry, value, { updated_at: new Date().toISOString() });
    await this.write(entry);
    return this.publicEntry(entry);
  }

  async write(entry) {
    const value = JSON.stringify(serializable(entry)) + '\n';
    // Serialise state transitions for one turn.  A temp file plus rename means a crash leaves
    // either the previous complete record or the new complete record, never partial JSON.
    entry.write = entry.write.then(async () => {
      const temporary = `${entry.path}.${process.pid}.${randomUUID()}.tmp`;
      const file = await open(temporary, 'wx', 0o600);
      try {
        await file.writeFile(value, 'utf8');
        await file.sync();
      } finally { await file.close(); }
      await chmod(temporary, 0o600).catch(() => {});
      await rename(temporary, entry.path);
    });
    return entry.write;
  }

  async removeFile(entry) {
    await entry.write;
    await rm(entry.path, { force: true });
  }
}

function serializable(entry) {
  const { path, write, recovered, ...value } = entry;
  return value;
}

function safeCode(error) {
  return typeof error?.code === 'string' && /^[a-z0-9_.-]{1,96}$/i.test(error.code) ? error.code : 'turn_failed';
}

function opaqueDetails(details) {
  const result = {};
  for (const key of ['request_id', 'turn_state', 'conversation_id']) {
    const value = details?.[key];
    if (typeof value === 'string' && value.length <= 4096) result[key] = value;
  }
  return result;
}

function requestHash(request) {
  const clean = value => {
    if (typeof value === 'function') return undefined;
    if (Buffer.isBuffer(value) || value instanceof Uint8Array) return { bytes: value.byteLength };
    if (Array.isArray(value)) return value.map(clean).filter(item => item !== undefined);
    if (value && typeof value === 'object') {
      const output = {};
      for (const key of Object.keys(value).sort()) {
        if (['onAccepted', 'onReasoning', 'onSubmitted', 'onTurnState'].includes(key)) continue;
        const item = clean(value[key]);
        if (item !== undefined) output[key] = item;
      }
      return output;
    }
    return value;
  };
  return createHash('sha256').update(JSON.stringify(clean(request))).digest('hex');
}
