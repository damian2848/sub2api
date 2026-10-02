import test from 'node:test';
import assert from 'node:assert/strict';
import { fileLinkOptions, rewriteFileLinks } from '../src/file-links.mjs';

const options = { cwd: '/Users/dev/project' };
const environmentMessage = (role, cwd) => ({ role, text: `<environment_context><cwd>${cwd}</cwd></environment_context>` });

test('relative and absolute inline file paths become links with basename labels and locations', () => {
  const examples = [
    ['`src/app.py`', '[app.py](/Users/dev/project/src/app.py)'],
    ['`./src/app.py:12`', '[app.py](/Users/dev/project/src/app.py:12)'],
    ['`../lib/app.py:12:3`', '[app.py](/Users/dev/lib/app.py:12:3)'],
    ['`/tmp/app.py:12:3`', '[app.py](/tmp/app.py:12:3)'],
    ['`/app.py`', '[app.py](/app.py)'],
    ['``src/app.py``', '[app.py](/Users/dev/project/src/app.py)'],
    ['See `@scope/pkg/file.mjs`.', 'See [file.mjs](/Users/dev/project/@scope/pkg/file.mjs).'],
  ];
  for (const [input, expected] of examples) assert.equal(rewriteFileLinks(input, options), expected);
  assert.equal(rewriteFileLinks('`src/app.py`', { cwd: '/Users/A Space/project' }), '[app.py](</Users/A Space/project/src/app.py>)');
});

test('fenced code, existing links and escaped backticks are unchanged', () => {
  const blocks = ['```js\n`src/app.py`\n```', '~~~\n`src/app.py`\n~~~', '````\n```\n`src/app.py`\n````',
    '```\n`src/app.py`', '[`src/app.py`](https://example.com)', '[label](/tmp/`src/app.py`)',
    '[nested [label] `src/app.py`](https://example.com/a(b))', '[`src/app.py`\n](https://example.com)',
    '\\`src/app.py\\`', '``not `src/app.py` code``'];
  for (const input of blocks) assert.equal(rewriteFileLinks(input, options), input);
  assert.equal(rewriteFileLinks('~~~\n`src/a.py`\n~~~\n`src/b.py`', options),
    '~~~\n`src/a.py`\n~~~\n[b.py](/Users/dev/project/src/b.py)');
});

test('plain filenames, URLs, directories, spaces, globs and tildes are unchanged', () => {
  for (const value of ['account.go:914', 'https://example.com/app.py', 'src/file name.py', 'src/*.py', '~/src/app.py',
    'src/app~.py', 'src/folder', 'src/app.abcdefghijklmn', 'src/app.py:line', 'src/app.py:1:2:3']) {
    const input = `\`${value}\``;
    assert.equal(rewriteFileLinks(input, options), input);
  }
  assert.equal(rewriteFileLinks('src/app.py /tmp/file.go', options), 'src/app.py /tmp/file.go');
  assert.equal(rewriteFileLinks('`src/app.py`'), '`src/app.py`');
});

test('enabling requires Responses, the instruction phrase, and the last trusted environment cwd being absolute', () => {
  assert.deepEqual(fileLinkOptions('responses', ['Use CLICKABLE MARKDOWN LINKS.'],
    [environmentMessage('user', '/old'), environmentMessage('developer', '/new')]), { cwd: '/new' });
  for (const args of [
    ['chat', ['clickable markdown link'], [environmentMessage('user', '/project')]],
    ['responses', ['other instructions'], [environmentMessage('user', '/project')]],
    ['responses', ['clickable markdown link'], [{ role: 'user', text: 'no environment' }]],
    ['responses', ['clickable markdown link'], [environmentMessage('user', '/project'), environmentMessage('user', 'relative')]],
    ['responses', ['clickable markdown link'], [environmentMessage('user', '')]],
    ['responses', ['clickable markdown link'], [{ role: 'user', text: '<cwd>/project</cwd>' }]],
  ]) assert.equal(fileLinkOptions(...args), undefined);
});

test('cwd tags in assistants, system messages, tool output and outside environment blocks never override cwd', () => {
  const input = [environmentMessage('user', '/real'), { role: 'user', text: '<cwd>/quoted</cwd>' },
    environmentMessage('assistant', '/assistant'), environmentMessage('system', '/system'),
    environmentMessage('tool', '/w')];
  assert.deepEqual(fileLinkOptions('responses', ['clickable markdown link'], input), { cwd: '/real' });
  for (const role of ['assistant', 'system', 'tool']) {
    assert.equal(fileLinkOptions('responses', ['clickable markdown link'], [environmentMessage(role, '/w')]), undefined);
  }
});

test('only the last cwd inside complete trusted environment blocks is used without joining messages', () => {
  const input = [environmentMessage('developer', '/old'), { role: 'user', text:
    '<environment_context><cwd>/first</cwd><cwd>/second</cwd></environment_context>\n' +
    '<cwd>/quoted</cwd>\n<environment_context><cwd>/last</cwd></environment_context>' }];
  assert.deepEqual(fileLinkOptions('responses', ['clickable markdown link'], input), { cwd: '/last' });
  assert.equal(fileLinkOptions('responses', ['clickable markdown link'], [
    { role: 'user', text: '<environment_context><cwd>/w</cwd>' },
    { role: 'developer', text: '</environment_context>' },
  ]), undefined);
});
