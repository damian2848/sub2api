// Validation for arguments emitted by Prism's prompt-emulated tool protocol.
//
// This intentionally implements the validation vocabulary that clients use for
// function parameters (Draft 7 and Draft 2020-12), without executing schemas or
// loading remote references.  A small, bounded Lark grammar recogniser is also
// provided for free-form tools which carry an `x-lark`/`lark` extension.  Both
// validators are deterministic and fail closed: an invalid schema or grammar
// never turns into an executable tool call.

const SCHEMA_KEYS = new Set([
  '$schema', '$id', '$ref', '$defs', 'definitions', '$anchor', '$comment',
  'type', 'enum', 'const', 'multipleOf', 'maximum', 'exclusiveMaximum',
  'minimum', 'exclusiveMinimum', 'maxLength', 'minLength', 'pattern',
  'format', 'contentEncoding', 'contentMediaType', 'maxItems', 'minItems',
  'uniqueItems', 'maxContains', 'minContains', 'maxProperties', 'minProperties',
  'required', 'dependencies', 'dependentRequired', 'dependentSchemas', 'properties',
  'patternProperties', 'additionalProperties', 'propertyNames', 'items',
  'prefixItems', 'additionalItems', 'contains', 'unevaluatedItems', 'unevaluatedProperties',
  'allOf', 'anyOf', 'oneOf', 'not', 'if', 'then', 'else', 'contains',
  'default', 'examples', 'title', 'description', 'readOnly', 'writeOnly',
]);

const own = (value, key) => Object.prototype.hasOwnProperty.call(value, key);
const isObject = value => value !== null && typeof value === 'object' && !Array.isArray(value);
const isNumber = value => typeof value === 'number' && Number.isFinite(value);
const isInteger = value => typeof value === 'number' && Number.isInteger(value) && Number.isFinite(value);
const VALID_TYPES = new Set(['null', 'boolean', 'object', 'array', 'number', 'integer', 'string']);
const deepEqual = (a, b) => {
  if (Object.is(a, b)) return true;
  if (typeof a !== typeof b || a === null || b === null) return false;
  if (Array.isArray(a)) return a.length === b.length && a.every((item, index) => deepEqual(item, b[index]));
  if (isObject(a)) {
    const ka = Object.keys(a), kb = Object.keys(b);
    return ka.length === kb.length && ka.every(key => own(b, key) && deepEqual(a[key], b[key]));
  }
  return false;
};

function typeMatches(value, type) {
  switch (type) {
    case 'null': return value === null;
    case 'boolean': return typeof value === 'boolean';
    case 'object': return isObject(value);
    case 'array': return Array.isArray(value);
    case 'number': return isNumber(value);
    case 'integer': return isInteger(value);
    case 'string': return typeof value === 'string';
    default: return false;
  }
}

function formatMatches(value, format) {
  if (typeof value !== 'string') return true;
  // These are intentionally the non-controversial syntax checks from the
  // JSON Schema format vocabulary. Unknown formats are annotations.
  switch (format) {
    case 'date': return /^\d{4}-\d{2}-\d{2}$/.test(value);
    case 'time': return /^\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})$/.test(value);
    case 'date-time': return /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})$/.test(value);
    case 'email': return /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(value);
    case 'hostname': return /^(?=.{1,253}$)(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$/.test(value);
    case 'ipv4': return value.split('.').length === 4 && value.split('.').every(part => /^\d{1,3}$/.test(part) && Number(part) <= 255);
    case 'ipv6': return value.includes(':') && /^[0-9a-f:.]+$/i.test(value);
    case 'uuid': return /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(value);
    case 'regex': try { new RegExp(value); return true; } catch { return false; }
    case 'uri': case 'uri-reference': try { new URL(value, format === 'uri-reference' ? 'http://schema.invalid' : undefined); return true; } catch { return false; }
    default: return true;
  }
}

function resolveRef(root, ref) {
  if (typeof ref !== 'string' || !ref.startsWith('#/')) return null;
  let value = root;
  for (const piece of ref.slice(2).split('/')) {
    const key = piece.replace(/~1/g, '/').replace(/~0/g, '~');
    if (!isObject(value) || !own(value, key)) return null;
    value = value[key];
  }
  return value;
}

function schemaLooksLikeSchema(schema) {
  if (typeof schema === 'boolean') return true;
  if (!isObject(schema)) return false;
  return Object.keys(schema).some(key => SCHEMA_KEYS.has(key));
}

function validateSchema(value, schema, path, ctx, seen) {
  if (schema === true || schema === undefined || schema === null) return null;
  if (schema === false) return `${path}: schema forbids this value`;
  if (!isObject(schema)) return null; // Legacy adapters sometimes used {cmd: "string"}; preserve permissive behaviour.
  if (seen.has(schema)) return `${path}: cyclic schema`;
  seen.add(schema);
  if (own(schema, 'type')) {
    const types = Array.isArray(schema.type) ? schema.type : [schema.type];
    if (!types.length || types.some(type => typeof type !== 'string' || !VALID_TYPES.has(type))) {
      seen.delete(schema); return `${path}: invalid schema type`;
    }
  }
  if (own(schema, 'enum') && !Array.isArray(schema.enum)) { seen.delete(schema); return `${path}: invalid enum`; }
  if (own(schema, 'required') && (!Array.isArray(schema.required) || schema.required.some(key => typeof key !== 'string'))) {
    seen.delete(schema); return `${path}: invalid required`; }
  if (own(schema, 'properties') && !isObject(schema.properties)) { seen.delete(schema); return `${path}: invalid properties`; }
  if (own(schema, 'patternProperties') && !isObject(schema.patternProperties)) { seen.delete(schema); return `${path}: invalid patternProperties`; }
  if (own(schema, 'additionalProperties') && typeof schema.additionalProperties !== 'boolean' && !isObject(schema.additionalProperties)) {
    seen.delete(schema); return `${path}: invalid additionalProperties`;
  }
  if (own(schema, 'items') && typeof schema.items !== 'boolean' && !isObject(schema.items) && !Array.isArray(schema.items)) {
    seen.delete(schema); return `${path}: invalid items`;
  }
  if (typeof schema.$ref === 'string') {
    const target = resolveRef(ctx.root, schema.$ref);
    if (!target) return `${path}: unresolved schema reference`;
    const error = validateSchema(value, target, path, ctx, seen);
    seen.delete(schema);
    return error;
  }
  if (own(schema, 'type')) {
    const types = Array.isArray(schema.type) ? schema.type : [schema.type];
    if (!types.every(type => typeof type === 'string') || !types.some(type => typeMatches(value, type))) {
      seen.delete(schema); return `${path}: expected ${types.join(' or ')}`;
    }
  }
  if (own(schema, 'enum') && (!Array.isArray(schema.enum) || !schema.enum.some(item => deepEqual(item, value)))) {
    seen.delete(schema); return `${path}: value is not in enum`;
  }
  if (own(schema, 'const') && !deepEqual(value, schema.const)) { seen.delete(schema); return `${path}: value differs from const`; }

  if (isNumber(value)) {
    if (own(schema, 'multipleOf') && isNumber(schema.multipleOf) && schema.multipleOf > 0 &&
      Math.abs(value / schema.multipleOf - Math.round(value / schema.multipleOf)) > 1e-9) {
      seen.delete(schema); return `${path}: is not a multipleOf ${schema.multipleOf}`;
    }
    if (own(schema, 'maximum') && isNumber(schema.maximum) && value > schema.maximum) { seen.delete(schema); return `${path}: exceeds maximum`; }
    if ((isNumber(schema.exclusiveMaximum) && value >= schema.exclusiveMaximum) ||
      (schema.exclusiveMaximum === true && isNumber(schema.maximum) && value >= schema.maximum)) { seen.delete(schema); return `${path}: exceeds exclusiveMaximum`; }
    if (own(schema, 'minimum') && isNumber(schema.minimum) && value < schema.minimum) { seen.delete(schema); return `${path}: below minimum`; }
    if ((isNumber(schema.exclusiveMinimum) && value <= schema.exclusiveMinimum) ||
      (schema.exclusiveMinimum === true && isNumber(schema.minimum) && value <= schema.minimum)) { seen.delete(schema); return `${path}: below exclusiveMinimum`; }
  }
  if (typeof value === 'string') {
    const length = [...value].length;
    if (own(schema, 'minLength') && isInteger(schema.minLength) && length < schema.minLength) { seen.delete(schema); return `${path}: shorter than minLength`; }
    if (own(schema, 'maxLength') && isInteger(schema.maxLength) && length > schema.maxLength) { seen.delete(schema); return `${path}: longer than maxLength`; }
    if (typeof schema.pattern === 'string') {
      try { if (!new RegExp(schema.pattern, 'u').test(value)) { seen.delete(schema); return `${path}: does not match pattern`; } }
      catch { seen.delete(schema); return `${path}: invalid pattern`; }
    }
    if (typeof schema.format === 'string' && !formatMatches(value, schema.format)) { seen.delete(schema); return `${path}: invalid ${schema.format}`; }
  }
  if (Array.isArray(value)) {
    if (own(schema, 'minItems') && isInteger(schema.minItems) && value.length < schema.minItems) { seen.delete(schema); return `${path}: fewer than minItems`; }
    if (own(schema, 'maxItems') && isInteger(schema.maxItems) && value.length > schema.maxItems) { seen.delete(schema); return `${path}: more than maxItems`; }
    if (schema.uniqueItems === true && value.some((item, index) => value.slice(0, index).some(previous => deepEqual(previous, item)))) { seen.delete(schema); return `${path}: duplicate array item`; }
    if (Array.isArray(schema.prefixItems)) for (let index = 0; index < Math.min(value.length, schema.prefixItems.length); index += 1) {
      const error = validateSchema(value[index], schema.prefixItems[index], `${path}[${index}]`, ctx, seen);
      if (error) { seen.delete(schema); return error; }
    }
    if (Array.isArray(schema.prefixItems) && schema.items === false && value.length > schema.prefixItems.length) {
      seen.delete(schema); return `${path}: items are forbidden`;
    }
    if (!own(schema, 'items') && Array.isArray(schema.prefixItems) && own(schema, 'unevaluatedItems') && value.length > schema.prefixItems.length) {
      if (schema.unevaluatedItems === false) { seen.delete(schema); return `${path}: unevaluated items are forbidden`; }
      if (isObject(schema.unevaluatedItems)) for (let index = schema.prefixItems.length; index < value.length; index += 1) {
        const error = validateSchema(value[index], schema.unevaluatedItems, `${path}[${index}]`, ctx, seen);
        if (error) { seen.delete(schema); return error; }
      }
    }
    if (Array.isArray(schema.items)) {
      for (let index = 0; index < Math.min(value.length, schema.items.length); index += 1) {
        const error = validateSchema(value[index], schema.items[index], `${path}[${index}]`, ctx, seen);
        if (error) { seen.delete(schema); return error; }
      }
      if (value.length > schema.items.length) {
        if (schema.additionalItems === false) { seen.delete(schema); return `${path}: additional items are forbidden`; }
        if (schema.additionalItems && typeof schema.additionalItems === 'object') for (let index = schema.items.length; index < value.length; index += 1) {
          const error = validateSchema(value[index], schema.additionalItems, `${path}[${index}]`, ctx, seen);
          if (error) { seen.delete(schema); return error; }
        }
      }
    } else if (schema.items === false) if (value.length && !Array.isArray(schema.prefixItems)) {
      seen.delete(schema); return `${path}: items are forbidden`;
    } else if (schema.items && typeof schema.items === 'object') {
      const start = Array.isArray(schema.prefixItems) ? schema.prefixItems.length : 0;
      for (let index = start; index < value.length; index += 1) {
        const error = validateSchema(value[index], schema.items, `${path}[${index}]`, ctx, seen);
        if (error) { seen.delete(schema); return error; }
      }
    }
    if (schema.contains) {
      const matches = value.reduce((count, item, index) => count + (!validateSchema(item, schema.contains, `${path}[${index}]`, ctx, new Set()) ? 1 : 0), 0);
      const minimum = isInteger(schema.minContains) ? schema.minContains : 1;
      const maximum = isInteger(schema.maxContains) ? schema.maxContains : Number.POSITIVE_INFINITY;
      if (matches < minimum || matches > maximum) { seen.delete(schema); return `${path}: contains constraint failed`; }
    }
  }
  if (isObject(value)) {
    const keys = Object.keys(value);
    if (own(schema, 'minProperties') && isInteger(schema.minProperties) && keys.length < schema.minProperties) { seen.delete(schema); return `${path}: fewer than minProperties`; }
    if (own(schema, 'maxProperties') && isInteger(schema.maxProperties) && keys.length > schema.maxProperties) { seen.delete(schema); return `${path}: more than maxProperties`; }
    if (Array.isArray(schema.required)) for (const key of schema.required) if (typeof key === 'string' && !own(value, key)) { seen.delete(schema); return `${path}: missing required property ${key}`; }
    const props = isObject(schema.properties) ? schema.properties : {};
    for (const [key, child] of Object.entries(props)) if (own(value, key)) {
      const error = validateSchema(value[key], child, `${path}.${key}`, ctx, seen);
      if (error) { seen.delete(schema); return error; }
    }
    const patterns = isObject(schema.patternProperties) ? Object.entries(schema.patternProperties) : [];
    for (const key of keys) for (const [pattern, child] of patterns) {
      let matched = false; try { matched = new RegExp(pattern, 'u').test(key); } catch { seen.delete(schema); return `${path}: invalid patternProperties`; }
      if (matched) { const error = validateSchema(value[key], child, `${path}.${key}`, ctx, seen); if (error) { seen.delete(schema); return error; } }
    }
    if (schema.additionalProperties === false) for (const key of keys) {
      const patternMatch = patterns.some(([pattern]) => { try { return new RegExp(pattern, 'u').test(key); } catch { return false; } });
      if (!own(props, key) && !patternMatch) { seen.delete(schema); return `${path}: unexpected property ${key}`; }
    } else if (schema.additionalProperties && isObject(schema.additionalProperties)) for (const key of keys) {
      const patternMatch = patterns.some(([pattern]) => { try { return new RegExp(pattern, 'u').test(key); } catch { return false; } });
      if (!own(props, key) && !patternMatch) { const error = validateSchema(value[key], schema.additionalProperties, `${path}.${key}`, ctx, seen); if (error) { seen.delete(schema); return error; } }
    }
    if (!own(schema, 'additionalProperties') && own(schema, 'unevaluatedProperties')) for (const key of keys) {
      const patternMatch = patterns.some(([pattern]) => { try { return new RegExp(pattern, 'u').test(key); } catch { return false; } });
      if (own(props, key) || patternMatch) continue;
      if (schema.unevaluatedProperties === false) { seen.delete(schema); return `${path}: unexpected property ${key}`; }
      if (isObject(schema.unevaluatedProperties)) {
        const error = validateSchema(value[key], schema.unevaluatedProperties, `${path}.${key}`, ctx, seen);
        if (error) { seen.delete(schema); return error; }
      }
    }
    if (schema.propertyNames) for (const key of keys) { const error = validateSchema(key, schema.propertyNames, `${path}.${key}`, ctx, seen); if (error) { seen.delete(schema); return error; } }
    const dependent = isObject(schema.dependentRequired) ? schema.dependentRequired :
      (isObject(schema.dependencies) ? schema.dependencies : {});
    for (const [key, required] of Object.entries(dependent)) if (own(value, key)) {
      if (Array.isArray(required)) for (const dep of required) {
        if (typeof dep === 'string' && !own(value, dep)) { seen.delete(schema); return `${path}: ${key} requires ${dep}`; }
      } else if (isObject(required)) {
        const error = validateSchema(value, required, path, ctx, new Set());
        if (error) { seen.delete(schema); return error; }
      }
    }
    if (isObject(schema.dependentSchemas)) for (const [key, dependency] of Object.entries(schema.dependentSchemas)) if (own(value, key)) {
      const error = validateSchema(value, dependency, path, ctx, new Set());
      if (error) { seen.delete(schema); return error; }
    }
  }
  for (const combiner of ['allOf', 'anyOf', 'oneOf']) if (Array.isArray(schema[combiner])) {
    const matches = schema[combiner].filter(child => !validateSchema(value, child, path, ctx, new Set())).length;
    if (combiner === 'allOf' && matches !== schema[combiner].length) { seen.delete(schema); return `${path}: allOf failed`; }
    if (combiner === 'anyOf' && matches === 0) { seen.delete(schema); return `${path}: anyOf failed`; }
    if (combiner === 'oneOf' && matches !== 1) { seen.delete(schema); return `${path}: oneOf failed`; }
  }
  if (schema.not && !validateSchema(value, schema.not, path, ctx, new Set())) { seen.delete(schema); return `${path}: not failed`; }
  if (schema.if) {
    const matched = !validateSchema(value, schema.if, path, ctx, new Set());
    const branch = matched ? schema.then : schema.else;
    if (branch) { const error = validateSchema(value, branch, path, ctx, new Set()); if (error) { seen.delete(schema); return error; } }
  }
  seen.delete(schema);
  return null;
}

export function validateJsonSchema(value, schema) {
  if (!schemaLooksLikeSchema(schema)) return { valid: true, errors: [] };
  const error = validateSchema(value, schema, '$', { root: schema }, new Set());
  return error ? { valid: false, errors: [error] } : { valid: true, errors: [] };
}

// ------------------------------ constrained Lark -------------------------

const BUILTINS = {
  CNAME: /[A-Za-z_][A-Za-z0-9_]*/y, NAME: /[A-Za-z_][A-Za-z0-9_]*/y,
  WORD: /[A-Za-z0-9_./:-]+/y, INT: /[+-]?\d+/y, SIGNED_INT: /[+-]?\d+/y,
  NUMBER: /[+-]?(?:\d+\.\d+|\d+)/y, SIGNED_NUMBER: /[+-]?(?:\d+\.\d+|\d+)/y,
  ESCAPED_STRING: /"(?:\\.|[^"\\])*"/y, STRING: /"(?:\\.|[^"\\])*"/y,
  WS: /\s+/y, _WS: /\s+/y,
};

function larkTokens(expression) {
  const out = [];
  for (let at = 0; at < expression.length;) {
    const ws = /^\s+/.exec(expression.slice(at)); if (ws) { at += ws[0].length; continue; }
    const c = expression[at];
    if ('|?*+()[]{}'.includes(c)) { out.push(c); at += 1; continue; }
    if (c === '"' || c === "'") {
      let end = at + 1, escaped = false;
      for (; end < expression.length; end += 1) { const x = expression[end]; if (!escaped && x === c) break; escaped = !escaped && x === '\\'; if (x !== '\\') escaped = false; }
      if (end >= expression.length) throw new Error('unterminated literal');
      let text; try { text = JSON.parse(c === '"' ? expression.slice(at, end + 1) : `"${expression.slice(at + 1, end).replace(/"/g, '\\"')}"`); } catch { throw new Error('invalid literal'); }
      out.push({ literal: text }); at = end + 1; continue;
    }
    if (c === '/') {
      let end = at + 1, escaped = false, inClass = false;
      for (; end < expression.length; end += 1) { const x = expression[end]; if (!escaped && x === '[') inClass = true; if (!escaped && x === ']') inClass = false; if (!escaped && x === '/' && !inClass) break; escaped = !escaped && x === '\\'; if (x !== '\\') escaped = false; }
      if (end >= expression.length) throw new Error('unterminated regex');
      out.push({ regex: expression.slice(at + 1, end) }); at = end + 1; continue;
    }
    const name = /^[A-Za-z_][A-Za-z0-9_]*/.exec(expression.slice(at));
    if (!name) throw new Error(`invalid grammar token at ${at}`);
    out.push({ name: name[0] }); at += name[0].length;
  }
  return out;
}

function parseLarkExpression(expression) {
  const tokens = larkTokens(expression); let at = 0;
  const primary = () => {
    const token = tokens[at++];
    if (!token) throw new Error('missing expression');
    if (token === '(') { const node = alternatives(); if (tokens[at++] !== ')') throw new Error('unclosed group'); return node; }
    if (token === '[') { const node = alternatives(); if (tokens[at++] !== ']') throw new Error('unclosed optional'); return { optional: node }; }
    if (token === '{') { const node = alternatives(); if (tokens[at++] !== '}') throw new Error('unclosed repetition'); return { repeat: node }; }
    if (token.literal !== undefined) return token;
    if (token.regex !== undefined) return token;
    if (token.name) return { name: token.name };
    throw new Error('invalid expression');
  };
  const factor = () => {
    let node = primary();
    if (tokens[at] === '?' || tokens[at] === '*' || tokens[at] === '+') { const op = tokens[at++]; node = op === '?' ? { optional: node } : op === '*' ? { repeat: node } : { plus: node }; }
    return node;
  };
  const sequence = () => { const nodes = []; while (at < tokens.length && tokens[at] !== '|' && tokens[at] !== ')' && tokens[at] !== ']' && tokens[at] !== '}') nodes.push(factor()); return { sequence: nodes }; };
  function alternatives() { const nodes = [sequence()]; while (tokens[at] === '|') { at += 1; nodes.push(sequence()); } return nodes.length === 1 ? nodes[0] : { alternatives: nodes }; }
  const node = alternatives(); if (at !== tokens.length) throw new Error('trailing grammar'); return node;
}

function compileLark(grammar) {
  if (typeof grammar !== 'string' || grammar.length === 0 || grammar.length > 64 * 1024) throw new Error('invalid_lark_grammar');
  const rules = new Map(), terminals = new Map(), ignored = [];
  for (const rawLine of grammar.split(/\r?\n/)) {
    const line = rawLine.trim(); if (!line || line.startsWith('//')) continue;
    if (line.startsWith('%ignore ')) {
      const token = line.slice(8).trim();
      const builtin = BUILTINS[token]; if (builtin) ignored.push(builtin.source);
      else if (terminals.has(token)) ignored.push(terminals.get(token));
      continue;
    }
    if (line.startsWith('%import ')) continue; // Builtins are available by name.
    if (line.startsWith('%')) continue; // Other directives are annotations for validation purposes.
    const match = /^([A-Za-z_][A-Za-z0-9_]*)\s*:\s*(.+)$/.exec(line);
    if (!match) throw new Error('invalid_lark_rule');
    let expression = match[2].trim().replace(/\s*->\s*[A-Za-z_][A-Za-z0-9_]*/g, '');
    // Lark allows a trailing rule comment.
    expression = expression.replace(/\s*\/\/.*$/, '').trim();
    if (!expression) throw new Error('empty_lark_rule');
    const node = parseLarkExpression(expression);
    if (/^[A-Z_]/.test(match[1])) {
      const literal = node?.literal; const regex = node?.regex;
      if (literal !== undefined) terminals.set(match[1], { literal });
      else if (regex !== undefined) terminals.set(match[1], { regex });
      else terminals.set(match[1], node);
    } else rules.set(match[1], node);
  }
  if (!rules.has('start')) throw new Error('lark_start_missing');
  const ignoreRegex = ignored.map(source => new RegExp(source, 'y'));
  return { rules, terminals, ignoreRegex };
}

function larkMatch(input, grammar) {
  const compiled = compileLark(grammar); const limit = 128 * 1024;
  if (input.length > limit) return false;
  const skip = position => { let at = position; for (;;) { let moved = false; for (const re of compiled.ignoreRegex) { re.lastIndex = at; const match = re.exec(input); if (match && match.index === at) { at += match[0].length; moved = true; } } if (!moved) return at; } };
  const memo = new Map();
  const nodeIds = new WeakMap(); let nextNodeId = 1;
  const nodeKey = node => {
    if (!node || typeof node !== 'object') return String(node);
    if (!nodeIds.has(node)) nodeIds.set(node, nextNodeId++);
    return nodeIds.get(node);
  };
  const run = (node, position, depth = 0) => {
    if (depth > 256) return [];
    const key = `${nodeKey(node)}@${position}`; if (memo.has(key)) return memo.get(key);
    let result = [];
    if (node?.literal !== undefined) { const at = skip(position); if (input.startsWith(node.literal, at)) result = [at + node.literal.length]; }
    else if (node?.regex !== undefined) { const at = skip(position); try { const re = new RegExp(node.regex, 'y'); re.lastIndex = at; const match = re.exec(input); if (match) result = [at + match[0].length]; } catch { result = []; } }
    else if (node?.name) {
      const at = skip(position); const terminal = compiled.terminals.get(node.name) ?? BUILTINS[node.name];
      if (terminal) result = run(terminal.literal !== undefined || terminal.regex !== undefined ? terminal : { regex: terminal.source }, at, depth + 1);
      else if (compiled.rules.has(node.name)) result = run(compiled.rules.get(node.name), at, depth + 1);
    } else if (node?.sequence) {
      result = [position]; for (const part of node.sequence) { const next = []; for (const candidate of result) next.push(...run(part, candidate, depth + 1)); result = [...new Set(next)]; if (!result.length) break; }
    } else if (node?.alternatives) result = [...new Set(node.alternatives.flatMap(part => run(part, position, depth + 1)))];
    else if (node?.optional) result = [position, ...run(node.optional, position, depth + 1)];
    else if (node?.repeat || node?.plus) {
      const first = node.plus ? run(node.plus, position, depth + 1) : [position, ...run(node.repeat, position, depth + 1)];
      const seen = new Set(first), queue = [...first];
      while (queue.length) { const candidate = queue.shift(); for (const next of run(node.repeat ?? node.plus, candidate, depth + 1)) if (next !== candidate && !seen.has(next)) { seen.add(next); queue.push(next); } }
      result = [...seen];
    }
    memo.set(key, result); return result;
  };
  const ends = run({ name: 'start' }, 0);
  const end = skip(input.length);
  return ends.includes(end);
}

function larkGrammarFrom(spec) {
  const params = spec?.params;
  for (const source of [spec?.lark, spec?.lark_grammar, params?.['x-lark'], params?.x_lark, params?.lark, params?.['x-lark-grammar']]) {
    if (typeof source === 'string') return source;
    if (isObject(source) && typeof source.grammar === 'string') return source.grammar;
  }
  return null;
}

export function validateLark(value, grammar) {
  try { return { valid: larkMatch(String(value ?? ''), grammar), errors: [] }; }
  catch (error) { return { valid: false, errors: [error instanceof Error ? error.message : 'invalid_lark_grammar'] }; }
}

export function validateToolArguments(spec, args) {
  const schema = spec?.params;
  const json = validateJsonSchema(args, schema);
  if (!json.valid) return json;
  const grammar = larkGrammarFrom(spec);
  if (!grammar) return json;
  const candidate = spec?.raw && isObject(args) && typeof args.input === 'string' ? args.input :
    (typeof args === 'string' ? args : JSON.stringify(args));
  return validateLark(candidate, grammar);
}
