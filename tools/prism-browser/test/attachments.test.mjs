import test from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { attachmentLimits, isPublicAddress, normalizeAttachment, resolveAttachments } from '../src/attachments.mjs';
import { PrismError } from '../src/errors.mjs';

const png = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+j4e8AAAAASUVORK5CYII=', 'base64');
const dataURL = (data, mime) => `data:${mime};base64,${data.toString('base64')}`;
const image = value => ({ type: 'input_image', image_url: value });
const file = (filename, file_data) => ({ type: 'input_file', filename, file_data });
const normalize = (part, limits) => normalizeAttachment(part, 'input.0.content.0', limits);
const errorCode = code => error => error instanceof PrismError && error.code === code && error.status === 400;

function fakeNetwork(replies, addresses = {}, { hang = false } = {}) {
  const visited = [];
  const lookup = (hostname, options, callback) => queueMicrotask(() => callback(null,
    (addresses[hostname] ?? ['93.184.216.34']).map(address => ({ address, family: address.includes(':') ? 6 : 4 }))));
  const request = (url, options, callback) => {
    const req = new EventEmitter();
    req.destroy = () => { req.destroyed = true; };
    req.end = () => options.lookup(url.hostname, { all: true }, (error, values) => {
      if (error) { req.emit('error', error); return; }
      visited.push({ url: url.href, addresses: values, headers: options.headers, agent: options.agent });
      if (hang || req.destroyed) return;
      const reply = replies[url.href];
      const res = new EventEmitter();
      res.statusCode = reply?.status ?? 200;
      res.headers = reply?.headers ?? {};
      res.destroy = () => { res.destroyed = true; };
      callback(res);
      queueMicrotask(() => {
        for (const chunk of reply?.chunks ?? [reply?.data ?? Buffer.alloc(0)]) if (!res.destroyed) res.emit('data', chunk);
        if (!res.destroyed) res.emit('end');
      });
    });
    return req;
  };
  return { lookup, request, visited };
}

test('inline image and file encodings preserve bytes and derive safe stable filenames', () => {
  const responseImage = normalize(image(dataURL(png, 'image/png')));
  const chatImage = normalize({ type: 'image_url', image_url: { url: dataURL(png, 'image/png'), detail: 'high' } });
  assert.deepEqual(responseImage.data, png);
  assert.equal(responseImage.mimeType, 'image/png');
  assert.equal(responseImage.filename, chatImage.filename);
  assert.match(responseImage.sha256, /^[a-f0-9]{64}$/);
  assert.match(responseImage.filename, /^attachment-[a-f0-9]{16}\.png$/);
  const pdf = Buffer.from('%PDF-1.7\nexample');
  const report = normalize(file('../../reports/quarterly.pdf', dataURL(pdf, 'application/pdf')));
  assert.match(report.filename, /^quarterly-[a-f0-9]{16}\.pdf$/);
  assert.equal(report.originalFilename, 'quarterly.pdf');
  const code = normalize({ type: 'file', file: { filename: 'C:\\app\\main.py', file_data: Buffer.from('print("hello")\n').toString('base64') } });
  assert.match(code.filename, /^main-[a-f0-9]{16}\.py$/);
  assert.equal(code.mimeType, 'text/plain');
  const later = normalize(file('main.py', Buffer.from('print("different")\n').toString('base64')));
  assert.notEqual(code.filename, later.filename);
  assert.equal(normalize(file('notes.md', dataURL(Buffer.from('# Notes\n'), 'text/markdown'))).mimeType, 'text/markdown');
});

test('malformed data, unsupported file identifiers and mismatched media fail explicitly', () => {
  for (const value of ['AAAA%', 'AAA', 'AB==', 'AAAA\n', '', 'data:image/png,AAAA', 'data:image/png;base64,@@@=']) {
    assert.throws(() => normalize(image(value.startsWith('data:') ? value : `data:image/png;base64,${value}`)), errorCode('invalid_attachment_data'), value);
  }
  for (const part of [{ type: 'input_image', file_id: 'file_1' }, { type: 'input_file', file_id: 'file_1' },
    { type: 'file', file: { file_id: 'file_1' } }]) assert.throws(() => normalize(part), errorCode('attachment_file_id_not_supported'));
  assert.throws(() => normalize(file('a.pdf', dataURL(png, 'image/png'))), errorCode('invalid_attachment_data'));
  assert.throws(() => normalize(image(dataURL(Buffer.from('text'), 'text/plain'))), errorCode('attachment_type_not_supported'));
  assert.throws(() => normalize(file('data.txt', Buffer.from([255, 255]).toString('base64'))), errorCode('invalid_attachment_data'));
  assert.throws(() => normalize(file('data.txt', Buffer.from('a\0b').toString('base64'))), errorCode('invalid_attachment_data'));
  for (const [filename, mime] of [['a.svg', 'image/svg+xml'], ['index.html', 'text/html'], ['a.exe', 'application/x-msdownload'], ['a.wasm', 'application/wasm']]) {
    assert.throws(() => normalize(file(filename, dataURL(Buffer.from('test'), mime))), errorCode('attachment_type_not_supported'));
  }
  assert.match(normalize(file('index.html', dataURL(Buffer.from('<p>Hello</p>'), 'text/plain'))).filename, /^index-[a-f0-9]{16}\.txt$/);
  assert.throws(() => normalize({ type: 'input_file', filename: 'a.txt', file_data: 'dGVzdA==', file_url: 'https://example.com/a.txt' }),
    errorCode('invalid_attachment_data'));
});

test('decoded byte limits are enforced before allocating oversized inline data', async () => {
  const limits = attachmentLimits({ maxAttachmentBytes: 4 });
  assert.equal(normalize(file('a.txt', Buffer.from('1234').toString('base64')), limits).data.length, 4);
  assert.throws(() => normalize(file('a.txt', Buffer.from('12345').toString('base64')), limits), errorCode('attachment_too_large'));
  const value = normalize(file('a.txt', Buffer.from('1234').toString('base64')));
  await assert.rejects(resolveAttachments([value, value], { limits: { maxTotalAttachmentBytes: 7 } }), errorCode('attachments_too_large'));
});

test('URL normalization is synchronous and rejects credentials, local targets and unsafe schemes', () => {
  const value = normalize(image('https://cdn.example.com/image.png?token=test'));
  assert.equal(value.url, 'https://cdn.example.com/image.png?token=test');
  assert.equal(value.data, undefined);
  for (const url of ['file:///tmp/a.png', 'https://user:pass@example.com/a.png', 'https://example.com/a.png#secret']) {
    assert.throws(() => normalize(image(url)), errorCode('invalid_attachment_url'));
  }
  for (const url of ['http://127.0.0.1/a.png', 'http://localhost/a.png', 'http://169.254.169.254/a.png', 'http://[::1]/a.png']) {
    assert.throws(() => normalize(image(url)), errorCode('attachment_url_not_allowed'));
  }
});

test('public address validation excludes private, mapped, reserved and documentation networks', () => {
  for (const address of ['0.0.0.0', '10.0.0.1', '100.64.0.1', '127.0.0.1', '169.254.169.254', '172.16.1.1',
    '192.168.1.1', '192.0.0.1', '192.0.2.5', '198.18.0.1', '198.51.100.1', '203.0.113.1', '224.0.0.1', '255.255.255.255',
    '::', '::1', 'fc00::1', 'fe80::1', 'ff02::1', '::ffff:127.0.0.1', '::ffff:7f00:1', '2001:db8::1', '2001::1', '2002:7f00::1', '3fff::1']) {
    assert.equal(isPublicAddress(address), false, address);
  }
  for (const address of ['1.1.1.1', '8.8.8.8', '93.184.216.34', '2606:4700:4700::1111', '2001:4860:4860::8888']) {
    assert.equal(isPublicAddress(address), true, address);
  }
});

test('remote image resolution follows validated redirects, pins DNS and identifies final bytes', async () => {
  const attachment = { ...normalize(image('https://example.com/a.png')), id: 'attachment_1', marker: '[Attachment 1]' };
  const network = fakeNetwork({
    'https://example.com/a.png': { status: 302, headers: { location: 'https://cdn.example.com/image.png' } },
    'https://cdn.example.com/image.png': { headers: { 'content-type': 'image/png' }, data: png },
  });
  const [resolved] = await resolveAttachments([attachment], network);
  assert.deepEqual(resolved.data, png);
  assert.match(resolved.filename, /^a-[a-f0-9]{16}\.png$/);
  assert.equal(resolved.url, undefined);
  assert.equal(resolved.marker, '[Attachment 1]');
  assert.equal(attachment.url, 'https://example.com/a.png');
  assert.equal(network.visited.length, 2);
  assert.equal(network.visited[0].agent, false);
  assert.deepEqual(network.visited[0].addresses, [{ address: '93.184.216.34', family: 4 }]);
  assert.equal(network.visited[0].headers['Accept-Encoding'], 'identity');
});

test('signed URL paths without a recognized extension use the downloaded media type', async () => {
  const url = 'https://example.com/download.bin?signature=abcdef';
  const [attachment] = await resolveAttachments([normalize(image(url))], fakeNetwork({
    [url]: { headers: { 'content-type': 'image/png' }, data: png },
  }));
  assert.equal(attachment.mimeType, 'image/png');
  assert.match(attachment.filename, /^download-[a-f0-9]{16}\.png$/);
});

test('every remote redirect and DNS answer is checked against SSRF targets', async () => {
  const value = normalize(image('https://example.com/a.png'));
  const privateDNS = fakeNetwork({}, { 'example.com': ['127.0.0.1'] });
  await assert.rejects(resolveAttachments([value], privateDNS), errorCode('attachment_url_not_allowed'));
  assert.equal(privateDNS.visited.length, 0);
  const mixedDNS = fakeNetwork({}, { 'example.com': ['93.184.216.34', '10.0.0.1'] });
  await assert.rejects(resolveAttachments([value], mixedDNS), errorCode('attachment_url_not_allowed'));
  const localRedirect = fakeNetwork({ 'https://example.com/a.png': { status: 302, headers: { location: 'http://169.254.169.254/latest' } } });
  await assert.rejects(resolveAttachments([value], localRedirect), errorCode('attachment_url_not_allowed'));
  assert.equal(localRedirect.visited.length, 1);
  const privateRedirect = fakeNetwork({ 'https://example.com/a.png': { status: 302, headers: { location: 'https://internal.example.com/a.png' } } },
    { 'internal.example.com': ['192.168.1.1'] });
  await assert.rejects(resolveAttachments([value], privateRedirect), errorCode('attachment_url_not_allowed'));
  assert.equal(privateRedirect.visited.length, 1);
});

test('remote limits reject content lengths, chunk overflow, unexpected bodies and redirect loops', async () => {
  const url = 'https://example.com/a.png';
  const value = normalize(image(url));
  await assert.rejects(resolveAttachments([value], { ...fakeNetwork({ [url]: { headers: { 'content-length': '1000' }, data: png } }),
    limits: { maxAttachmentBytes: 100 } }), errorCode('attachment_too_large'));
  await assert.rejects(resolveAttachments([value], { ...fakeNetwork({ [url]: { chunks: [png, png] } }),
    limits: { maxAttachmentBytes: png.length } }), errorCode('attachment_too_large'));
  await assert.rejects(resolveAttachments([value], fakeNetwork({ [url]: { headers: { 'content-type': 'text/html' }, data: Buffer.from('<html>') } })),
    errorCode('attachment_type_not_supported'));
  await assert.rejects(resolveAttachments([value], fakeNetwork({ [url]: { headers: { 'content-encoding': 'gzip' }, data: png } })),
    errorCode('attachment_download_failed'));
  const loop = fakeNetwork({ [url]: { status: 301, headers: { location: '/a.png' } } });
  await assert.rejects(resolveAttachments([value], loop), errorCode('attachment_download_failed'));
  assert.equal(loop.visited.length, 4);
});

test('remote downloads observe cancellation and an overall timeout', async () => {
  const value = normalize(image('https://example.com/a.png'));
  const controller = new AbortController();
  const network = fakeNetwork({}, {}, { hang: true });
  const work = resolveAttachments([value], { ...network, signal: controller.signal });
  controller.abort(new PrismError('request_timeout', 504));
  await assert.rejects(work, error => error.code === 'request_timeout' && error.status === 504);
  await assert.rejects(resolveAttachments([value], { ...fakeNetwork({}, {}, { hang: true }), timeoutMs: 5 }),
    errorCode('attachment_download_timeout'));
});
