import test from 'node:test';
import assert from 'node:assert/strict';
import { CHARS_PER_TOKEN, PromptCache } from '../src/prompt-cache.mjs';

const tokens = count => 'x'.repeat(count * CHARS_PER_TOKEN);

test('a growing conversation reads its shared prefix from the cache in 128-token blocks', () => {
  const cache = new PromptCache();
  const turn1 = `${tokens(3000)}TASK`;
  assert.equal(cache.observe('32', turn1), 0, 'the first prompt is a cache write, reported as nothing cached');
  const turn2 = `${turn1}\n[result] tool output`;
  // The shared prefix is the whole first prompt (3000 tokens + 4 chars) -> 3001 tokens -> 2944 in blocks of 128.
  assert.equal(cache.observe('32', turn2), Math.floor(Math.floor(turn1.length / CHARS_PER_TOKEN) / 128) * 128);
  assert.equal(cache.observe('32', turn2), Math.floor(Math.floor(turn2.length / CHARS_PER_TOKEN) / 128) * 128,
    'resending an identical prompt hits its full length');
});

test('short prompts, other accounts and different prefixes are not cached', () => {
  const cache = new PromptCache();
  cache.record('32', tokens(5000));
  assert.equal(cache.lookup('32', tokens(1000)), 0, 'below 1024 tokens nothing is cached');
  assert.equal(cache.lookup('24', tokens(5000)), 0, 'the cache is per source account');
  assert.equal(cache.lookup('32', `y${tokens(5000)}`), 0, 'a different first character shares no prefix');
  assert.equal(cache.lookup('32', `${tokens(1000)}y${tokens(4000)}`), 0, 'a shared prefix under 1024 tokens is not a hit');
  assert.equal(cache.lookup('32', `${tokens(2000)}y`), 1920);
  assert.equal(cache.lookup('32', 42), 0);
});

test('entries expire after the TTL and the cache can be turned off', () => {
  let now = 0;
  const cache = new PromptCache({ ttlMs: 600000, now: () => now });
  cache.record('32', tokens(4000));
  now = 599999;
  assert.equal(cache.lookup('32', tokens(4000)), 3968);
  now = 600000;
  assert.equal(cache.lookup('32', tokens(4000)), 0, 'unused for the TTL, the prompt has left the cache');
  assert.equal(cache.accounts.size, 0);
  const off = new PromptCache({ ttlMs: 0 });
  off.record('32', tokens(4000));
  assert.equal(off.observe('32', tokens(4000)), 0);
});

test('clear releases all prompt cache entries for a disabled lifecycle', () => {
  const cache = new PromptCache();
  cache.record('32', tokens(4000));
  cache.record('24', tokens(4000));
  assert.equal(cache.accounts.size, 2);
  cache.clear();
  assert.equal(cache.accounts.size, 0);
  assert.equal(cache.lookup('32', tokens(4000)), 0);
});

test('the cache keeps at most maxEntries prompts per account, dropping the least recent', () => {
  const cache = new PromptCache({ maxEntries: 2 });
  cache.record('32', `a${tokens(2000)}`);
  cache.record('32', `b${tokens(2000)}`);
  cache.record('32', `a${tokens(2000)}`); // refreshed, now most recent
  cache.record('32', `c${tokens(2000)}`);
  assert.equal(cache.lookup('32', `b${tokens(2000)}`), 0);
  assert.ok(cache.lookup('32', `a${tokens(2000)}`) > 0);
  assert.ok(cache.lookup('32', `c${tokens(2000)}`) > 0);
});

function oldEstimate(first, second) {
  let common = 0;
  while (common < Math.min(first.length, second.length) && first.charCodeAt(common) === second.charCodeAt(common)) common += 1;
  const count = Math.floor(common / CHARS_PER_TOKEN);
  return count < 1024 ? 0 : Math.floor(count / 128) * 128;
}

test('fifty deterministic randomized prompt pairs exactly match the old prefix algorithm', () => {
  let seed = 0x20261002;
  const random = () => { seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0; return seed / 2 ** 32; };
  for (let index = 0; index < 50; index += 1) {
    const prefixLength = index < 8 ? [0, 3071, 3072, 3073, 3455, 3456, 3840, 262144][index] : Math.floor(random() * 16000);
    const prefix = Array.from({ length: prefixLength }, () => String.fromCharCode(Math.floor(random() * 65536))).join('');
    const first = prefix + 'A' + 'first'.repeat(Math.floor(random() * 100));
    const second = index % 5 === 0 ? first : index % 5 === 1 ? first + 'extended' : prefix + 'Bsecond';
    const cache = new PromptCache();
    cache.record('32', first);
    assert.equal(cache.lookup('32', second), oldEstimate(first, second), `pair ${index}`);
    assert.equal(cache.observe('32', second), oldEstimate(first, second), `observe ${index}`);
    assert.equal(cache.lookup('32', second), oldEstimate(second, second), `identical ${index}`);
  }
});

test('a 256 KiB prompt occupies about 11 KiB of hashes and stores no plaintext', () => {
  const cache = new PromptCache();
  const prompt = 'x'.repeat(256 * 1024);
  cache.record('32', prompt);
  const [entry] = cache.accounts.get('32');
  assert.deepEqual(Object.keys(entry).sort(), ['hashes', 'length', 'used']);
  assert.equal(entry.length, prompt.length);
  assert.equal(entry.hashes.byteLength, Math.ceil(prompt.length / 384) * 16);
  assert.ok(entry.hashes.byteLength < 11 * 1024);
  assert.equal(cache.lookup('32', prompt), oldEstimate(prompt, prompt));
});

test('partial blocks distinguish entries without rounding cached tokens upward', () => {
  const cache = new PromptCache();
  const prefix = tokens(1024);
  cache.record('32', prefix + 'a');
  cache.record('32', prefix + 'b');
  assert.equal(cache.accounts.get('32').length, 2);
  cache.record('32', prefix + 'a');
  assert.equal(cache.accounts.get('32').length, 2);
  assert.equal(cache.lookup('32', prefix + 'c'), 1024);
  assert.equal(cache.lookup('32', tokens(1023)), 0);
});

test('hashes preserve JS code units including unpaired surrogates and split surrogate pairs', () => {
  const prefix = 'x'.repeat(383);
  const first = prefix + '\ud800' + tokens(3000);
  const second = prefix + '\ud801' + tokens(3000);
  const cache = new PromptCache();
  cache.record('32', first);
  assert.equal(cache.lookup('32', second), 0);
  const unicode = prefix + '😀' + tokens(3000);
  cache.record('32', unicode);
  assert.equal(cache.lookup('32', unicode + 'extended'), oldEstimate(unicode, unicode + 'extended'));
});

test('the default sixty-four entry bound and record refresh retain TTL behavior', () => {
  let now = 0;
  const cache = new PromptCache({ ttlMs: 10, now: () => now });
  for (let index = 0; index < 65; index += 1) cache.record('32', `${index}:${tokens(2000)}`);
  assert.equal(cache.accounts.get('32').length, 64);
  assert.equal(cache.lookup('32', `0:${tokens(2000)}`), 0);
  now = 9;
  cache.record('32', `1:${tokens(2000)}`);
  now = 10;
  assert.ok(cache.lookup('32', `1:${tokens(2000)}`) > 0);
  assert.equal(cache.accounts.get('32').length, 1);
  now = 19;
  assert.equal(cache.lookup('32', `1:${tokens(2000)}`), 0);
});

test('conversation scopes are globally LRU bounded and TTL-pruned even when never used again', () => {
  let now = 0;
  const cache = new PromptCache({ maxKeys: 2, ttlMs: 100, now: () => now });
  cache.record('32:one', tokens(2000));
  cache.record('32:two', tokens(2000));
  cache.record('32:one', tokens(2000));
  cache.record('32:three', tokens(2000));
  assert.equal(cache.accounts.size, 2);
  assert.equal(cache.accounts.has('32:two'), false);
  now = 110;
  cache.record('32:four', tokens(2000));
  assert.deepEqual([...cache.accounts.keys()], ['32:four']);
});
