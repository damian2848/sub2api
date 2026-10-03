// Offline native-page fixture, not a replacement for BrowserSession's routing logic.
// Requests travel through Chromium's real network stack to this local HTTPS server.
import http from 'node:http';
import https from 'node:https';
import net from 'node:net';
import { readFile } from 'node:fs/promises';
import { randomUUID } from 'node:crypto';
import { once } from 'node:events';

export const ORIGIN = 'https://prism.openai.com';
export const USER_ID = 'mock-prism-user';
export const USER_EMAIL = 'offline-smoke@example.invalid';
export const MODELS = ['gpt-5.6-sol', 'gpt-6.1-sol', 'gpt-6-astra'];
export const FAKE_TOKEN = 'offline-smoke-token-not-a-real-credential';

const pageSource = `<!doctype html><html><head><meta charset="utf-8"><title>Offline Prism fixture</title>
<script src="/assets/mock-prism-ui.js" defer></script></head><body>
<button id="new-chat" aria-label="New chat tab">New chat tab</button>
<button id="thinking" aria-label="Thinking: Medium">Thinking: Medium</button>
<div id="tabs"></div><div id="composer"></div><div id="menu" role="menu" hidden>
<div role="menuitem">Model</div><div role="menuitem">5.6 Sol</div><div role="menuitem">6.1 Sol</div>
<div role="menuitem">6 Astra</div></div></body></html>`;

// The selectors, native Enter handler and React prompt shape intentionally mirror the
// small contracts BrowserSession consumes, without serving official assets or tokens.
const scriptTemplate = `(() => {
  const DRAIN_SANDBOX = __DRAIN_SANDBOX__;
  const models = ${JSON.stringify(MODELS)};
  window.__STATSIG__ = {firstInstance: {loadingStatus: 'Loading', getDynamicConfig: () => ({
    get: (key, fallback) => key === 'models' ? models.map(id => ({id, label: id})) : fallback})}};
  setTimeout(() => { window.__STATSIG__.firstInstance.loadingStatus = 'Ready'; }, 25);
  window.mockPrism = { projectId: new URLSearchParams(location.search).get('u'), previousResponseId: null,
    projectOverride: null, nativeModel: 'gpt-5.6-sol', pending: new Map(), tabs: 0 };
  const state = window.mockPrism;
  // The official page wraps window.fetch: it attaches the Sentinel proof to model API calls. The native
  // editor sends its own proof explicitly below; a request made with the UNWRAPPED fetch carries none.
  window.__prismOriginalFetch = window.fetch;
  window.SentinelSDK = { token: async () => 'offline-native-proof' };
  window.fetch = async (input, init = {}) => {
    const url = typeof input === 'string' ? input : input.url;
    if (url.includes('/api/llm/') && !(init.headers && (init.headers['OpenAI-Sentinel-Token'] || init.headers['openai-sentinel-token']))) {
      init = { ...init, headers: { ...(init.headers || {}), 'OpenAI-Sentinel-Token': await window.SentinelSDK.token() } };
    }
    return window.__prismOriginalFetch.call(window, input, init);
  };
  const post = async (path, body, headers = {}) => {
    const response = await fetch(path, {method: 'POST',
      headers: {'Content-Type':'application/json', ...headers}, body: JSON.stringify(body)});
    // Deliberately do not consume sandbox responses: the CDP observer must
    // capture them even when the official UI only checks response.ok.
    // Production-like pages read every body. DRAIN_SANDBOX=false models a page that only checks response.ok
    // for sandbox calls: Playwright route mode cannot read those bodies, the CDP response-stage observer can.
    if (DRAIN_SANDBOX || (!path.endsWith('/heartbeat') && !path.endsWith('/wait-for-sync'))) await response.clone().arrayBuffer();
    return response;
  };
  const composerRoot = document.getElementById('composer');
  const tabsRoot = document.getElementById('tabs');
  const complete = entry => { clearTimeout(entry.timer); entry.stop.remove(); state.pending.delete(entry.request_id); };
  function newChat() {
    document.querySelectorAll('[data-tab-id]').forEach(tab => tab.className = '');
    composerRoot.querySelectorAll('.chat-composer').forEach(node => node.hidden = true);
    const tabId = 'chat:' + (Date.now() + ++state.tabs);
    const tab = document.createElement('div'); tab.dataset.tabId = tabId;
    tab.className = '--tabs-active-border'; tab.textContent = tabId; tabsRoot.append(tab);
    const wrap = document.createElement('div'); wrap.className = 'chat-composer'; composerRoot.append(wrap);
    const input = document.createElement('textarea'); input.setAttribute('aria-label', 'Message'); wrap.append(input);
    const upload = document.createElement('input'); upload.type = 'file'; wrap.append(upload);
    const prompt = {content: []};
    input.__reactFiber$smoke = {memoizedProps: {prompt, pendingUploads: []}, return: null};
    tab.addEventListener('mousedown', event => { if (event.button === 1) {tab.remove(); wrap.remove();} });
    upload.addEventListener('change', async () => {
      const file = upload.files[0]; if (!file) return;
      const props = input.__reactFiber$smoke.memoizedProps; props.pendingUploads = [file.name];
      const response = await fetch('/api/project-files/upload', {method:'POST', headers: {
        'X-Prism-Project-Id': state.projectId, 'X-Prism-File-Name': encodeURIComponent(file.name)}, body: file});
      const data = await response.json();
      if (response.ok) prompt.content.push({type:'input_file', filename:file.name, project_path:data.project_path});
      props.pendingUploads = [];
    });
    input.addEventListener('keydown', async event => {
      if (event.key !== 'Enter' || event.shiftKey) return;
      event.preventDefault();
      const conversationId = crypto.randomUUID();
      const response = await post('/api/llm/response_with_tools_start', {conversationId,
        previousResponseId: state.previousResponseId,
        input:[{role:'user',content:[{type:'input_text',text:input.value}, ...prompt.content]}],
        metadata:{projectId:state.projectOverride || state.projectId, model:state.nativeModel,
          reasoning_effort:'medium', native_only:'preserved-metadata'}}, {'OpenAI-Sentinel-Token':'offline-native-proof'});
      if (!response.ok) return;
      const data = await response.json(); if (['completed','error','failed'].includes(data.status)) return;
      const stop = document.createElement('button'); stop.dataset.testid = 'ai-stop-button'; stop.textContent = 'Stop';
      wrap.append(stop);
      const entry = {request_id:data.request_id, conversation_id:conversationId, turn_state:data.turn_state, stop};
      state.pending.set(entry.request_id, entry);
      stop.addEventListener('click', async () => {
        await post('/api/llm/response_with_tools_stop', {request_id:entry.request_id,
          conversation_id:conversationId, turn_state:entry.turn_state}); complete(entry);
      });
      const poll = async () => {
        if (!state.pending.has(entry.request_id)) return;
        try {
          const status = await post('/api/llm/response_with_tools_status', {request_id:entry.request_id,
            conversation_id:conversationId, turn_state:entry.turn_state, diff_format:'mock-native', ui_poll:true});
          if (status.ok) { const result = await status.json(); entry.turn_state = result.turn_state || entry.turn_state;
            if (['completed','error','failed'].includes(result.status)) return complete(entry); }
        } catch {}
        entry.timer = setTimeout(poll, 80);
      };
      entry.timer = setTimeout(poll, 50);
    });
  }
  document.getElementById('new-chat').addEventListener('click', newChat);
  document.getElementById('thinking').addEventListener('click', () => {document.getElementById('menu').hidden = false;});
  document.addEventListener('keydown', event => {if (event.key === 'Escape') document.getElementById('menu').hidden = true;});
  newChat();
  (async () => {
    if (new URLSearchParams(location.search).has('n')) {
      state.projectId = crypto.randomUUID();
      const response = await post('/api/projects', {project_uuid:state.projectId});
      if (!response.ok) return;
      await post('/api/backend/1/new', {project_id:state.projectId});
    }
    if (!state.projectId) return;
    await post('/api/projects/' + state.projectId + '/wait-for-sync', {});
    await post('/api/backend/1/heartbeat', {project_id:state.projectId});
  })();
})();`;

const listen = async server => { server.listen(0, '127.0.0.1'); await once(server, 'listening'); return server.address().port; };
const json = (response, status, body) => {
  response.writeHead(status, {'Content-Type':'application/json', 'Cache-Control':'no-store'});
  response.end(JSON.stringify(body));
};

export async function createMockPrism() {
  const [key, cert] = await Promise.all([
    readFile(new URL('./fixtures/mock-key.pem', import.meta.url)),
    readFile(new URL('./fixtures/mock-cert.pem', import.meta.url)),
  ]);
  const records = [];
  const scenarios = [];
  const turns = new Map();
  const projects = new Set();
  const blockedConnections = [];
  const unverified = [];
  const sockets = new Set();
  let auth = {user:{id:USER_ID, email:USER_EMAIL, is_anonymous:false}};
  let assetHits = 0;
  let drainSandbox = true;
  let active = 0;
  let peakActive = 0;
  const server = https.createServer({key, cert}, async (request, response) => {
    try {
      const chunks = []; for await (const chunk of request) chunks.push(chunk);
      const raw = Buffer.concat(chunks);
      let body = null; try {body = JSON.parse(raw.toString());} catch {}
      const url = new URL(request.url, ORIGIN);
      const record = {method:request.method, path:url.pathname, query:url.search,
        headers:request.headers, body, raw};
      records.push(record);
      if (url.pathname === '/') {response.writeHead(200, {'Content-Type':'text/html', 'Cache-Control':'no-store'}); return response.end(pageSource);}
      if (url.pathname === '/assets/mock-prism-ui.js') {
        assetHits += 1; response.writeHead(200, {'Content-Type':'text/javascript', 'Cache-Control':'public,max-age=3600,immutable'});
        return response.end(scriptTemplate.replace('__DRAIN_SANDBOX__', String(drainSandbox)));
      }
      if (url.pathname === '/auth/session') return json(response, 200, auth);
      if (url.pathname === '/api/projects' && request.method === 'POST') {
        projects.add(body.project_uuid); return json(response, 200, {uuid:body.project_uuid});
      }
      if (url.pathname.endsWith('/wait-for-sync')) return json(response, 200, {status:'synced'});
      if (url.pathname.endsWith('/heartbeat')) return json(response, 200, {status:'healthy'});
      if (url.pathname === '/api/project-files/upload') {
        return json(response, 200, {project_path:'/prism-uploads/' + decodeURIComponent(request.headers['x-prism-file-name'])});
      }
      if (url.pathname.startsWith('/api/llm/') && !request.headers['openai-sentinel-token']) {
        unverified.push({path:url.pathname, method:request.method});
        response.writeHead(403, {'Content-Type':'text/html'});
        return response.end('<html>Request verification failed</html>');
      }
      if (url.pathname === '/api/llm/response_with_tools_start') {
        const scenario = scenarios.shift() || {};
        if (scenario.startHttp) return json(response, scenario.startHttp, {error:'offline fixture failure'});
        const id = randomUUID();
        const turn = {id, body, scenario, polls:0, state:'mock-turn-state', finished:false, stopped:false};
        turns.set(id, turn); active += 1; peakActive = Math.max(peakActive, active);
        if (scenario.startTerminal) return finish(response, turn);
        return json(response, 200, {request_id:id, conversation_id:body.conversationId, status:'running', turn_state:turn.state});
      }
      if (url.pathname === '/api/llm/response_with_tools_status') {
        const turn = turns.get(body.request_id);
        if (!turn || turn.body.conversationId !== body.conversation_id) return json(response, 400, {error:'unknown mock turn'});
        turn.polls += 1;
        if (turn.scenario.statusFailures > 0) {turn.scenario.statusFailures -= 1; return json(response, 503, {error:'temporary poll failure'});}
        if (!turn.scenario.hold && turn.polls >= (turn.scenario.polls || 2)) return finish(response, turn);
        turn.state = 'mock-turn-state-' + turn.polls;
        return json(response, 200, {request_id:turn.id, conversation_id:turn.body.conversationId,
          turn_state:turn.state, status:'running', codex_live_progress:{toolCalls:[{name:'read_file', call_id:'mock-read', line_index:1}]}});
      }
      if (url.pathname === '/api/llm/response_with_tools_stop') {
        const turn = turns.get(body.request_id);
        if (turn) {turn.stopped = true; if (!turn.finished) {turn.finished = true; active -= 1;}}
        return json(response, 200, {status:'stopped'});
      }
      if (url.pathname.startsWith('/api/')) return json(response, 200, {ok:true});
      response.writeHead(404); response.end();
    } catch (error) { if (!response.headersSent) json(response, 500, {error:error.message}); else response.end(); }
  });
  function finish(response, turn) {
    if (!turn.finished) {turn.finished = true; active -= 1;}
    const payload = turn.scenario.payload || {model:turn.scenario.model || turn.body.metadata.model,
      output:turn.scenario.output || [{type:'message', content:[{type:'output_text', text:turn.scenario.text || 'offline mock answer'}]}]};
    return json(response, 200, {status:'completed', request_id:turn.id, conversation_id:turn.body.conversationId,
      turn_state:turn.state, response:{status:turn.scenario.failure ? 'error':'success', payload}});
  }
  const upstreamPort = await listen(server);
  // Every destination other than the mock Prism origin is rejected, never forwarded.
  // This prevents fake credentials and fixture traffic reaching a production host.
  const proxy = http.createServer((request, response) => {
    blockedConnections.push(request.url); response.writeHead(403); response.end('Offline smoke blocks external HTTP');
  });
  proxy.on('connect', (request, socket, head) => {
    socket.on('error', () => {});
    if (request.url !== 'prism.openai.com:443') {
      blockedConnections.push(request.url); socket.end('HTTP/1.1 403 Forbidden\r\n\r\n'); return;
    }
    const upstream = net.connect(upstreamPort, '127.0.0.1');
    sockets.add(socket); sockets.add(upstream);
    socket.on('error', () => upstream.destroy()); upstream.on('error', () => socket.destroy());
    socket.on('close', () => {sockets.delete(socket); upstream.destroy();});
    upstream.on('close', () => {sockets.delete(upstream); socket.destroy();});
    upstream.once('connect', () => {
      socket.write('HTTP/1.1 200 Connection Established\r\n\r\n');
      if (head.length) upstream.write(head);
      socket.pipe(upstream); upstream.pipe(socket);
    });
  });
  const proxyPort = await listen(proxy);
  return {
    records, turns, projects, blockedConnections, unverified,
    proxyURL:`http://127.0.0.1:${proxyPort}`,
    enqueue(scenario = {}) {scenarios.push({...scenario});},
    setAuth(value) {auth = value;},
    // Applies to pages loaded afterwards (the script is cached per browser context).
    setDrainSandbox(value) {drainSandbox = Boolean(value);},
    get assetHits() {return assetHits;},
    get peakActive() {return peakActive;},
    async close() {
      for (const socket of sockets) socket.destroy();
      server.closeAllConnections(); proxy.closeAllConnections();
      await Promise.all([new Promise(resolve => server.close(resolve)), new Promise(resolve => proxy.close(resolve))]);
    },
  };
}
