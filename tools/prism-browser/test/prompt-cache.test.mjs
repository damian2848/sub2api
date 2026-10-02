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
