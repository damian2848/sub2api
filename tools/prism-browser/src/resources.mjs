import { readFile } from 'node:fs/promises';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { PrismError } from './errors.mjs';

const execFileAsync = promisify(execFile);
const MIB = 1024 * 1024;
const number = text => /^\d+$/.test(String(text).trim()) ? Number(String(text).trim()) : null;
const finiteLimit = text => { const value = number(text); return value && value < Number.MAX_SAFE_INTEGER ? value : null; };

// Container memory includes Chromium and all of its renderers; Node RSS alone is not a safe guard.
// Resolve the process's actual cgroup instead of assuming the root group's counters are its own.
export async function memorySnapshot({ read = readFile, platform = process.platform, pid = process.pid,
  run = execFileAsync, rss = () => process.memoryUsage().rss } = {}) {
  if (platform === 'linux') {
    let memberships = '';
    try { memberships = await read('/proc/self/cgroup', 'utf8'); } catch {}
    const rows = String(memberships).trim().split('\n');
    const v2 = rows.find(row => row.startsWith('0::'))?.slice(3);
    const v1 = rows.map(row => row.split(':')).find(row => row[1]?.split(',').includes('memory'))?.[2];
    const candidates = [];
    if (v2 !== undefined) {
      if (v2 !== '/' && !v2.includes('..')) candidates.push({ root: `/sys/fs/cgroup${v2}`, version: 2 });
      candidates.push({ root: '/sys/fs/cgroup', version: 2 });
    }
    if (v1 !== undefined) {
      if (v1 !== '/' && !v1.includes('..')) candidates.push({ root: `/sys/fs/cgroup/memory${v1}`, version: 1 });
      candidates.push({ root: '/sys/fs/cgroup/memory', version: 1 });
    }
    // Namespaced containers may expose counters at root even when /proc reports a host-relative path.
    if (!candidates.length) candidates.push({ root: '/sys/fs/cgroup', version: 2 }, { root: '/sys/fs/cgroup/memory', version: 1 });
    for (const { root, version } of candidates) {
      try {
        const usedBytes = number(await read(`${root}/${version === 2 ? 'memory.current' : 'memory.usage_in_bytes'}`, 'utf8'));
        if (!Number.isFinite(usedBytes)) continue;
        let limitBytes = null;
        try { limitBytes = finiteLimit(await read(`${root}/${version === 2 ? 'memory.max' : 'memory.limit_in_bytes'}`, 'utf8')); } catch {}
        return { usedBytes, limitBytes, source: `cgroup_v${version}`, includesChromium: true, degraded: false };
      } catch {}
    }
  }
  // Local development fallback sums this sidecar's process subtree, including Chromium children.
  // It excludes unrelated processes but is a sampled RSS estimate (shared pages may count twice).
  try {
    const { stdout } = await run('ps', ['-axo', 'pid=,ppid=,rss='], { timeout: 2000, maxBuffer: 8 * MIB });
    const rows = String(stdout).trim().split('\n').map(line => line.trim().split(/\s+/).map(Number))
      .filter(row => row.length === 3 && row.every(Number.isFinite));
    const members = new Set([pid]);
    for (let changed = true; changed;) {
      changed = false;
      for (const [child, parent] of rows) if (members.has(parent) && !members.has(child)) { members.add(child); changed = true; }
    }
    const usedBytes = rows.filter(([child]) => members.has(child)).reduce((sum, [, , kb]) => sum + kb * 1024, 0);
    if (usedBytes > 0) return { usedBytes, limitBytes: null, source: 'process_tree_rss', includesChromium: true, degraded: true };
  } catch {}
  return { usedBytes: rss(), limitBytes: null, source: 'node_rss_only', includesChromium: false, degraded: true };
}

export class ResourceGuard {
  constructor({ limitBytes = 0, reserveBytes = 0, sample = memorySnapshot, onAudit = () => {} } = {}) {
    if (!Number.isFinite(limitBytes) || limitBytes < 0 || !Number.isFinite(reserveBytes) || reserveBytes < 0) throw new Error('invalid_memory_guard');
    this.limitBytes = limitBytes;
    this.reserveBytes = reserveBytes;
    this.sample = sample;
    this.onAudit = onAudit;
    this.last = null;
    this.rejections = 0;
  }
  async snapshot() {
    const memory = await this.sample();
    const thresholdBytes = memory.limitBytes && this.limitBytes ? Math.min(memory.limitBytes, this.limitBytes) : this.limitBytes;
    this.last = { ...memory, thresholdBytes, reserveBytes: this.reserveBytes,
      pressured: Boolean(thresholdBytes && memory.usedBytes + this.reserveBytes >= thresholdBytes), rejections: this.rejections };
    return this.last;
  }
  async assertAdmission(kind = 'context') {
    // Opt-in: with no configured limit nothing is sampled, audited or refused (0 = off).
    if (!this.limitBytes) return null;
    const memory = await this.snapshot();
    this.onAudit('memory_admission', { kind, ...memory });
    if (!memory.pressured) return memory;
    this.rejections += 1;
    const error = new PrismError('browser_memory_pressure', 429);
    error.retryAfterSeconds = 10;
    // Admission only: existing pages and polls continue, and no context is killed here.
    throw error;
  }
}

// Bounded, content-free latency samples for preparation/load/queue evidence before enabling multiplex.
export class RuntimeMetrics {
  constructor({ capacity = 256 } = {}) {
    if (!Number.isInteger(capacity) || capacity < 1 || capacity > 100000) throw new Error('invalid_metrics_capacity');
    this.capacity = capacity; this.series = new Map();
  }
  record(name, milliseconds, fields = {}) {
    if (!/^[a-z_]{1,48}$/.test(name) || !Number.isFinite(milliseconds) || milliseconds < 0) return;
    let series = this.series.get(name);
    if (!series) { if (this.series.size >= 32) return; this.series.set(name, series = { count: 0, totalMs: 0, samples: [], last: null }); }
    series.count += 1; series.totalMs += milliseconds;
    series.samples.push(milliseconds); if (series.samples.length > this.capacity) series.samples.shift();
    series.last = { ms: Math.round(milliseconds), ...Object.fromEntries(Object.entries(fields)
      .filter(([key, value]) => /^(source|worker|multiplex)$/.test(key) && ['number', 'boolean'].includes(typeof value) ||
        key === 'cache_mode' && ['cdp_fetch_static_cache', 'playwright_route_cache_disabled'].includes(value))) };
  }
  snapshot() {
    return Object.fromEntries([...this.series].map(([name, series]) => {
      const sorted = [...series.samples].sort((a, b) => a - b);
      return [name, { count: series.count, mean_ms: Math.round(series.totalMs / series.count),
        p50_ms: Math.round(sorted[Math.max(0, Math.ceil(sorted.length * .5) - 1)] || 0),
        p95_ms: Math.round(sorted[Math.max(0, Math.ceil(sorted.length * .95) - 1)] || 0), last: series.last }];
    }));
  }
}
