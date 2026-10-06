import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, mkdir, readFile, readdir, rename, rm, stat, symlink, unlink, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { BrowserSession } from '../src/browser.mjs';
import { AccountPoolManager } from '../src/pool.mjs';
import { CONFIGURATION_BODY_LIMIT, PrismConfigurationStore, configurationFromEnvironment,
  validateConfiguration } from '../src/configuration.mjs';

const defaults = { enabled: true, project_isolation: false, http_cache: false, memory_limit_mib: 0, memory_reserve_mib: 32,
  multiplex_pages: false, prewarm_chat: true, stream_reasoning: true };
const changed = { enabled: false, project_isolation: true, http_cache: true, memory_limit_mib: 2048, memory_reserve_mib: 64,
  multiplex_pages: true, prewarm_chat: false, stream_reasoning: false };
const invalid = error => error.code === 'invalid_prism_configuration' && error.status === 400;

async function fixture(t, environment = {}) {
  const root = await mkdtemp(join(tmpdir(), 'prism-config-test-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const dataDir = join(root, 'data');
  const store = new PrismConfigurationStore({ dataDir, environment });
  await store.init();
  return { root, dataDir, store, environment };
}

test('startup configuration preserves the eight environment defaults and normalizes supported env forms', () => {
  assert.deepEqual(configurationFromEnvironment({}), defaults);
  assert.deepEqual(configurationFromEnvironment({ PRISM_PROJECT_ISOLATION: ' ON ', PRISM_HTTP_CACHE: '1',
    PRISM_MULTIPLEX_PAGES: 'true', PRISM_PREWARM_CHAT: 'off', PRISM_STREAM_REASONING: '0',
    PRISM_MEMORY_LIMIT_MIB: '2048', PRISM_MEMORY_RESERVE_MIB: '64', PRISM_BROWSER_ENABLED: 'off',
    PRISM_MANAGEMENT_KEY: 'must-not-be-copied' }), changed);
  for (const environment of [{ PRISM_PROJECT_ISOLATION: 'maybe' }, { PRISM_PREWARM_CHAT: 'maybe' },
    { PRISM_STREAM_REASONING: 'maybe' }, { PRISM_BROWSER_ENABLED: 'maybe' }, { PRISM_MEMORY_LIMIT_MIB: '-1' },
    { PRISM_MEMORY_RESERVE_MIB: '1048577' }, { PRISM_MEMORY_LIMIT_MIB: '32' }]) {
    assert.throws(() => configurationFromEnvironment(environment), invalid);
  }
});

test('full configuration requires typed allowed fields and a usable enabled memory budget', () => {
  const { http_cache: omitted, ...missing } = defaults;
  for (const value of [null, [], false, 1, 'config', {}, missing, { ...defaults, access_token: 'secret' },
    { ...defaults, http_cache: 'true' }, { ...defaults, http_cache: 1 },
    { ...defaults, memory_limit_mib: '2048' }, { ...defaults, memory_reserve_mib: 1.1 },
    { ...defaults, memory_limit_mib: NaN }, { ...defaults, memory_limit_mib: Infinity },
    { ...defaults, memory_limit_mib: -1 }, { ...defaults, memory_limit_mib: 1048577 },
    { ...defaults, memory_limit_mib: 32 }, { ...defaults, memory_limit_mib: 31 }]) {
    assert.throws(() => validateConfiguration(value), invalid);
  }
  assert.deepEqual(validateConfiguration({ ...defaults, memory_reserve_mib: 1048576 }),
    { ...defaults, memory_reserve_mib: 1048576 }, 'a disabled guard may retain any allowed reserve');
  assert.deepEqual(validateConfiguration({ ...defaults, memory_limit_mib: 1048576, memory_reserve_mib: 0 }),
    { ...defaults, memory_limit_mib: 1048576, memory_reserve_mib: 0 });
});

test('PUT persists only the strict schema privately and changes desired, never the effective snapshot', async t => {
  const { dataDir, store } = await fixture(t);
  assert.deepEqual(store.snapshot(), { effective: defaults, desired: defaults,
    restart_required: false, source: 'environment', apply_mode: 'restart' });
  const saved = await store.put(changed);
  assert.deepEqual(saved, { effective: defaults, desired: changed,
    restart_required: true, source: 'saved', apply_mode: 'restart' });
  assert.deepEqual(JSON.parse(await readFile(store.path, 'utf8')), { schema: 1, values: changed });
  assert.equal((await stat(store.path)).mode & 0o777, 0o600);
  assert.equal((await stat(dataDir)).mode & 0o777, 0o700);
  assert.deepEqual(await readdir(dataDir), ['runtime-config.json'], 'no transient files remain');
  saved.effective.project_isolation = true; saved.desired.http_cache = false;
  assert.deepEqual(store.effective, defaults);
  assert.deepEqual(store.snapshot().desired, changed, 'responses do not expose mutable internal config');
  assert.throws(() => { store.effective.http_cache = true; }, TypeError);
});

test('same-as-effective saved settings have no pending restart and apply only on the next startup', async t => {
  const { dataDir, store } = await fixture(t);
  const same = await store.put(defaults);
  assert.equal(same.restart_required, false);
  assert.equal(same.source, 'saved');
  await store.put(changed);
  const restarted = new PrismConfigurationStore({ dataDir, environment: {} });
  await restarted.init();
  assert.deepEqual(restarted.snapshot(), { effective: changed, desired: changed,
    restart_required: false, source: 'saved', apply_mode: 'restart' });
  await restarted.put(defaults);
  assert.deepEqual(restarted.effective, changed);
  await restarted.init();
  assert.deepEqual(restarted.effective, changed, 'init is not a live reload after startup');
});

test('startup migrates a schema 1 configuration written before the enabled switch', async t => {
  const { dataDir, store } = await fixture(t, { PRISM_BROWSER_ENABLED: 'false' });
  const { enabled: omitted, ...legacy } = changed;
  await writeFile(store.path, JSON.stringify({ schema: 1, values: legacy }));
  const restarted = new PrismConfigurationStore({ dataDir, environment: { PRISM_BROWSER_ENABLED: 'false' } });
  await restarted.init();
  assert.equal(restarted.effective.enabled, false, 'legacy files inherit the current environment default');
  assert.deepEqual(restarted.effective, { ...legacy, enabled: false });
  await writeFile(store.path, JSON.stringify({ schema: 1, values: legacy }));
  const enabledByEnvironment = new PrismConfigurationStore({ dataDir, environment: { PRISM_BROWSER_ENABLED: 'true' } });
  await enabledByEnvironment.init();
  assert.equal(enabledByEnvironment.effective.enabled, true);
});

test('DELETE restores the original environment as desired and cannot hot-change an effective saved config', async t => {
  const environment = { PRISM_PREWARM_CHAT: 'false' };
  const { dataDir, store } = await fixture(t, environment);
  await store.put(changed);
  const restarted = new PrismConfigurationStore({ dataDir, environment });
  await restarted.init();
  environment.PRISM_PREWARM_CHAT = 'true';
  const reset = await restarted.reset();
  assert.deepEqual(reset, { effective: changed, desired: { ...defaults, prewarm_chat: false },
    restart_required: true, source: 'environment', apply_mode: 'restart' });
  await assert.rejects(stat(store.path), error => error.code === 'ENOENT');
  assert.deepEqual(await restarted.reset(), reset, 'reset is idempotent');
  const restored = new PrismConfigurationStore({ dataDir, environment: { PRISM_PREWARM_CHAT: 'false' } });
  await restored.init();
  assert.deepEqual(restored.effective, reset.desired);
  assert.equal(restored.snapshot().restart_required, false);
});

test('concurrent PUT and DELETE operations serialize and publish only their own completed states', async t => {
  const { store } = await fixture(t);
  const second = { ...changed, memory_limit_mib: 4096 };
  const results = await Promise.all([store.put(changed), store.reset(), store.put(second)]);
  assert.deepEqual(results.map(value => value.desired), [changed, defaults, second]);
  assert.deepEqual(store.snapshot().desired, second);
  assert.deepEqual(JSON.parse(await readFile(store.path, 'utf8')).values, second);
  const operations = [];
  for (let index = 0; index < 8; index++) operations.push(store.put({ ...changed, memory_limit_mib: 2048 + index }), store.reset());
  await Promise.all(operations);
  assert.equal(store.snapshot().source, 'environment');
  await assert.rejects(stat(store.path), error => error.code === 'ENOENT');
});

test('a failed persistence operation leaves desired unchanged and does not poison later operations', async t => {
  const { dataDir, store } = await fixture(t);
  await store.put(changed);
  const before = store.snapshot();
  await rename(dataDir, dataDir + '-preserved');
  await writeFile(dataDir, 'a file blocks the private directory');
  await assert.rejects(store.put(defaults));
  assert.deepEqual(store.snapshot(), before);
  await assert.rejects(store.reset());
  assert.deepEqual(store.snapshot(), before);
  await unlink(dataDir);
  await rename(dataDir + '-preserved', dataDir);
  assert.deepEqual((await store.put(defaults)).desired, defaults);
  assert.equal((await store.reset()).source, 'environment');
});

test('startup fails closed for corrupt, oversized or invalid saved configuration and does not delete it', async t => {
  const { dataDir, store } = await fixture(t);
  for (const payload of ['not json', '{}', '[]', 'null', JSON.stringify({ schema: 2, values: defaults }),
    JSON.stringify({ schema: 1, values: defaults, secret: 'not-allowed' }),
    JSON.stringify({ schema: 1, values: { ...defaults, http_cache: 'true' } }),
    JSON.stringify({ schema: 1, values: { ...defaults, memory_limit_mib: 31 } }),
    ' '.repeat(CONFIGURATION_BODY_LIMIT + 1)]) {
    await writeFile(store.path, payload);
    const restarting = new PrismConfigurationStore({ dataDir, environment: {} });
    await assert.rejects(restarting.init(), invalid);
    assert.equal(await readFile(store.path, 'utf8'), payload, 'a failed startup must not erase operator data');
  }
});

test('loading a valid saved file hardens its permissions and rejects nonregular files or symlinks', async t => {
  const { dataDir, store } = await fixture(t);
  await writeFile(store.path, JSON.stringify({ schema: 1, values: changed }), { mode: 0o666 });
  const restarted = new PrismConfigurationStore({ dataDir, environment: {} });
  await restarted.init();
  assert.equal((await stat(store.path)).mode & 0o777, 0o600);
  await unlink(store.path); await mkdir(store.path);
  await assert.rejects(new PrismConfigurationStore({ dataDir, environment: {} }).init(), invalid);
  await rm(store.path, { recursive: true });
  const other = join(dataDir, 'other-file.json');
  await writeFile(other, JSON.stringify({ schema: 1, values: changed }));
  await symlink(other, store.path);
  await assert.rejects(new PrismConfigurationStore({ dataDir, environment: {} }).init());
});

test('desired changes cannot affect existing or newly constructed drivers using the effective startup options', async t => {
  const { dataDir, store } = await fixture(t);
  const browserOptions = Object.freeze({ httpCache: store.effective.http_cache, prewarm: store.effective.prewarm_chat });
  const pool = new AccountPoolManager({ dataDir, projectIsolation: store.effective.project_isolation,
    multiplex: store.effective.multiplex_pages, browserOptions });
  const original = new BrowserSession({}, () => {}, '32', 0, pool.browserOptions);
  await store.put(changed);
  const next = new BrowserSession({}, () => {}, '32', 1, pool.browserOptions);
  for (const session of [original, next]) {
    assert.equal(session.httpCache, false);
    assert.equal(session.prewarm, true);
    assert.equal(session.multiplex, false);
  }
  assert.equal(pool.projectIsolation, false);
  assert.equal(pool.multiplex, false);
  assert.equal(pool.managers.every(manager => manager.projectIsolation === false), true);
  await store.reset();
  assert.deepEqual(store.effective, defaults);
});
