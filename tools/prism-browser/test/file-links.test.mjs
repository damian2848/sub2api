import test from 'node:test';
import assert from 'node:assert/strict';
import { fileLinkOptions, rewriteFileLinks } from '../src/file-links.mjs';

const options = { cwd: '/Users/dev/project' };

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

test('enabling requires Responses, the instruction phrase, and the last cwd being absolute', () => {
  assert.deepEqual(fileLinkOptions('responses', ['Use CLICKABLE MARKDOWN LINKS.'], ['<cwd>/old</cwd>', '<cwd>/new</cwd>']), { cwd: '/new' });
  for (const args of [
    ['chat', ['clickable markdown link'], ['<cwd>/project</cwd>']],
    ['responses', ['other instructions'], ['clickable markdown link <cwd>/project</cwd>']],
    ['responses', ['clickable markdown link'], ['no environment']],
    ['responses', ['clickable markdown link'], ['<cwd>/project</cwd><cwd>relative</cwd>']],
    ['responses', ['clickable markdown link'], ['<cwd></cwd>']],
  ]) assert.equal(fileLinkOptions(...args), undefined);
});
