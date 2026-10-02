import { createServer } from 'node:http';
import { pathToFileURL } from 'node:url';
import { keyMatches } from './accounts.mjs';
import { AccountPoolManager } from './pool.mjs';
import { createHash } from 'node:crypto';
import { PrismError, aborted, publicError } from './errors.mjs';
import { createStreamWriter, parseRequest, resultBody } from './protocol.mjs';

function bearer(req) {
  const value = req.headers.authorization;
  return typeof value === 'string' && /^Bearer [^\s]+$/.test(value) ? value.slice(7) : '';
}

function send(res, status, value) {
  if (res.destroyed || res.writableEnded) return;
  const body = JSON.stringify(value);
  res.writeHead(status, { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body),
    'Cache-Control': 'no-store', 'X-Content-Type-Options': 'nosniff' });
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

export function createPrismServer({ manager, managementKey, bodyLimit = 8 * 1024 * 1024, requestTimeout = 1800000,
  sessionTimeout = 30000, keepaliveMs = 10000, maxTextBytes, maxTranscriptChars }) {
  if (typeof managementKey !== 'string' || managementKey.length < 32) throw new Error('PRISM_MANAGEMENT_KEY must have at least 32 characters');
  const managementHash = createHash('sha256').update(managementKey).digest('hex');
  // Codex requests (tool schemas plus history) are large; management bodies are not.
  const managementLimit = Math.min(bodyLimit, 128 * 1024);
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
          id, object: 'model', created: 0, owned_by: 'prism' })) });
      }
      if (!['responses', 'chat/completions'].includes(action) || req.method !== 'POST') {
        throw new PrismError('method_not_allowed', 405);
      }
      const status = manager.status(source);
      if (!status.ready) throw new PrismError('account_not_ready', 503);
      const request = parseRequest(await readJSON(req, bodyLimit), action === 'responses' ? 'responses' : 'chat',
        status.models, { ...(maxTextBytes ? { maxTextBytes } : {}), ...(maxTranscriptChars ? { maxTranscriptChars } : {}) });
      if (request.stream) {
        res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache, no-store',
          Connection: 'keep-alive', 'X-Accel-Buffering': 'no', 'X-Prism-Usage': 'estimated' });
        res.flushHeaders();
        streamWriter = createStreamWriter(res, request, { signal: controller.signal, onDisconnect: disconnect });
        await streamWriter.begin();
        keepalive = setInterval(() => {
          streamWriter.heartbeat().catch(disconnect);
        }, keepaliveMs);
      }
      aborted(controller.signal);
      const text = await manager.generate(source, request, controller.signal);
      aborted(controller.signal);
      const result = resultBody(request, text, streamWriter?.identity);
      if (res.destroyed) return;
      if (request.stream) await streamWriter.finish(result);
      else { res.setHeader('X-Prism-Usage', 'estimated'); send(res, 200, result); }
    } catch (error) {
      if (res.destroyed || res.writableEnded) return;
      const output = publicError(error);
      if (streamWriter) await streamWriter.error(error).catch(disconnect);
      else send(res, error instanceof PrismError ? error.status : 502, output);
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
  const manager = new AccountPoolManager({ dataDir: process.env.PRISM_DATA_DIR || '/data',
    queueLimit: integer('PRISM_QUEUE_LIMIT', 8, 1, 64), maxAccounts: integer('PRISM_MAX_ACCOUNTS', 16, 1, 256),
    concurrency: integer('PRISM_ACCOUNT_CONCURRENCY', 2, 1, 4), maxWorkers: integer('PRISM_MAX_WORKERS', 32, 1, 1024),
    startLimit: integer('PRISM_ACCOUNT_START_LIMIT', 0, 0, 120),
    startWindowMs: integer('PRISM_START_WINDOW_SECONDS', 65, 1, 3600) * 1000 });
  await manager.init();
  const server = createPrismServer({ manager, managementKey: process.env.PRISM_MANAGEMENT_KEY,
    bodyLimit: integer('PRISM_BODY_LIMIT', 8 * 1024 * 1024, 4096, 32 * 1024 * 1024),
    maxTranscriptChars: integer('PRISM_MAX_TRANSCRIPT_CHARS', 32000, 1000, 1000000),
    requestTimeout: integer('PRISM_REQUEST_TIMEOUT', 1800, 30, 3600) * 1000 });
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
