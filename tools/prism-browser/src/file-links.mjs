import { posix } from 'node:path';

const FILE_PATH = /^(?:\/(?:[\w@+.-]+\/)*|(?:[\w@+.-]+\/)+)[\w@+.-]+\.[A-Za-z0-9]{1,12}(?::\d+(?::\d+)?)?$/;

export function fileLinkOptions(family, instructions, input) {
  if (family !== 'responses' || !instructions.some(text => /clickable markdown link/i.test(text))) return;
  const matches = [...input.join('\n').matchAll(/<cwd>([\s\S]*?)<\/cwd>/g)];
  const cwd = matches.at(-1)?.[1].trim();
  if (cwd && posix.isAbsolute(cwd) && !/[\r\n]/.test(cwd)) return { cwd };
}

function linkEnd(text, start) {
  let depth = 0;
  let cursor = start;
  for (; cursor < text.length; cursor += 1) {
    if (text[cursor] === '\\') { cursor += 1; continue; }
    if (text[cursor] === '[') depth += 1;
    if (text[cursor] === ']' && --depth === 0) break;
  }
  if (text[++cursor] !== '(') return start;
  depth = 1;
  for (cursor += 1; cursor < text.length; cursor += 1) {
    if (text[cursor] === '\\') { cursor += 1; continue; }
    if (text[cursor] === '(') depth += 1;
    if (text[cursor] === ')' && --depth === 0) return cursor + 1;
  }
  return start;
}

function codeEnd(text, start, delimiter) {
  let cursor = start + delimiter.length;
  while ((cursor = text.indexOf(delimiter, cursor)) !== -1) {
    if (text[cursor - 1] !== '`' && text[cursor + delimiter.length] !== '`') return cursor;
    cursor += delimiter.length;
  }
  return -1;
}

function fileLink(text, cwd) {
  if (!FILE_PATH.test(text)) return;
  const [, filename, location = ''] = /^(.*?)(:\d+(?::\d+)?)?$/.exec(text);
  const absolute = posix.isAbsolute(filename) ? filename : posix.normalize(`${cwd}/${filename}`);
  const target = absolute + location;
  return `[${posix.basename(filename)}](${/[\s()<>]/.test(target) ? `<${target}>` : target})`;
}

export function rewriteFileLinks(text, options) {
  if (!options?.cwd) return text;
  const output = [];
  let fence = null;
  for (let cursor = 0; cursor < text.length;) {
    if (cursor === 0 || text[cursor - 1] === '\n') {
      const newline = text.indexOf('\n', cursor);
      const end = newline === -1 ? text.length : newline + 1;
      const line = text.slice(cursor, end);
      const marker = /^ {0,3}(`{3,}|~{3,})([^\r\n]*)/.exec(line);
      if (fence || marker) {
        if (fence && marker && marker[1][0] === fence.char && marker[1].length >= fence.length && !marker[2].trim()) fence = null;
        else if (!fence && marker) fence = { char: marker[1][0], length: marker[1].length };
        output.push(line);
        cursor = end;
        continue;
      }
    }
    if (text[cursor] === '\\') {
      output.push(text.slice(cursor, cursor + 2));
      cursor += 2;
      continue;
    }
    if (text[cursor] === '[') {
      const end = linkEnd(text, cursor);
      if (end > cursor) {
        output.push(text.slice(cursor, end));
        cursor = end;
        continue;
      }
    }
    if (text[cursor] === '`') {
      let length = 1;
      while (text[cursor + length] === '`') length += 1;
      const end = codeEnd(text, cursor, '`'.repeat(length));
      if (end !== -1) {
        const content = text.slice(cursor + length, end);
        output.push(fileLink(content, options.cwd) || text.slice(cursor, end + length));
        cursor = end + length;
        continue;
      }
      output.push(text.slice(cursor, cursor + length));
      cursor += length;
      continue;
    }
    output.push(text[cursor]);
    cursor += 1;
  }
  return output.join('');
}
