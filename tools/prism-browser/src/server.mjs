import { createServer } from 'node:http';
import { pathToFileURL } from 'node:url';
import { keyMatches } from './accounts.mjs';
import { AccountPoolManager } from './pool.mjs';
import { createHash } from 'node:crypto';
import { PrismError, aborted, publicError } from './errors.mjs';
import { createStreamWriter, parseRequest, resultBody } from './protocol.mjs';
import { PromptCache } from './prompt-cache.mjs';
import { attachmentLimits, resolveAttachments } from './attachments.mjs';
import { nativeStartSettings } from './start-limit.mjs';
import { ProjectRegistry, projectIsolationEnabled, requestProjectScope } from './projects.mjs';
import { ResourceGuard, RuntimeMetrics } from './resources.mjs';
import { multiplexEnabled } from './page-multiplexer.mjs';

function bearer(req) {
  const value = req.headers.authorization;
  return typeof value === 'string' && /^Bearer [^\s]+$/.test(value) ? value.slice(7) : '';
}

function send(res, status, value, retryAfterSeconds) {
  if (res.destroyed || res.writableEnded) return;
  const body = JSON.stringify(value);
  res.writeHead(status, { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body),
    'Cache-Control': 'no-store', 'X-Content-Type-Options': 'nosniff',
    ...(Number.isInteger(retryAfterSeconds) ? { 'Retry-After': String(retryAfterSeconds) } : {}) });
  res.end(body);
}

export function readJSON(req, limit) {
  if (req.headers['content-encoding']) throw new PrismError('content_encoding_not_supported', 415);
  if (req.headers['content-type'] && !/^application\/json(?:\s*;|$)/i.test(req.headers['content-type'])) {
    throw new PrismError('content_type_must_be_json', 415);
  }
  if (Number(req.headers['content-length']) > limit) {
    req.resume();
    throw new PrismError('request_body_too_large', 413);
  }
  return new Promise((resolve, reject) => {
    const chunks = [];
    let size = 0;
    const cleanup = () => { req.off('data', onData); req.off('end', onEnd); req.off('error', onError); req.off('aborted', onAbort); };
    const onError = () => { cleanup(); reject(new PrismError('request_body_unreadable', 400)); };
    const onAbort = () => { cleanup(); reject(new PrismError('request_cancelled', 499)); };
    const onData = chunk => {
      size += chunk.length;
      if (size > limit) { cleanup(); req.resume(); reject(new PrismError('request_body_too_large', 413)); }
      else chunks.push(chunk);
    };
    const onEnd = () => {
      cleanup();
      try { resolve(JSON.parse(Buffer.concat(chunks).toString('utf8') || '{}')); }
      catch { reject(new PrismError('invalid_json', 400)); }
    };
    req.on('data', onData); req.on('end', onEnd); req.on('error', onError); req.on('aborted', onAbort);
  });
}

// PRISM_STREAM_REASONING=false stops forwarding Prism's progress notes as reasoning events.
export function streamReasoningEnabled(value = process.env.PRISM_STREAM_REASONING) {
  return !['false', '0', 'off'].includes(String(value ?? '').trim().toLowerCase());
}

export function createPrismServer({ manager, managementKey, bodyLimit = 8 * 1024 * 1024, requestTimeout = 1800000,
  sessionTimeout = 30000, keepaliveMs = 10000, maxTextBytes, maxTranscriptChars,
  maxAttachments, maxAttachmentBytes, maxTotalAttachmentBytes, promptCache = new PromptCache(),
  streamReasoning = streamReasoningEnabled(), projectIsolation = false }) {
  if (typeof managementKey !== 'string' || managementKey.length < 32) throw new Error('PRISM_MANAGEMENT_KEY must have at least 32 characters');
  const managementHash = createHash('sha256').update(managementKey).digest('hex');
  // Codex requests (tool schemas plus history) are large; management bodies are not.
  const managementLimit = Math.min(bodyLimit, 128 * 1024);
  const mediaLimits = attachmentLimits({ maxAttachments, maxAttachmentBytes, maxTotalAttachmentBytes });
  const server = createServer(async (req, res) => {
    const controller = new AbortController();
    let timer;
    let keepalive;
    let streamWriter;
    const disconnect = () => { if (!res.writableEnded) controller.abort(new PrismError('request_cancelled', 499)); };
    req.on('aborted', disconnect);
    res.on('close', disconnect);
    res.on('error', disconnect);
    try {
      const url = new URL(req.url, 'http://localhost');
      if (url.search || url.hash) throw new PrismError('query_parameters_not_supported', 400);
      if (req.method === 'GET' && url.pathname === '/health') return send(res, 200, { status: 'ok' });
      if (url.pathname === '/internal/resources' && req.method === 'GET') {
        if (!keyMatches(bearer(req), managementHash)) throw new PrismError('invalid_management_key', 401);
        return send(res, 200, await manager.resources?.() || { available: false });
      }
      const internal = /^\/internal\/accounts\/([1-9][0-9]{0,18})\/(session|bootstrap|status)$/.exec(url.pathname);
      const user = /^\/accounts\/([1-9][0-9]{0,18})\/v1\/(models|responses|chat\/completions)$/.exec(url.pathname);
      if (!internal && !user) throw new PrismError('route_not_found', 404);
      timer = setTimeout(() => controller.abort(new PrismError('request_timeout', 504)),
        internal?.[2] === 'session' && req.method === 'PUT' ? sessionTimeout : requestTimeout);
      if (internal) {
        if (!keyMatches(bearer(req), managementHash)) throw new PrismError('invalid_management_key', 401);
        const [, source, action] = internal;
        if (action === 'status' && req.method === 'GET') return send(res, 200, manager.status(source));
        if (action === 'session' && req.method === 'PUT') return send(res, 200,
          await manager.provision(source, await readJSON(req, managementLimit), controller.signal));
        if (action === 'session' && req.method === 'DELETE') return send(res, 200,
          await manager.revoke(source, controller.signal));
        if (action === 'bootstrap' && req.method === 'POST') {
          const options = await readJSON(req, managementLimit);
          if (!options || Array.isArray(options) || typeof options !== 'object' ||
            Object.keys(options).some(key => key !== 'retry_probe') ||
            (options.retry_probe !== undefined && typeof options.retry_probe !== 'boolean')) {
            throw new PrismError('invalid_bootstrap_options', 400);
          }
          return send(res, 200, await manager.bootstrap(source, controller.signal, options));
        }
        throw new PrismError('method_not_allowed', 405);
      }
      const [, source, action] = user;
      manager.authenticateKey(source, bearer(req));
      if (action === 'models' && req.method === 'GET') {
        const status = manager.status(source);
        if (!status.ready) throw new PrismError('account_not_ready', 503);
        return send(res, 200, { object: 'list', data: status.models.map(id => ({
          id, object: 'model', created: 0, owned_by: 'prism',
          input_modalities: ['text', 'image'], output_modalities: ['text'] })) });
      }
      if (!['responses', 'chat/completions'].includes(action) || req.method !== 'POST') {
        throw new PrismError('method_not_allowed', 405);
      }
      const status = manager.status(source);
      if (!status.ready) throw new PrismError('account_not_ready', 503);
      const request = parseRequest(await readJSON(req, bodyLimit), action === 'responses' ? 'responses' : 'chat',
        status.models, { ...mediaLimits, ...(maxTextBytes ? { maxTextBytes } : {}),
          ...(maxTranscriptChars ? { maxTranscriptChars } : {}) });
      // Scope headers are only read when isolation is on; otherwise they are ignored, never rejected.
      if (projectIsolation) request.projectScope = requestProjectScope(req.headers);
      request.failover = req.headers['x-prism-failover'] === 'none' ? 'none' : 'available';
      if (request.attachments?.length) {
        request.attachments = await resolveAttachments(request.attachments, { signal: controller.signal, limits: mediaLimits });
      }
      // A stream opens only once Prism has accepted the start. Until then nothing is sent, so a start
      // Prism refuses can still be answered with a plain 429 and the gateway can use another account.
      let streamOpening = null;
      const openStream = () => streamOpening ??= (async () => {
        res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache, no-store',
          Connection: 'keep-alive', 'X-Accel-Buffering': 'no', 'X-Prism-Usage': 'estimated' });
        res.flushHeaders();
        streamWriter = createStreamWriter(res, request, { signal: controller.signal, onDisconnect: disconnect });
        await streamWriter.begin();
        keepalive = setInterval(() => {
          streamWriter.heartbeat().catch(disconnect);
        }, keepaliveMs);
      })();
      if (request.stream) request.onAccepted = () => { if (!res.destroyed) openStream().catch(disconnect); };
      // Prism reports reasoning summaries and tool progress while it works. Responses clients get them as a
      // reasoning item, so a long generation is not silent. The answer itself still arrives with the result.
      if (request.stream && request.family === 'responses' && streamReasoning) {
        const sent = new Set();
        request.onReasoning = text => {
          if (res.destroyed || res.writableEnded || sent.has(text)) return;
          sent.add(text);
          openStream().then(() => streamWriter.reasoning(text)).catch(disconnect);
        };
      }
      aborted(controller.signal);
      const text = await manager.generate(source, request, controller.signal);
      aborted(controller.signal);
      if (request.stream) await openStream();
      // Only a prompt Prism has processed can be in its cache, so it is recorded after success.
      const prompt = request.input.map(item => item.content.filter(part => part.type === 'input_text')
        .map(part => part.text).join('')).join('\n');
      // The text markers do not describe attachment contents or Prism's native file inspection.
      // Isolated projects estimate cache reads per conversation scope (none for anonymous requests);
      // without isolation the estimate stays per source account, as before.
      const cachedTokens = request.attachments?.length ? 0 : !projectIsolation ? promptCache.observe(source, prompt)
        : request.projectScope?.reusable ? promptCache.observe(`${source}:${request.projectScope.id}`, prompt) : 0;
      const result = resultBody(request, text, streamWriter?.identity, { cachedTokens });
      if (res.destroyed) return;
      if (request.stream) await streamWriter.finish(result);
      else { res.setHeader('X-Prism-Usage', 'estimated'); send(res, 200, result); }
    } catch (error) {
      if (res.destroyed || res.writableEnded) return;
      const output = publicError(error);
      if (streamWriter) await streamWriter.error(error).catch(disconnect);
      else send(res, error instanceof PrismError ? error.status : 502, output, error?.retryAfterSeconds);
    } finally {
      clearTimeout(timer);
      clearInterval(keepalive);
      req.off('aborted', disconnect);
      res.off('close', disconnect);
      res.off('error', disconnect);
    }
  });
  server.requestTimeout = requestTimeout + 10000;
  server.headersTimeout = 15000;
  return server;
}

export async function main() {
  const integer = (name, fallback, min, max) => {
    const value = Number(process.env[name] || fallback);
    if (!Number.isInteger(value) || value < min || value > max) throw new Error(`invalid ${name}`);
    return value;
  };
  for (const name of ['PRISM_HTTP_CACHE', 'PRISM_MULTIPLEX_PAGES', 'PRISM_PROJECT_ISOLATION']) {
    const value = process.env[name];
    if (value !== undefined && value !== '' && !['true', 'false', '1', '0', 'on', 'off'].includes(value.trim().toLowerCase())) {
      throw new Error(`invalid ${name}`);
    }
  }
  const dataDir = process.env.PRISM_DATA_DIR || '/data';
  const projectIsolation = projectIsolationEnabled();
  const metrics = new RuntimeMetrics();
  const admissionGuard = new ResourceGuard({
    // 0 (default) = no admission guard. Set it from /internal/resources measurements, not a guess.
    limitBytes: integer('PRISM_MEMORY_LIMIT_MIB', 0, 0, 1048576) * 1024 * 1024,
    reserveBytes: integer('PRISM_MEMORY_RESERVE_MIB', 32, 0, 1048576) * 1024 * 1024,
    onAudit: (event, fields) => { if (process.env.PRISM_AUDIT_REQUESTS === 'true') console.log(JSON.stringify({ event, ...fields })); } });
  const manager = new AccountPoolManager({ dataDir, admissionGuard, metrics, projectIsolation,
    multiplex: multiplexEnabled(), browserOptions: { pollMs: integer('PRISM_STATUS_POLL_MS', 1000, 0, 10000) },
    projectRegistry: new ProjectRegistry({ dataDir,
      maxSessions: integer('PRISM_MAX_SESSION_PROJECTS', 128, 1, 4096),
      ttlMs: integer('PRISM_SESSION_PROJECT_TTL_SECONDS', 86400, 1, 604800) * 1000 }),
    queueLimit: integer('PRISM_QUEUE_LIMIT', 8, 1, 64), maxAccounts: integer('PRISM_MAX_ACCOUNTS', 16, 1, 256),
    concurrency: integer('PRISM_ACCOUNT_CONCURRENCY', 2, 1, 4), maxWorkers: integer('PRISM_MAX_WORKERS', 32, 1, 1024),
    startOptions: nativeStartSettings(),
    transientRetries: integer('PRISM_TRANSIENT_RETRIES', 1, 0, 1),
    transientRetryDelayMs: integer('PRISM_TRANSIENT_RETRY_DELAY_SECONDS', 4, 0, 60) * 1000,
    transientRetryWaitMs: integer('PRISM_TRANSIENT_RETRY_WAIT_SECONDS', 15, 0, 120) * 1000,
    startCooldownMs: integer('PRISM_START_COOLDOWN_SECONDS', 60, 0, 600) * 1000 });
  // Read by each browser session; validated here so a bad value stops startup. 0 or 250-10000.
  const statusPollMs = integer('PRISM_STATUS_POLL_MS', 1000, 0, 10000);
  if (statusPollMs > 0 && statusPollMs < 250) throw new Error('invalid PRISM_STATUS_POLL_MS');
  await manager.init();
  const server = createPrismServer({ manager, managementKey: process.env.PRISM_MANAGEMENT_KEY,
    bodyLimit: integer('PRISM_BODY_LIMIT', 8 * 1024 * 1024, 4096, 32 * 1024 * 1024),
    maxAttachments: integer('PRISM_MAX_ATTACHMENTS', 8, 1, 32),
    maxAttachmentBytes: integer('PRISM_MAX_ATTACHMENT_BYTES', 10 * 1024 * 1024, 1, 32 * 1024 * 1024),
    maxTotalAttachmentBytes: integer('PRISM_MAX_TOTAL_ATTACHMENT_BYTES', 20 * 1024 * 1024, 1, 64 * 1024 * 1024),
    maxTranscriptChars: integer('PRISM_MAX_TRANSCRIPT_CHARS', 32000, 1000, 1000000),
    requestTimeout: integer('PRISM_REQUEST_TIMEOUT', 1800, 30, 3600) * 1000,
    // 0 turns the estimated cache-read share off (usage then reports no cached tokens).
    promptCache: new PromptCache({ ttlMs: integer('PRISM_PROMPT_CACHE_TTL_SECONDS', 600, 0, 3600) * 1000 }),
    projectIsolation });
  const port = integer('PRISM_PORT', 8319, 1, 65535);
  await new Promise(resolve => server.listen(port, process.env.PRISM_HOST || '0.0.0.0', resolve));
  console.log(JSON.stringify({ event: 'listening', port }));
  let closing = false;
  const shutdown = async () => {
    if (closing) return;
    closing = true;
    const deadline = setTimeout(() => process.exit(1), 10000);
    const stopped = new Promise(resolve => server.close(resolve));
    await manager.close();
    server.closeAllConnections();
    await stopped;
    clearTimeout(deadline);
  };
  process.on('SIGTERM', shutdown);
  process.on('SIGINT', shutdown);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch(() => { console.error(JSON.stringify({ event: 'startup_failed' })); process.exitCode = 1; });
}
