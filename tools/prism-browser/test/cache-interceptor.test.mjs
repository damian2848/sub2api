import test from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { httpCacheEnabled, installCachePreservingInterceptor } from '../src/cache-interceptor.mjs';
import { BrowserSession } from '../src/browser.mjs';

const origin = 'https://prism.openai.com';
const project = '01234567-89ab-4cde-8123-0123456789ab';
const tick = () => new Promise(resolve => setImmediate(resolve));

function cdpFixture() {
  const cdp = new EventEmitter(); const sends = [];
  cdp.send = async (method, params) => { sends.push({ method, params }); if (method === 'Fetch.getResponseBody') return { body: JSON.stringify({ request_id: 'request', turn_state: 'state', status: 'running' }) }; };
  return { cdp, sends };
}

test('real Chromium interceptor never calls Playwright route and caches static assets', async () => {
  const { cdp, sends } = cdpFixture(); let mode;
  await installCachePreservingInterceptor({ newCDPSession: async () => cdp }, { route: () => assert.fail('disables HTTP cache') },
    { origin, route: item => item.continue(), observe: async () => {}, onMode: value => { mode = value; } });
  assert.equal(mode, 'cdp_fetch_static_cache');
  assert.deepEqual(sends.find(item => item.method === 'Network.setCacheDisabled').params, { cacheDisabled: false });
  const patterns = sends.find(item => item.method === 'Fetch.enable').params.patterns;
  assert.deepEqual(patterns[0], { urlPattern: `${origin}/api/*`, requestStage: 'Request' });
  assert.ok(patterns.some(item => item.urlPattern.endsWith('/heartbeat*') && item.requestStage === 'Response'));
});

test('CDP start override preserves request identity and records actual sent input for matching response', async () => {
  const { cdp, sends } = cdpFixture(); const driver = new BrowserSession({}, () => {});
  driver.projectId = project;
  const input = [{ role: 'user', content: [{ type: 'input_text', text: 'PRIVATE REQUEST' }] }];
  driver.turn = { request: { input, model: 'gpt-6.1-sol', effort: 'high' }, submitAllowed: true,
    ownBodies: new Set(), reject: () => assert.fail('valid request') };
  await installCachePreservingInterceptor({ newCDPSession: async () => cdp }, {}, { origin,
    route: item => driver.route(item), observe: item => driver.observe(item) });
  cdp.emit('Fetch.requestPaused', { requestId: 'fetch', networkId: 'network', request: { url: `${origin}/api/llm/response_with_tools_start`, method: 'POST',
    headers: {}, postData: JSON.stringify({ conversationId: 'conversation', input: [], metadata: { projectId: project, model: 'wrong', reasoning_effort: 'low' } }) } });
  await tick();
  const rewritten = JSON.parse(Buffer.from(sends.find(item => item.method === 'Fetch.continueRequest').params.postData, 'base64').toString());
  assert.deepEqual(rewritten.input, input); assert.equal(rewritten.metadata.model, 'gpt-6.1-sol');
  cdp.emit('Fetch.requestPaused', { requestId: 'fetch', networkId: 'network', request: { url: `${origin}/api/llm/response_with_tools_start` }, responseStatusCode: 200 }); await tick();
  assert.equal(driver.turn.requestId, 'request'); assert.deepEqual(driver.turn.startRequest.postDataJSON().input, input);
});

test('CDP cannot bypass unrelated project mutation and attachment upload guards', async () => {
  const { cdp, sends } = cdpFixture(); const driver = new BrowserSession({}, () => {}); driver.projectId = project;
  await installCachePreservingInterceptor({ newCDPSession: async () => cdp }, {}, { origin, route: item => driver.route(item), observe: async () => {} });
  for (const [id, path] of [['mutation', '/api/projects/ffffffff-ffff-ffff-ffff-ffffffffffff/file'], ['upload', '/api/project-files/upload'], ['start', '/api/llm/response_with_tools_start']]) {
    cdp.emit('Fetch.requestPaused', { requestId: id, request: { url: origin + path, method: 'POST', headers: {}, postData: '{}' } });
  }
  await tick(); assert.deepEqual(sends.filter(item => item.method === 'Fetch.failRequest').map(item => item.params.requestId), ['mutation', 'upload', 'start']);
});

test('test double fallback keeps legacy route semantics but clearly marks cache disabled', async () => {
  const calls = []; let mode;
  await installCachePreservingInterceptor({}, { route: async (...args) => calls.push(args), on() {} }, { origin, route: async () => {}, observe: async () => {}, onMode: value => { mode = value; } });
  assert.equal(calls[0][0], origin + '/**'); assert.equal(mode, 'playwright_route_cache_disabled');
});


test('HTTP cache restore can be rolled back to the legacy guarded Playwright path', async () => {
  assert.equal(httpCacheEnabled('true'), true); assert.equal(httpCacheEnabled('false'), false);
  assert.equal(httpCacheEnabled('0'), false); assert.equal(httpCacheEnabled('off'), false);
  let glob; let mode;
  await installCachePreservingInterceptor({ newCDPSession: () => assert.fail('rollback bypasses CDP Fetch') },
    { route: async value => { glob = value; }, on() {} }, { origin, enabled: false,
      route: async () => {}, observe: async () => {}, onMode: value => { mode = value; } });
  assert.equal(glob, origin + '/**'); assert.equal(mode, 'playwright_route_cache_disabled');
  const driver = new BrowserSession({}, () => {}, 'source', 0, { httpCache: false });
  assert.equal(driver.httpCache, false);
});


test('CDP watches backend heartbeat/sync as well as project paths and keeps the legacy unwatched response observer', async () => {
  const { cdp, sends } = cdpFixture(); const page = new EventEmitter();
  const observations = []; let current = true;
  await installCachePreservingInterceptor({ newCDPSession: async () => cdp }, page, { origin,
    route: item => item.continue(), observe: async response => { observations.push(response.url()); }, current: () => current });
  const patterns = sends.find(item => item.method === 'Fetch.enable').params.patterns;
  assert.ok(patterns.some(item => item.urlPattern === origin + '/api/*/heartbeat*' && item.requestStage === 'Response'));
  assert.ok(patterns.some(item => item.urlPattern === origin + '/api/*/wait-for-sync*' && item.requestStage === 'Response'));
  const urls = [origin + '/other/heartbeat', 'https://sandbox.example.invalid/backend/heartbeat',
    origin + '/api/heartbeat', origin + '/auth/session', origin + '/api/backend/1/heartbeat?counter=2',
    origin + '/api/backend/1/wait-for-sync', origin + '/api/llm/response_with_tools_status'];
  for (const url of urls) page.emit('response', { url: () => url });
  await tick();
  assert.deepEqual(observations, urls.slice(0, 4), 'only unwatched responses use the native observer, with no duplicate watched starts/status');
  current = false; page.emit('response', { url: () => origin + '/other/heartbeat' }); await tick();
  assert.equal(observations.length, 4, 'closed/replaced pages cannot restore an old heartbeat');
});
