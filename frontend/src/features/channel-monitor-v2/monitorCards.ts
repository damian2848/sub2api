import type { HealthState, MonitorCandyHistory, MonitorCandyResult, MonitorCoverage, MonitorMatrixBucket, MonitorMatrixRow, MonitorMetric } from '@/api/channelMonitorV2'

export const CANDY_HISTORY_LIMIT = 100

export function candyHistorySlots(history: MonitorCandyHistory): Array<MonitorCandyResult | null> {
  const results = history.results.slice(-CANDY_HISTORY_LIMIT)
  return [...Array<null>(CANDY_HISTORY_LIMIT - results.length).fill(null), ...results]
}

export function hasMonitorSamples(metric: MonitorMetric): boolean {
  return metric.has_samples === true || metric.request_count > 0
}

// Availability comes only from actual generation request facts, regardless of
// source. Task/quality verdicts must not manufacture transport successes.
export function effectiveAvailability(metric: MonitorMetric): number | null {
  const value = hasMonitorSamples(metric) ? 1 - metric.error_rate : null
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 && value <= 1 ? value : null
}

export function monitorAvailabilitySource(metric: MonitorMetric): 'business' | 'probe' | 'mixed' | null {
  const source = metric.availability_source
  return hasMonitorSamples(metric) && (source === 'business' || source === 'probe' || source === 'mixed') ? source : null
}

export function hasAvailabilitySamples(metric: MonitorMetric): boolean {
  return effectiveAvailability(metric) !== null
}

export function monitorRefreshSeconds(configSeconds: number | undefined, rows: readonly MonitorMatrixRow[], bootstrap: boolean): number {
  if (bootstrap) return 10
  let seconds = configSeconds && Number.isFinite(configSeconds) && configSeconds > 0 ? configSeconds : 300
  for (const row of rows) {
    for (const history of monitorCandyHistories(row)) {
      const interval = history.interval_minutes
      if (interval && Number.isFinite(interval) && interval > 0) {
        seconds = Math.min(seconds, interval * 60)
      }
    }
  }
  // Refresh before a healthy minute probe can appear stale in a five-minute snapshot.
  return Math.max(60, seconds)
}

export function monitorCandyHistories(row: MonitorMatrixRow): MonitorCandyHistory[] {
  return row.candy_histories?.length ? row.candy_histories : row.candy ? [row.candy] : []
}

// Derive a visual health state from the bucket's error rate when the server
// returned "unknown" because request_count < minimum_sample.  This lets
// low-traffic windows show red/green instead of a grey bar that hides failures.
function timelineHealthOverride(bucket: MonitorMatrixBucket): HealthState {
  const h = bucket.health.overall
  if (h !== 'unknown') return h
  // No actual request facts → genuinely unknown.
  if (!hasMonitorSamples(bucket.metrics)) return 'unknown'
  // Derive from error_rate alone (TTFT is unreliable at low N).
  const errorRate = bucket.metrics.error_rate
  if (!Number.isFinite(errorRate)) return 'unknown'
  const threshold = bucket.health.thresholds
  const warn = threshold?.warning_error_rate ?? 0.05
  const crit = threshold?.critical_error_rate ?? 0.20
  if (errorRate >= crit) return 'critical'
  if (errorRate >= warn) return 'warning'
  return 'healthy'
}

// Keep real time gaps. Each bar summarizes observed health in one of 18 equal
// time windows, never interpolating a successful request into missing history.
// Bars with traffic below the configured minimum_sample threshold still show
// their derived health color but are marked `lowSample` so the UI can render
// them with reduced confidence (e.g. semi-transparent).
export function monitorCardTimeline(row: MonitorMatrixRow, coverage: MonitorCoverage | undefined) {
  const start = Date.parse(coverage?.requested_start || '')
  const end = Date.parse(coverage?.requested_end || coverage?.data_through || '')
  if (!Number.isFinite(start) || !Number.isFinite(end) || end <= start) return []
  const width = (end - start) / 18
  const bars = Array.from({ length: 18 }, (_, index) => ({ start: start + index * width, end: start + (index + 1) * width, state: 'unknown' as HealthState, lowSample: false, observed: false, buckets: [] as MonitorMatrixBucket[] }))
  const severity: Record<HealthState, number> = { unknown: 0, healthy: 1, warning: 2, critical: 3 }
  for (const bucket of row.buckets) {
    const at = Date.parse(bucket.bucket_start)
    if (at < start || at >= end || !hasMonitorSamples(bucket.metrics)) continue
    const bar = bars[Math.floor((at - start) / width)]
    if (!bar) continue
    bar.observed = true
    bar.buckets.push(bucket)
    // Use the server health when it has enough samples; otherwise derive from
    // the error rate so low-traffic windows are not hidden behind grey.
    const effective = timelineHealthOverride(bucket)
    if (severity[effective] > severity[bar.state]) bar.state = effective
    // Track whether ANY bucket in this bar was below the sample threshold.
    const minSample = bucket.health.minimum_sample ?? 50
    if (bucket.metrics.request_count < minSample) bar.lowSample = true
  }
  for (const bar of bars) bar.buckets.sort((a, b) => Date.parse(a.bucket_start) - Date.parse(b.bucket_start))
  return bars
}

export function candyDisplayState(history: MonitorCandyHistory, now: number): 'correct' | 'incorrect' | 'error' | 'unknown' | 'stale' {
  const latest = history.results.at(-1)
  if (!latest) return 'unknown'
  const age = now - Date.parse(latest.checked_at)
  if (!Number.isFinite(age) || age > Math.max(180, history.interval_minutes * 60 + 120) * 1000) return 'stale'
  return latest.verdict
}
