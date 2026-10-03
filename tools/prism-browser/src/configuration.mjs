import { constants } from 'node:fs';
import { chmod, mkdir, open, rename, unlink } from 'node:fs/promises';
import { randomUUID } from 'node:crypto';
import { join } from 'node:path';
import { PrismError } from './errors.mjs';

export const CONFIGURATION_BODY_LIMIT = 4096;
const fields = {
  project_isolation: ['PRISM_PROJECT_ISOLATION', false],
  http_cache: ['PRISM_HTTP_CACHE', false],
  memory_limit_mib: ['PRISM_MEMORY_LIMIT_MIB', 0],
  memory_reserve_mib: ['PRISM_MEMORY_RESERVE_MIB', 32],
  multiplex_pages: ['PRISM_MULTIPLEX_PAGES', false],
  prewarm_chat: ['PRISM_PREWARM_CHAT', true],
  stream_reasoning: ['PRISM_STREAM_REASONING', true],
};
const keys = Object.keys(fields);
const invalid = () => new PrismError('invalid_prism_configuration', 400);

// The API and saved file contain only these seven nonsecret startup settings.
// A full replacement prevents partial updates from accidentally inheriting stale UI state.
export function validateConfiguration(value) {
  if (!value || Array.isArray(value) || typeof value !== 'object' ||
    Object.keys(value).length !== keys.length || Object.keys(value).some(key => !Object.hasOwn(fields, key))) {
    throw invalid();
  }
  for (const key of keys) {
    if (!Object.hasOwn(value, key)) throw invalid();
    if (typeof fields[key][1] === 'boolean') {
      if (typeof value[key] !== 'boolean') throw invalid();
    } else if (!Number.isInteger(value[key]) || value[key] < 0 || value[key] > 1048576) {
      throw invalid();
    }
  }
  if (value.memory_limit_mib !== 0 && value.memory_limit_mib <= value.memory_reserve_mib) throw invalid();
  return Object.freeze(Object.fromEntries(keys.map(key => [key, value[key]])));
}

export function configurationFromEnvironment(environment = process.env) {
  const value = {};
  for (const [key, [name, fallback]] of Object.entries(fields)) {
    const raw = environment[name];
    const text = raw === undefined ? '' : String(raw).trim().toLowerCase();
    if (!text) value[key] = fallback;
    else if (typeof fallback === 'boolean') {
      if (!['true', 'false', '1', '0', 'on', 'off'].includes(text)) throw invalid();
      value[key] = ['true', '1', 'on'].includes(text);
    } else value[key] = Number(text);
  }
  return validateConfiguration(value);
}

async function privateDirectory(path) {
  await mkdir(path, { recursive: true, mode: 0o700 });
  await chmod(path, 0o700);
}

async function syncDirectory(path) {
  const directory = await open(path, constants.O_RDONLY);
  try {
    // Persist the rename/removal where directory fsync is supported.
    await directory.sync().catch(error => {
      if (!['EINVAL', 'ENOTSUP', 'EISDIR'].includes(error.code)) throw error;
    });
  } finally { await directory.close(); }
}

async function readSavedConfiguration(path) {
  let file;
  try {
    file = await open(path, constants.O_RDONLY | (constants.O_NOFOLLOW || 0));
  } catch (error) {
    if (error.code === 'ENOENT') return null;
    throw error;
  }
  try {
    const stat = await file.stat();
    if (!stat.isFile() || stat.size > CONFIGURATION_BODY_LIMIT) throw invalid();
    // Bounded even if a concurrently replaced file grew after stat().
    const buffer = Buffer.alloc(CONFIGURATION_BODY_LIMIT + 1);
    let length = 0;
    while (length < buffer.length) {
      const { bytesRead } = await file.read(buffer, length, buffer.length - length, null);
      if (!bytesRead) break;
      length += bytesRead;
    }
    if (length > CONFIGURATION_BODY_LIMIT) throw invalid();
    let saved;
    try { saved = JSON.parse(buffer.subarray(0, length).toString('utf8')); }
    catch { throw invalid(); }
    if (!saved || Array.isArray(saved) || typeof saved !== 'object' || !Object.hasOwn(saved, 'schema') || saved.schema !== 1 ||
      Object.keys(saved).length !== 2 || !Object.hasOwn(saved, 'values')) throw invalid();
    const value = validateConfiguration(saved.values);
    await file.chmod(0o600);
    return value;
  } finally { await file.close(); }
}

export class PrismConfigurationStore {
  #environment;
  #effective;
  #desired;
  #source = 'environment';
  #writes = Promise.resolve();
  #initialized = false;

  constructor({ dataDir, environment = process.env }) {
    this.dataDir = dataDir;
    this.path = join(dataDir, 'runtime-config.json');
    this.#environment = configurationFromEnvironment(environment);
    this.#effective = this.#environment;
    this.#desired = this.#environment;
  }

  // Only startup may establish the effective snapshot. HTTP writes never call init().
  async init() {
    if (this.#initialized) return this.snapshot();
    await privateDirectory(this.dataDir);
    const saved = await readSavedConfiguration(this.path);
    if (saved) {
      this.#effective = saved;
      this.#desired = saved;
      this.#source = 'saved';
    }
    this.#initialized = true;
    return this.snapshot();
  }

  get effective() { return this.#effective; }

  snapshot() {
    return { effective: { ...this.#effective }, desired: { ...this.#desired },
      restart_required: keys.some(key => this.#effective[key] !== this.#desired[key]),
      source: this.#source, apply_mode: 'restart' };
  }

  serialize(work) {
    if (!this.#initialized) return Promise.reject(new Error('prism_configuration_not_initialized'));
    const writing = this.#writes.catch(() => {}).then(work);
    this.#writes = writing;
    return writing;
  }

  async put(body) {
    const value = validateConfiguration(body);
    return this.serialize(async () => {
      await privateDirectory(this.dataDir);
      const temporary = `${this.path}.${randomUUID()}.tmp`;
      let file;
      try {
        file = await open(temporary, 'wx', 0o600);
        await file.writeFile(JSON.stringify({ schema: 1, values: value }) + '\n');
        await file.sync();
        await file.close(); file = null;
        await rename(temporary, this.path);
        await syncDirectory(this.dataDir);
      } finally {
        await file?.close();
        await unlink(temporary).catch(error => { if (error.code !== 'ENOENT') throw error; });
      }
      this.#desired = value;
      this.#source = 'saved';
      return this.snapshot();
    });
  }

  async reset() {
    return this.serialize(async () => {
      await unlink(this.path).catch(error => { if (error.code !== 'ENOENT') throw error; });
      await syncDirectory(this.dataDir);
      this.#desired = this.#environment;
      this.#source = 'environment';
      return this.snapshot();
    });
  }
}
