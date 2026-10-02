import test from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { NativeAttachmentUpload, composerAttachmentState, nativeAttachmentInput,
  validateNativeReferences, validateResolvedAttachments } from '../src/browser-attachments.mjs';
import { BrowserSession } from '../src/browser.mjs';

const projectId = '01234567-89ab-4cde-8123-0123456789ab';
const fixture = (filename = 'sample-aabbcc.png', data = Buffer.from('image bytes')) => ({
  filename, mimeType: 'image/png', data, sha256: createHash('sha256').update(data).digest('hex'),
});
const reference = filename => ({ type: 'input_file', filename, project_path: '/prism-uploads/' + filename });
const errorCode = code => error => error.code === code;

test('resolved buffers are verified and duplicate bytes reuse one native upload', () => {
  const item = fixture();
  assert.equal(validateResolvedAttachments([item, item]).length, 1);
  assert.equal(validateResolvedAttachments([fixture('_report..v2-aabbcc.png')]).length, 1);
  assert.throws(() => validateResolvedAttachments([{ ...item, sha256: 'bad' }]), errorCode('browser_attachment_digest_mismatch'));
  assert.throws(() => validateResolvedAttachments([item, fixture(item.filename, Buffer.from('different'))]),
    errorCode('browser_attachment_filename_collision'));
  assert.throws(() => validateResolvedAttachments([{ ...item, data: undefined, url: 'https://example.com/a.png' }]),
    errorCode('browser_attachment_shape_invalid'));
  assert.throws(() => validateResolvedAttachments([fixture('../outside.png')]), errorCode('browser_attachment_shape_invalid'));
});

test('composer references become ready only after native registration finishes', () => {
  const previous = globalThis.document;
  const item = fixture();
  const prompt = { prompt: { content: [{ type: 'input_text', text: 'UI text' }, reference(item.filename)] }, pendingUploads: ['upload'] };
  const textarea = { getClientRects: () => [1], __reactFiber$test: { return: { memoizedProps: prompt } } };
  globalThis.document = { querySelectorAll: () => [textarea] };
  try {
    assert.equal(composerAttachmentState({ expected: [item.filename] }), false);
    prompt.pendingUploads = [];
    assert.deepEqual(composerAttachmentState({ expected: [item.filename] }), [{
      ...reference(item.filename), file_name: undefined,
    }]);
    assert.equal(composerAttachmentState({ expected: ['missing.png'] }), false);
  } finally {
    if (previous === undefined) delete globalThis.document;
    else globalThis.document = previous;
  }
});

test('only expected native project upload references are permitted', () => {
  const item = fixture();
  const valid = reference(item.filename);
  assert.deepEqual(validateNativeReferences([valid], [item]), [valid]);
  assert.throws(() => validateNativeReferences([], [item]), errorCode('browser_attachment_reference_missing'));
  for (const bad of [reference('other.png'), { ...valid, project_path: '/private/sample-aabbcc.png' },
    { ...valid, project_path: '/prism-uploads/../' + item.filename }, { ...valid, type: 'input_image' },
    { ...valid, file_name: 'other.png' }]) {
    assert.throws(() => validateNativeReferences([bad], [item]), errorCode('browser_attachment_reference_invalid'));
  }
  assert.throws(() => validateNativeReferences([valid, valid], [item]), errorCode('browser_attachment_reference_duplicate'));
});

test('native references map flattened prompt markers without native history or retry mutations', () => {
  const item = fixture();
  const refs = [reference(item.filename)];
  const input = [{ role: 'user', content: [{ type: 'input_text', text: 'Read [Attachment 1] and [Attachment 2].' }] }];
  const before = structuredClone(input);
  const body = { input: [
    { role: 'system', content: [{ type: 'input_text', text: 'UNRELATED NATIVE SYSTEM' }] },
    { role: 'user', content: [{ type: 'input_text', text: 'UNRELATED UI TEXT' }, ...refs] },
  ] };
  const combined = nativeAttachmentInput(input, body, [item, item], refs);
  assert.deepEqual(input, before);
  assert.equal(combined.length, 1);
  assert.deepEqual(combined[0].content.at(-1), refs[0]);
  const text = JSON.stringify(combined);
  assert.ok(!text.includes('UNRELATED'));
  assert.match(text, /native read-only tools/);
  assert.match(text, /\[Attachment 1\]: \/prism-uploads\/sample-aabbcc.png/);
  assert.match(text, /\[Attachment 2\]: \/prism-uploads\/sample-aabbcc.png/);
  assert.throws(() => nativeAttachmentInput(input, body, [item], [{ ...refs[0], project_path: '/prism-uploads_2/' + item.filename }]),
    errorCode('browser_attachment_reference_changed'));
  assert.throws(() => nativeAttachmentInput(input, { input: [{ role: 'system', content: refs }] }, [item], refs),
    errorCode('browser_attachment_reference_invalid'));
});

function fakePage(items, { existing = false, reuse = false, wrongReference = false, cancel } = {}) {
  const calls = [];
  const attached = [];
  const uploadPayload = payload => {
    calls.push(['payload', payload]);
    attached.push(reference(wrongReference ? 'unrelated.png' : payload.name));
    cancel?.();
  };
  const button = name => ({
    last() { return this; },
    async click(options) { calls.push(['click', name, options]); },
    async waitFor() { if (!reuse) throw new Error('not visible'); },
  });
  const menu = { filter() { return this; }, first() { return this; }, async click(options) { calls.push(['choose', options]); } };
  return {
    calls,
    isClosed: () => false,
    locator: () => ({ count: async () => Number(existing), last() { return this; }, setInputFiles: async payload => uploadPayload(payload) }),
    getByRole: (role, options) => role === 'menuitem' ? menu : button(options.name),
    async waitForEvent(event, options) {
      calls.push(['event', event, options]);
      return { setFiles: async payload => uploadPayload(payload) };
    },
    async waitForFunction(fn, argument, options) {
      calls.push(['ready', argument, options]);
      return { jsonValue: async () => attached.slice(), dispose: async () => {} };
    },
  };
}

for (const existing of [false, true]) test(`native upload uses ${existing ? 'file input' : 'file chooser'} payload buffers and waits for references`, async () => {
  const items = [fixture(), fixture('document-ddeeff.pdf')];
  const page = fakePage(items, { existing, reuse: true });
  const uploader = new NativeAttachmentUpload(page, projectId, items);
  const prepared = await uploader.prepare();
  assert.deepEqual(prepared, items.map(item => reference(item.filename)));
  const payloads = page.calls.filter(([name]) => name === 'payload').map(([, payload]) => payload);
  assert.deepEqual(payloads, items.map(item => ({ name: item.filename, mimeType: item.mimeType, buffer: item.data })));
  const waits = page.calls.filter(([name, argument]) => name === 'ready' && argument);
  assert.deepEqual(waits.map(([, argument]) => argument.expected), [[items[0].filename], items.map(item => item.filename)]);
  assert.equal(uploader.active, null);
});

test('native upload scope rejects unrelated project headers and late uploads', () => {
  const item = fixture();
  const uploader = new NativeAttachmentUpload({}, projectId, [item]);
  const request = (project = projectId, filename = item.filename) => ({ method: () => 'POST',
    headers: () => ({ 'x-prism-project-id': project, 'x-prism-file-name': encodeURIComponent(filename) }) });
  assert.equal(uploader.allowsUpload(request()), false);
  uploader.active = item;
  assert.equal(uploader.allowsUpload(request()), true);
  assert.equal(uploader.allowsUpload(request('another-project')), false);
  assert.equal(uploader.allowsUpload(request(projectId, 'another.png')), false);
  assert.equal(uploader.allowsUpload({ method: () => 'DELETE', headers: () => ({}) }), false);
  uploader.active = null;
  assert.equal(uploader.allowsUpload(request()), false);
});

test('browser routes allow native uploads only for the active managed-project attachment', async () => {
  const item = fixture();
  const uploader = new NativeAttachmentUpload({}, projectId, [item]);
  const driver = new BrowserSession({}, () => {});
  driver.projectId = projectId;
  driver.turn = { attachments: uploader };
  const upload = async (project = projectId) => {
    const state = { aborted: false, continued: false };
    await driver.route({ request: () => ({ url: () => 'https://prism.openai.com/api/project-files/upload',
      method: () => 'POST', headers: () => ({ 'x-prism-project-id': project, 'x-prism-file-name': item.filename }) }),
    abort: async () => { state.aborted = true; }, continue: async () => { state.continued = true; } });
    return state;
  };
  assert.deepEqual(await upload(), { aborted: true, continued: false });
  uploader.active = item;
  assert.deepEqual(await upload('unrelated-project'), { aborted: true, continued: false });
  assert.deepEqual(await upload(), { aborted: false, continued: true });
  driver.turn = null;
  assert.deepEqual(await upload(), { aborted: true, continued: false });
});

test('abort after uploading stops readiness waits and releases upload scope', async () => {
  const item = fixture();
  const controller = new AbortController();
  const page = fakePage([item], { cancel: () => controller.abort() });
  const uploader = new NativeAttachmentUpload(page, projectId, [item], controller.signal);
  await assert.rejects(uploader.prepare(), errorCode('request_cancelled'));
  assert.equal(uploader.active, null);
  assert.ok(!page.calls.some(([name]) => name === 'ready'));
});

test('browser uploads before Enter and merges only validated file references', async () => {
  const item = fixture();
  const page = fakePage([item]);
  const driver = new BrowserSession({}, () => {});
  driver.projectId = projectId;
  driver.page = page;
  driver.labels.set('gpt-5.6-sol', '5.6 Sol');
  const request = { model: 'gpt-5.6-sol', effort: 'low', attachments: [item],
    input: [{ role: 'user', content: [{ type: 'input_text', text: 'Read [Attachment 1].' }] }] };
  let sent;
  driver.composer = async () => ({
    async fill() { page.calls.push(['fill']); },
    async press() {
      assert.deepEqual(driver.turn.attachments.references, [reference(item.filename)]);
      assert.equal(driver.turn.submitAllowed, true);
      page.calls.push(['press']);
      const body = { conversationId: 'current-conversation', metadata: { projectId }, input: [
        { role: 'system', content: [{ type: 'input_text', text: 'UI SYSTEM' }] },
        { role: 'user', content: [{ type: 'input_text', text: 'UI PLACEHOLDER' }, reference(item.filename)] },
      ] };
      await driver.route({ request: () => ({ url: () => 'https://prism.openai.com/api/llm/response_with_tools_start',
        method: () => 'POST', postDataJSON: () => body }), abort: async () => assert.fail('start aborted'),
      continue: async options => { sent = JSON.parse(options.postData); } });
      driver.turn.completed = true;
      driver.turn.resolve('ATTACHMENT RESULT');
    },
  });
  assert.equal(await driver.generate(request), 'ATTACHMENT RESULT');
  assert.ok(page.calls.findIndex(([name]) => name === 'fill') < page.calls.findIndex(([name]) => name === 'payload'));
  assert.ok(page.calls.findIndex(([name]) => name === 'ready') < page.calls.findIndex(([name]) => name === 'press'));
  assert.match(JSON.stringify(sent.input), /Read \[Attachment 1\]/);
  assert.ok(!JSON.stringify(sent.input).includes('UI SYSTEM'));
  assert.ok(!JSON.stringify(sent.input).includes('UI PLACEHOLDER'));
  assert.equal(request.input[0].content.length, 1);
});

test('failed attachment registration closes the page context before another turn', async () => {
  const item = fixture();
  const page = fakePage([item], { wrongReference: true });
  const driver = new BrowserSession({}, () => {});
  driver.projectId = projectId;
  driver.page = page;
  driver.labels.set('gpt-5.6-sol', '5.6 Sol');
  let closed = false;
  driver.context = { close: async () => { closed = true; } };
  driver.composer = async () => ({ fill: async () => {}, press: async () => assert.fail('submitted invalid attachment') });
  await assert.rejects(driver.generate({ model: 'gpt-5.6-sol', effort: 'low', attachments: [item], input: [] }),
    errorCode('browser_attachment_reference_invalid'));
  assert.equal(closed, true);
  assert.equal(driver.page, null);
  assert.equal(driver.turn, null);
});
