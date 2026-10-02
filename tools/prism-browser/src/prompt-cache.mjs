// Estimated prompt caching for usage reports. Prism returns no token usage, so the sidecar
// estimates it; this adds the cache-read share the way OpenAI's automatic prompt caching
// behaves: the longest prefix shared with a recently processed prompt of the same account
// counts as cached once the prompt reaches 1024 tokens, in 128-token blocks, for a few minutes
// after that prompt was last used. Cache writes are never reported (OpenAI reports none).
// The figures are estimates, like the rest of the sidecar's usage.

export const CHARS_PER_TOKEN = 3;

function commonPrefix(a, b) {
  const limit = Math.min(a.length, b.length);
  let at = 0;
  // Compare in chunks first; long prompts usually share most of their text.
  const step = 4096;
  while (at + step <= limit && a.slice(at, at + step) === b.slice(at, at + step)) at += step;
  while (at < limit && a.charCodeAt(at) === b.charCodeAt(at)) at += 1;
  return at;
}

export class PromptCache {
  constructor({ ttlMs = 600000, maxEntries = 64, minTokens = 1024, blockTokens = 128, now = () => Date.now() } = {}) {
    this.ttlMs = ttlMs;
    this.maxEntries = maxEntries;
    this.minTokens = minTokens;
    this.blockTokens = blockTokens;
    this.now = now;
    this.accounts = new Map();
  }

  get enabled() { return this.ttlMs > 0 && this.maxEntries > 0; }

  // Cached tokens for `text` against the account's recent prompts. Does not record anything.
  lookup(key, text) {
    if (!this.enabled || typeof text !== 'string') return 0;
    const entries = this.prune(key);
    if (Math.ceil(text.length / CHARS_PER_TOKEN) < this.minTokens) return 0;
    let best = 0;
    for (const entry of entries) best = Math.max(best, commonPrefix(entry.text, text));
    const tokens = Math.floor(best / CHARS_PER_TOKEN);
    if (tokens < this.minTokens) return 0;
    return Math.floor(tokens / this.blockTokens) * this.blockTokens;
  }

  // Remembers a prompt that Prism processed, refreshing it if it is already known.
  record(key, text) {
    if (!this.enabled || typeof text !== 'string' || !text) return;
    const entries = this.prune(key).filter(entry => entry.text !== text);
    entries.push({ text, used: this.now() });
    while (entries.length > this.maxEntries) entries.shift();
    this.accounts.set(key, entries);
  }

  // Looks up and then records: the estimate for a request that has just completed.
  observe(key, text) {
    const cached = this.lookup(key, text);
    this.record(key, text);
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
