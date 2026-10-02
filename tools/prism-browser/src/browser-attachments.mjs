import { createHash } from 'node:crypto';
import { PrismError, aborted } from './errors.mjs';

const filenamePattern = /^[A-Za-z0-9_-][A-Za-z0-9_.-]{0,180}$/;
const projectPathPattern = /^\/prism-uploads(?:_\d+)?\/([A-Za-z0-9_-][A-Za-z0-9_.-]{0,180})$/;
const partFilename = part => part?.filename ?? part?.file_name;

function fail(code) { throw new PrismError(code); }

export function validateResolvedAttachments(attachments) {
  if (!Array.isArray(attachments) || !attachments.length) {
    fail('browser_attachment_shape_invalid');
  }
  const unique = new Map();
  for (const item of attachments) {
    if (!item || !filenamePattern.test(item.filename || '') ||
      typeof item.mimeType !== 'string' || !/^[a-z0-9][a-z0-9!#$&^_.+-]*\/[a-z0-9][a-z0-9!#$&^_.+-]*$/i.test(item.mimeType) ||
      !Buffer.isBuffer(item.data) || !item.data.length) fail('browser_attachment_shape_invalid');
    const sha256 = createHash('sha256').update(item.data).digest('hex');
    if (item.sha256 && item.sha256 !== sha256) fail('browser_attachment_digest_mismatch');
    if (unique.has(item.filename) && unique.get(item.filename).sha256 !== sha256) {
      fail('browser_attachment_filename_collision');
    }
    if (!unique.has(item.filename)) unique.set(item.filename, { ...item, sha256 });
  }
  return [...unique.values()];
}

// Read only the active composer's prompt. Native upload registers a project file before it
// adds the reference here; pendingUploads must finish before the native Enter handler runs.
export function composerAttachmentState({ expected }) {
  const textarea = [...document.querySelectorAll('textarea')].filter(element => element.getClientRects().length).at(-1);
  const fiberKey = textarea && Object.keys(textarea).find(key => key.startsWith('__reactFiber$'));
  let fiber = fiberKey && textarea[fiberKey];
  for (let depth = 0; fiber && depth < 100; depth++, fiber = fiber.return) {
    const props = fiber.memoizedProps;
    if (!Array.isArray(props?.prompt?.content)) continue;
    const pending = props.pendingUploads;
    if (pending && (pending === true || pending.length || pending.size ||
      typeof pending === 'object' && Object.keys(pending).length)) return false;
    const parts = props.prompt.content.filter(part => part?.type !== 'input_text');
    const names = parts.map(part => part.filename ?? part.file_name);
    if (!expected.every(filename => names.includes(filename))) return false;
    return parts.map(part => ({ type: part.type, filename: part.filename, file_name: part.file_name,
      project_path: part.project_path }));
  }
  return false;
}

export function validateNativeReferences(parts, attachments) {
  const expected = new Map(attachments.map(item => [item.filename, item]));
  const refs = new Map();
  if (!Array.isArray(parts)) fail('browser_attachment_reference_missing');
  for (const part of parts) {
    const filename = partFilename(part);
    const path = projectPathPattern.exec(part?.project_path || '');
    if (part?.type !== 'input_file' || !expected.has(filename) || !path || path[1] !== filename ||
      part.filename && part.file_name && part.filename !== part.file_name) {
      fail('browser_attachment_reference_invalid');
    }
    if (refs.has(filename)) fail('browser_attachment_reference_duplicate');
    refs.set(filename, { type: 'input_file', filename, project_path: part.project_path });
  }
  if (refs.size !== expected.size) fail('browser_attachment_reference_missing');
  return attachments.map(item => refs.get(item.filename));
}

export function nativeAttachmentInput(input, body, attachments, prepared) {
  const parts = [];
  if (!Array.isArray(body?.input)) fail('browser_attachment_reference_missing');
  for (const message of body.input) {
    if (!Array.isArray(message?.content)) continue;
    for (const part of message.content) {
      if (part?.type === 'input_text' || part?.type === 'text') continue;
      if (message.role !== 'user') fail('browser_attachment_reference_invalid');
      parts.push(part);
    }
  }
  const unique = validateResolvedAttachments(attachments);
  const native = validateNativeReferences(parts, unique);
  if (!prepared || JSON.stringify(native) !== JSON.stringify(prepared)) fail('browser_attachment_reference_changed');
  if (!Array.isArray(input)) fail('browser_attachment_input_invalid');
  const index = input.findLastIndex(message => message.role === 'user');
  if (index < 0 || !Array.isArray(input[index].content)) fail('browser_attachment_input_invalid');
  const byName = new Map(native.map(part => [part.filename, part]));
  const legend = attachments.map((item, offset) => `[Attachment ${offset + 1}]: ${byName.get(item.filename).project_path}`).join('\n');
  const instruction = 'Attached files are available in the Prism project at the paths below. ' +
    'You may use native read-only tools only to inspect these attachments (including viewing images or reading PDFs). ' +
    'This permission is separate from the caller actions in the prompt; emit caller actions in the requested format. ' +
    'Do not create, edit, rename, or delete project files.\n' + legend;
  return input.map((message, offset) => offset !== index ? message : { ...message, content: [
    ...message.content, { type: 'input_text', text: instruction }, ...native,
  ] });
}

export class NativeAttachmentUpload {
  constructor(page, projectId, attachments, signal, checkCurrent = () => {}) {
    this.page = page;
    this.projectId = projectId;
    this.attachments = validateResolvedAttachments(attachments);
    this.signal = signal;
    this.checkCurrent = () => { aborted(signal); checkCurrent(); };
    this.active = null;
    this.references = null;
  }

  allowsUpload(request) {
    if (!this.active || this.signal?.aborted || request.method() !== 'POST') return false;
    const headers = request.headers();
    let filename;
    try { filename = decodeURIComponent(headers['x-prism-file-name'] || ''); } catch { return false; }
    return headers['x-prism-project-id'] === this.projectId && filename === this.active.filename;
  }

  async prepare() {
    const attached = [];
    for (const item of this.attachments) {
      this.checkCurrent();
      this.active = item;
      try {
        await this.upload(item);
        this.checkCurrent();
        attached.push(item);
        const ready = await this.page.waitForFunction(composerAttachmentState,
          { expected: attached.map(file => file.filename) }, { timeout: 90000 });
        this.checkCurrent();
        let parts;
        try { parts = await ready.jsonValue(); } finally { await ready.dispose?.(); }
        this.checkCurrent();
        this.references = validateNativeReferences(parts, attached);
        await this.page.waitForFunction(() => ![...document.querySelectorAll('[aria-label]')]
          .some(element => element.getClientRects().length && /^Uploading /i.test(element.getAttribute('aria-label') || '')),
        null, { timeout: 30000 });
        this.checkCurrent();
      } finally {
        this.active = null;
      }
    }
    return this.references;
  }

  async upload(item) {
    const page = this.page;
    const payload = { name: item.filename, mimeType: item.mimeType, buffer: item.data };
    const existing = page.locator('input[type="file"]');
    this.checkCurrent();
    if (await existing.count()) {
      this.checkCurrent();
      await existing.last().setInputFiles(payload, { timeout: 15000 });
    } else {
      this.checkCurrent();
      await page.getByRole('button', { name: 'Upload files & photos', exact: true }).last().click({ timeout: 15000 });
      this.checkCurrent();
      const chooserPromise = page.waitForEvent('filechooser', { timeout: 15000 });
      chooserPromise.catch(() => {});
      await page.getByRole('menuitem').filter({ hasText: /^Upload files & photos/ }).first().click({ timeout: 15000 });
      this.checkCurrent();
      const chooser = await chooserPromise;
      this.checkCurrent();
      await chooser.setFiles(payload, { timeout: 15000 });
    }
    this.checkCurrent();
    // Content-hashed filenames make Prism's native duplicate-file reuse safe.
    const reuse = page.getByRole('button', { name: 'Use existing file', exact: true });
    if (await reuse.waitFor({ state: 'visible', timeout: 1500 }).then(() => true).catch(() => false)) {
      this.checkCurrent();
      await reuse.click({ timeout: 10000 });
    }
    this.checkCurrent();
  }
}
