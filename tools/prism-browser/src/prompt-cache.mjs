// Estimated prompt caching for usage reports. Prism returns no token usage, so the sidecar
// estimates it; this adds the cache-read share the way OpenAI's automatic prompt caching
// behaves: the longest prefix shared with a recently processed prompt of the same account
// counts as cached once the prompt reaches 1024 tokens, in 128-token blocks, for a few minutes
// after that prompt was last used. Cache writes are never reported (OpenAI reports none).
// The figures are estimates, like the rest of the sidecar's usage.

import { createHash } from 'node:crypto';

export const CHARS_PER_TOKEN = 3;
const HASH_BYTES = 16;

function promptHashes(text, blockChars) {
  const blocks = Math.ceil(text.length / blockChars);
  const hashes = Buffer.alloc(blocks * HASH_BYTES);
  let previous = Buffer.alloc(0);
  for (let index = 0; index < blocks; index += 1) {
    const chunk = Buffer.from(text.slice(index * blockChars, (index + 1) * blockChars), 'utf16le');
    previous = createHash('sha256').update(previous).update(chunk).digest().subarray(0, HASH_BYTES);
    previous.copy(hashes, index * HASH_BYTES);
  }
  return hashes;
}

function commonBlocks(entry, hashes, length, blockChars) {
  const limit = Math.floor(Math.min(entry.length, length) / blockChars);
  let matched = 0;
  while (matched < limit) {
    const offset = matched * HASH_BYTES;
    if (!entry.hashes.subarray(offset, offset + HASH_BYTES).equals(hashes.subarray(offset, offset + HASH_BYTES))) break;
    matched += 1;
  }
  return matched;
}

export class PromptCache {
  constructor({ ttlMs = 600000, maxEntries = 64, minTokens = 1024, blockTokens = 128, now = () => Date.now() } = {}) {
    if (!Number.isInteger(blockTokens) || blockTokens < 1) throw new RangeError('invalid_prompt_cache_block');
    this.ttlMs = ttlMs;
    this.maxEntries = maxEntries;
    this.minTokens = minTokens;
    this.blockTokens = blockTokens;
    this.now = now;
    this.accounts = new Map();
  }

  get enabled() { return this.ttlMs > 0 && this.maxEntries > 0; }

  // Cached tokens for `text` against the account's recent prompts. Does not record anything.
  lookup(key, text, suppliedHashes) {
    if (!this.enabled || typeof text !== 'string') return 0;
    const entries = this.prune(key);
    if (Math.ceil(text.length / CHARS_PER_TOKEN) < this.minTokens) return 0;
    const blockChars = this.blockTokens * CHARS_PER_TOKEN;
    const hashes = suppliedHashes || promptHashes(text, blockChars);
    let best = 0;
    for (const entry of entries) best = Math.max(best, commonBlocks(entry, hashes, text.length, blockChars));
    const tokens = best * this.blockTokens;
    if (tokens < this.minTokens) return 0;
    return tokens;
  }

  // Remembers a prompt that Prism processed, refreshing it if it is already known.
  record(key, text, suppliedHashes) {
    if (!this.enabled || typeof text !== 'string' || !text) return;
    const hashes = suppliedHashes || promptHashes(text, this.blockTokens * CHARS_PER_TOKEN);
    const entries = this.prune(key).filter(entry => entry.length !== text.length || !entry.hashes.equals(hashes));
    entries.push({ hashes, length: text.length, used: this.now() });
    while (entries.length > this.maxEntries) entries.shift();
    this.accounts.set(key, entries);
  }

  // Looks up and then records: the estimate for a request that has just completed.
  observe(key, text) {
    const hashes = this.enabled && typeof text === 'string' ? promptHashes(text, this.blockTokens * CHARS_PER_TOKEN) : undefined;
    const cached = this.lookup(key, text, hashes);
    this.record(key, text, hashes);
    return cached;
  }

  prune(key) {
    const cutoff = this.now() - this.ttlMs;
    const entries = (this.accounts.get(key) || []).filter(entry => entry.used > cutoff);
    if (entries.length) this.accounts.set(key, entries);
    else this.accounts.delete(key);
    return entries;
  }
}
