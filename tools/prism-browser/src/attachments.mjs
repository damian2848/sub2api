import { createHash } from 'node:crypto';
import { lookup as dnsLookup } from 'node:dns';
import http from 'node:http';
import https from 'node:https';
import { isIP } from 'node:net';
import { PrismError, aborted } from './errors.mjs';

const MIB = 1024 * 1024;
const TYPES = new Map([
  ['image/png', 'png'], ['image/jpeg', 'jpg'], ['image/webp', 'webp'], ['image/gif', 'gif'],
  ['application/pdf', 'pdf'], ['text/plain', 'txt'], ['text/markdown', 'md'], ['text/csv', 'csv'],
  ['application/json', 'json'], ['application/yaml', 'yaml'], ['text/yaml', 'yaml'],
]);
const EXTENSIONS = new Map([...TYPES].map(([mime, extension]) => [extension, mime]));
for (const [extension, mime] of [['jpeg', 'image/jpeg'], ['log', 'text/plain'], ['yml', 'application/yaml']]) {
  EXTENSIONS.set(extension, mime);
}
for (const extension of ['js', 'mjs', 'cjs', 'ts', 'tsx', 'jsx', 'vue', 'svelte', 'css', 'scss', 'less', 'py', 'go',
  'rs', 'rb', 'java', 'c', 'cc', 'cpp', 'h', 'hpp', 'cs', 'swift', 'kt', 'kts', 'php', 'sh', 'sql', 'toml', 'ini',
  'conf', 'config', 'xml', 'tex', 'r', 'ipynb', 'diff', 'patch']) EXTENSIONS.set(extension, 'text/plain');
const TEXT_TYPES = new Set([...TYPES.keys()].filter(mime => !mime.startsWith('image/') && mime !== 'application/pdf'));
const fail = (code, param) => { throw new PrismError(code, 400, param); };
const envInt = (name, fallback) => {
  const value = Number(process.env[name]);
  return Number.isSafeInteger(value) && value > 0 ? value : fallback;
};

export function attachmentLimits(overrides = {}) {
  return {
    maxAttachments: overrides.maxAttachments ?? envInt('PRISM_MAX_ATTACHMENTS', 8),
    maxAttachmentBytes: overrides.maxAttachmentBytes ?? envInt('PRISM_MAX_ATTACHMENT_BYTES', 10 * MIB),
    maxTotalAttachmentBytes: overrides.maxTotalAttachmentBytes ?? envInt('PRISM_MAX_TOTAL_ATTACHMENT_BYTES', 20 * MIB),
  };
}

function cleanFilename(value, param) {
  if (value !== undefined && (typeof value !== 'string' || !value.trim())) fail('invalid_attachment_filename', param);
  const name = (value ?? 'attachment').replace(/\\/g, '/').split('/').pop()
    .replace(/[^a-zA-Z0-9._-]+/g, '_').replace(/^\.+/, '').slice(0, 120) || 'attachment';
  return name;
}

function filenameType(filename) {
  return EXTENSIONS.get(filename.split('.').pop().toLowerCase());
}

function mimeType(value) {
  const mime = typeof value === 'string' ? value.split(';', 1)[0].trim().toLowerCase() : '';
  if (mime === 'image/jpg') return 'image/jpeg';
  if (['application/javascript', 'application/typescript', 'text/javascript', 'text/x-python', 'text/x-shellscript',
    'application/x-yaml', 'text/x-yaml', 'text/x-markdown', 'application/xml', 'text/xml', 'text/css'].includes(mime)) return 'text/plain';
  return mime;
}

function checkedData(data, mime, filename, kind, param, limits) {
  if (!data.length) fail('invalid_attachment_data', param);
  if (data.length > limits.maxAttachmentBytes) fail('attachment_too_large', param);
  const detected = data.subarray(0, 8).equals(Buffer.from([137, 80, 78, 71, 13, 10, 26, 10])) ? 'image/png' :
    data[0] === 0xff && data[1] === 0xd8 && data[2] === 0xff ? 'image/jpeg' :
      /^(GIF87a|GIF89a)$/.test(data.subarray(0, 6).toString('ascii')) ? 'image/gif' :
        data.subarray(0, 4).toString('ascii') === 'RIFF' && data.subarray(8, 12).toString('ascii') === 'WEBP' ? 'image/webp' :
          data.subarray(0, 5).toString('ascii') === '%PDF-' ? 'application/pdf' : '';
  const supplied = mimeType(mime);
  const declared = supplied === 'application/octet-stream' || !supplied ? filenameType(filename) ??
    (detected || (kind === 'file' ? 'text/plain' : '')) : supplied;
  if (!TYPES.has(declared) || (kind === 'image' && !declared.startsWith('image/'))) fail('attachment_type_not_supported', param);
  if (TEXT_TYPES.has(declared)) {
    try { new TextDecoder('utf-8', { fatal: true }).decode(data); } catch { fail('invalid_attachment_data', param); }
    if (data.includes(0) || detected) fail('invalid_attachment_data', param);
  } else if (declared !== detected) fail('invalid_attachment_data', param);
  const extensionMime = filenameType(filename);
  if (extensionMime && extensionMime !== declared && !(TEXT_TYPES.has(extensionMime) && TEXT_TYPES.has(declared))) {
    fail('invalid_attachment_data', param);
  }
  const sha256 = createHash('sha256').update(data).digest('hex');
  const stem = filename.replace(/\.[^.]+$/, '').slice(0, 80) || 'attachment';
  const extension = TEXT_TYPES.has(declared) && extensionMime ? filename.split('.').pop().toLowerCase() : TYPES.get(declared);
  return { data, mimeType: declared, sha256, filename: `${stem}-${sha256.slice(0, 16)}.${extension}` };
}

function decodeBase64(value, param, limits) {
  if (typeof value !== 'string' || !value.length || value.length % 4 !== 0 || !/^[A-Za-z0-9+/]*={0,2}$/.test(value)) {
    fail('invalid_attachment_data', param);
  }
  if (value.length / 4 * 3 - (value.endsWith('==') ? 2 : value.endsWith('=') ? 1 : 0) > limits.maxAttachmentBytes) {
    fail('attachment_too_large', param);
  }
  const data = Buffer.from(value, 'base64');
  if (data.toString('base64') !== value) fail('invalid_attachment_data', param);
  return data;
}

function decodeData(value, param, limits) {
  if (!value.startsWith('data:')) return { data: decodeBase64(value, param, limits), mime: '' };
  const match = /^data:([^;,]+);base64,([\s\S]*)$/.exec(value);
  if (!match) fail('invalid_attachment_data', param);
  return { data: decodeBase64(match[2], param, limits), mime: match[1] };
}

function checkedURL(value, param) {
  let url;
  try { url = new URL(value); } catch { fail('invalid_attachment_url', param); }
  if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || !url.hostname || url.hash) {
    fail('invalid_attachment_url', param);
  }
  const hostname = url.hostname.replace(/^\[|\]$/g, '');
  if (isIP(hostname) && !isPublicAddress(hostname)) fail('attachment_url_not_allowed', param);
  if (hostname === 'localhost' || hostname.endsWith('.localhost') || hostname.endsWith('.local')) {
    fail('attachment_url_not_allowed', param);
  }
  return url;
}

// Only publicly routed addresses can be downloaded. The request's DNS callback pins the
// validated address, so a second lookup cannot redirect a download into a private network.
export function isPublicAddress(address) {
  const family = isIP(address);
  if (family === 4) {
    const [a, b, c] = address.split('.').map(Number);
    return !(a === 0 || a === 10 || a === 127 || a >= 224 || (a === 100 && b >= 64 && b <= 127) ||
      (a === 169 && b === 254) || (a === 172 && b >= 16 && b <= 31) ||
      (a === 192 && (b === 168 || (b === 0 && (c === 0 || c === 2)) || (b === 88 && c === 99))) ||
      (a === 198 && (b === 18 || b === 19 || (b === 51 && c === 100))) || (a === 203 && b === 0 && c === 113));
  }
  if (family !== 6 || address.includes('.')) return false;
  const halves = address.toLowerCase().split('::');
  const left = halves[0] ? halves[0].split(':') : [];
  const right = halves[1] ? halves[1].split(':') : [];
  const pieces = halves.length === 1 ? left : [...left, ...Array(8 - left.length - right.length).fill('0'), ...right];
  const [a, b] = pieces.map(value => parseInt(value, 16));
  return a >= 0x2000 && a <= 0x3fff && !(a === 0x2001 && (b <= 0x01ff || b === 0x0db8)) &&
    a !== 0x2002 && !(a === 0x3fff && b <= 0x0fff);
}

export function normalizeAttachment(part, param, limits = attachmentLimits()) {
  let kind, source, filename, dataValue, urlValue;
  if (part.type === 'input_image' || part.type === 'image_url') {
    kind = 'image';
    source = part.type === 'input_image' ? part.image_url : part.image_url?.url;
    if (part.file_id !== undefined) fail('attachment_file_id_not_supported', param);
    if (typeof source !== 'string') fail('invalid_attachment_data', param);
    if (source.startsWith('data:')) dataValue = source; else urlValue = source;
  } else {
    kind = 'file';
    source = part.type === 'file' ? part.file : part;
    if (!source || typeof source !== 'object' || Array.isArray(source)) fail('invalid_attachment_data', param);
    if (source.file_id !== undefined) fail('attachment_file_id_not_supported', param);
    filename = source.filename;
    dataValue = source.file_data;
    urlValue = source.file_url;
    if ((dataValue !== undefined) === (urlValue !== undefined)) fail('invalid_attachment_data', param);
    if ((dataValue !== undefined && typeof dataValue !== 'string') || (urlValue !== undefined && typeof urlValue !== 'string')) {
      fail('invalid_attachment_data', param);
    }
  }
  if (dataValue !== undefined) {
    const { data, mime } = decodeData(dataValue, param, limits);
    const originalFilename = cleanFilename(filename, param);
    return { kind, param, originalFilename, ...checkedData(data, mime, originalFilename, kind, param, limits) };
  }
  const url = checkedURL(urlValue, param);
  let urlFilename;
  try { urlFilename = decodeURIComponent(url.pathname.split('/').pop()); } catch { fail('invalid_attachment_url', param); }
  const originalFilename = cleanFilename(filename ?? (urlFilename || undefined), param);
  return { kind, param, originalFilename, filename: originalFilename, url: url.href };
}

function cancellation(signal) {
  return signal?.reason instanceof PrismError ? signal.reason : new PrismError('request_cancelled', 499);
}

function downloadOnce(url, { signal, maxBytes, deadline, lookup = dnsLookup, request }, param) {
  aborted(signal);
  return new Promise((resolve, reject) => {
    let req, response, settled = false;
    const finish = (error, value) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      signal?.removeEventListener('abort', onAbort);
      if (error) { req?.destroy(); response?.destroy(); reject(error); } else resolve(value);
    };
    const onAbort = () => finish(cancellation(signal));
    const timer = setTimeout(() => finish(new PrismError('attachment_download_timeout', 400, param)), Math.max(1, deadline - Date.now()));
    const validatedLookup = (hostname, options, callback) => {
      lookup(hostname, { all: true, verbatim: true }, (error, addresses) => {
        if (settled) return callback(new PrismError('request_cancelled', 499));
        if (error || !addresses?.length) return callback(new PrismError('attachment_download_failed', 400, param));
        if (addresses.some(value => !isPublicAddress(value.address))) {
          return callback(new PrismError('attachment_url_not_allowed', 400, param));
        }
        if (options.all) callback(null, addresses); else callback(null, addresses[0].address, addresses[0].family);
      });
    };
    signal?.addEventListener('abort', onAbort, { once: true });
    try {
      req = (request ?? (url.protocol === 'https:' ? https.request : http.request))(url, {
        method: 'GET', lookup: validatedLookup, agent: false,
        headers: { Accept: 'image/png,image/jpeg,image/webp,image/gif,application/pdf,text/plain,text/markdown,text/csv,application/json',
          'Accept-Encoding': 'identity' },
      }, res => {
        response = res;
        if (settled) { res.destroy(); return; }
        if ([301, 302, 303, 307, 308].includes(res.statusCode)) {
          const location = res.headers.location;
          res.destroy();
          return finish(null, { location });
        }
        if (res.statusCode !== 200 || (res.headers['content-encoding'] && res.headers['content-encoding'] !== 'identity')) {
          return finish(new PrismError('attachment_download_failed', 400, param));
        }
        const length = Number(res.headers['content-length']);
        if (Number.isFinite(length) && length > maxBytes) return finish(new PrismError('attachment_too_large', 400, param));
        const chunks = [];
        let size = 0;
        res.on('data', chunk => {
          size += chunk.length;
          if (size > maxBytes) return finish(new PrismError('attachment_too_large', 400, param));
          chunks.push(chunk);
        });
        res.on('end', () => finish(null, { data: Buffer.concat(chunks), mime: res.headers['content-type'] }));
        res.on('error', () => finish(new PrismError('attachment_download_failed', 400, param)));
        res.on('aborted', () => finish(new PrismError('attachment_download_failed', 400, param)));
      });
      req.on('error', error => finish(error instanceof PrismError ? error : new PrismError('attachment_download_failed', 400, param)));
      req.end();
    } catch (error) {
      finish(error instanceof PrismError ? error : new PrismError('attachment_download_failed', 400, param));
    }
    if (signal?.aborted) onAbort();
  });
}

export async function resolveAttachments(attachments = [], { signal, limits: overrides, timeoutMs = 15000, lookup, request } = {}) {
  const limits = attachmentLimits(overrides);
  if (attachments.length > limits.maxAttachments) fail('too_many_attachments', 'input');
  const resolved = [];
  let totalBytes = 0;
  for (const attachment of attachments) {
    aborted(signal);
    let value = attachment;
    if (attachment.url) {
      let url = checkedURL(attachment.url, attachment.param);
      const deadline = Date.now() + timeoutMs;
      for (let redirects = 0; ; redirects += 1) {
        const downloaded = await downloadOnce(url, { signal,
          maxBytes: Math.min(limits.maxAttachmentBytes, limits.maxTotalAttachmentBytes - totalBytes), deadline, lookup, request }, attachment.param);
        if ('location' in downloaded) {
          if (!downloaded.location || redirects >= 3) fail('attachment_download_failed', attachment.param);
          let target;
          try { target = new URL(downloaded.location, url).href; } catch { fail('invalid_attachment_url', attachment.param); }
          url = checkedURL(target, attachment.param);
          continue;
        }
        const { url: ignored, ...retained } = attachment;
        value = { ...retained, ...checkedData(downloaded.data, downloaded.mime, attachment.originalFilename,
          attachment.kind, attachment.param, limits) };
        break;
      }
    } else if (!Buffer.isBuffer(value.data) || value.data.length > limits.maxAttachmentBytes) {
      fail('attachment_too_large', attachment.param);
    }
    totalBytes += value.data.length;
    if (totalBytes > limits.maxTotalAttachmentBytes) fail('attachments_too_large', attachment.param);
    resolved.push(value);
  }
  aborted(signal);
  return resolved;
}
