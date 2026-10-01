import test from 'node:test';
import assert from 'node:assert/strict';
import { BrowserSession, FALLBACK_MODEL, catalogCollapsed, modelFromLabel } from '../src/browser.mjs';

function route(body, path = '/api/llm/response_with_tools_start') {
  const state = { aborted: false, body: null };
  return { state, request: () => ({ url: () => `https://prism.openai.com${path}`, method: () => 'POST',
    postDataJSON: () => body }), async abort() { state.aborted = true; },
  async continue(options) { state.body = options ? JSON.parse(options.postData) : body; } };
}

test('one UI start replaces the UI text, preserves official metadata and blocks an automatic replay', async () => {
  const driver = new BrowserSession({}, () => {});
  driver.projectId = '01234567-89ab-4cde-8123-0123456789ab';
  const input = [{ type: 'message', role: 'user', content: [{ type: 'input_text', text: 'Original question' }] }];
  driver.turn = { started: false, request: { model: 'gpt-6.1-sol', effort: 'high', input },
    reject(error) { assert.fail(error.code); } };
  const body = { input: [{ role: 'user', content: 'UI placeholder' }], metadata: {
    projectId: driver.projectId, model: 'gpt-6.1-sol', reasoning_effort: 'high', sandbox_token: 'official-token',
    sandbox_url: 'official-url', userId: 'official-identity' }, conversationId: 'official-conversation' };
  const first = route(body);
  await driver.route(first);
  assert.deepEqual(first.state.body.input, input);
  assert.deepEqual(first.state.body.metadata, body.metadata);
  assert.equal(first.state.body.conversationId, body.conversationId);
  const replay = route(body);
  await driver.route(replay);
  assert.equal(replay.state.aborted, true);
});

test('model mismatch and unrelated project mutations are rejected before upstream', async () => {
  const driver = new BrowserSession({}, () => {});
  driver.projectId = '01234567-89ab-4cde-8123-0123456789ab';
  let code;
  driver.turn = { started: false, request: { model: 'gpt-6.1-sol', effort: 'low', input: [] },
    reject(error) { code = error.code; } };
  const wrong = route({ metadata: { projectId: driver.projectId, model: 'gpt-6-astra', reasoning_effort: 'low' } });
  await driver.route(wrong);
  assert.equal(code, 'browser_model_selection_mismatch');
  assert.equal(wrong.state.aborted, true);
  const unrelated = route({}, '/api/projects/other-project/delete');
  await driver.route(unrelated);
  assert.equal(unrelated.state.aborted, true);
});

test('catalog advertises only actual model menu labels', () => {
  assert.equal(modelFromLabel('6.1 Sol'), 'gpt-6.1-sol');
  assert.equal(modelFromLabel('5.6 Terra'), 'gpt-5.6-terra');
  assert.equal(modelFromLabel('6 Luna'), 'gpt-6-luna');
  assert.equal(modelFromLabel('Model6 Astra'), null);
  assert.equal(modelFromLabel('EffortMedium'), null);
});

test('native project UUID is durably reserved before the create is sent', async () => {
  const driver = new BrowserSession({}, () => {});
  driver.creating = true;
  let persisted;
  driver.reserveProject = async id => { persisted = id; };
  const id = '01234567-89ab-4cde-8123-0123456789ab';
  const create = route({ project_uuid: id, title: 'Blank project' }, '/api/projects');
  create.continue = async () => { assert.equal(persisted, id); create.state.body = 'sent'; };
  await driver.route(create);
  assert.equal(create.state.body, 'sent');
  assert.equal(driver.projectId, id);
  const second = route({ project_uuid: '11234567-89ab-4cde-8123-0123456789ab' }, '/api/projects');
  await driver.route(second);
  assert.equal(second.state.aborted, true);
});

const full = ['gpt-6-astra', 'gpt-5.6-sol', 'gpt-5.6-terra', 'gpt-5.6-luna'];

test('catalog decision flags only the one-model fallback list against a larger persisted catalog', () => {
  assert.equal(FALLBACK_MODEL, 'gpt-5.6-sol');
  assert.equal(catalogCollapsed(['gpt-5.6-sol'], full), true);
  assert.equal(catalogCollapsed(['gpt-5.6-sol'], ['gpt-5.6-sol', 'gpt-5.6-terra']), true);
  assert.equal(catalogCollapsed(['gpt-5.6-sol'], ['gpt-5.6-sol']), false);
  assert.equal(catalogCollapsed(['gpt-5.6-sol'], []), false);
  assert.equal(catalogCollapsed(['gpt-5.6-sol']), false);
  assert.equal(catalogCollapsed(['gpt-5.6-terra'], full), false);
  assert.equal(catalogCollapsed(['gpt-5.6-sol', 'gpt-5.6-terra'], full), false);
  assert.equal(catalogCollapsed([], full), false);
});

function catalogDriver(reads, statuses = ['Ready']) {
  const calls = [];
  const driver = new BrowserSession({}, () => {});
  driver.page = { async evaluate() { calls.push('statsig'); return statuses.length > 1 ? statuses.shift() : statuses[0]; },
    async waitForTimeout(ms) { calls.push(`wait ${ms}`); } };
  driver.readModelMenu = async () => { calls.push('read'); return new Map(reads.shift().map(id => [id, `label ${id}`])); };
  return { driver, calls };
}

test('a collapsed catalog is re-read once after three seconds and the second result is accepted', async () => {
  const recovered = catalogDriver([['gpt-5.6-sol'], full]);
  assert.deepEqual([...(await recovered.driver.catalog(full)).keys()], full);
  assert.deepEqual(recovered.calls, ['statsig', 'read', 'wait 3000', 'read']);
  // Still the fallback list on the second read: accepted, never a third read.
  const still = catalogDriver([['gpt-5.6-sol'], ['gpt-5.6-sol'], full]);
  assert.deepEqual([...(await still.driver.catalog(full)).keys()], ['gpt-5.6-sol']);
  assert.equal(still.calls.filter(call => call === 'read').length, 2);
  // A genuinely small catalog (nothing larger persisted) or a full read needs no retry.
  const first = catalogDriver([['gpt-5.6-sol']]);
  assert.deepEqual([...(await first.driver.catalog([])).keys()], ['gpt-5.6-sol']);
  assert.deepEqual(first.calls, ['statsig', 'read']);
  const healthy = catalogDriver([full]);
  assert.deepEqual([...(await healthy.driver.catalog(full)).keys()], full);
  assert.deepEqual(healthy.calls, ['statsig', 'read']);
});

test('the menu is not read until Statsig reports Ready, and a missing Statsig only delays by the timeout', async () => {
  const loading = catalogDriver([full], ['Loading', null, 'Loading', 'Ready']);
  await loading.driver.catalog(full);
  assert.deepEqual(loading.calls, ['statsig', 'wait 250', 'statsig', 'wait 250', 'statsig', 'wait 250', 'statsig', 'read']);
  const absent = catalogDriver([full], [null]);
  const started = Date.now();
  assert.equal(await absent.driver.waitForStatsig(undefined, 30, 5), false);
  assert.ok(Date.now() - started >= 30);
  const failing = new BrowserSession({}, () => {});
  failing.page = { evaluate: () => Promise.reject(new Error('navigation')), async waitForTimeout() {} };
  assert.equal(await failing.waitForStatsig(undefined, 5, 1), false);
  const ready = catalogDriver([full]);
  assert.equal(await ready.driver.waitForStatsig(), true);
  const controller = new AbortController();
  controller.abort();
  await assert.rejects(ready.driver.waitForStatsig(controller.signal), error => error.code === 'request_cancelled');
});
