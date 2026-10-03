import type { HealthState, MonitorCandyHistory, MonitorCandyResult, MonitorCoverage, MonitorMatrixBucket, MonitorMatrixRow, MonitorMetric } from '@/api/channelMonitorV2'

export const CANDY_HISTORY_LIMIT = 100

export function candyHistorySlots(history: MonitorCandyHistory): Array<MonitorCandyResult | null> {
  const results = history.results.slice(-CANDY_HISTORY_LIMIT)
  return [...Array<null>(CANDY_HISTORY_LIMIT - results.length).fill(null), ...results]
}

export function hasMonitorSamples(metric: MonitorMetric): boolean {
  return metric.has_samples === true || metric.request_count > 0
}

// Business availability takes precedence. Probe counts may be redacted, so the
// explicit rate (including zero) is the signal, never a zero error-rate default.
export function effectiveAvailability(metric: MonitorMetric): number | null {
  const value = hasMonitorSamples(metric) ? 1 - metric.error_rate : metric.probe_availability
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 && value <= 1 ? value : null
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

// Keep real time gaps. Each bar summarizes observed health in one of 18 equal
// time windows, never interpolating a successful request into missing history.
export function monitorCardTimeline(row: MonitorMatrixRow, coverage: MonitorCoverage | undefined) {
  const start = Date.parse(coverage?.requested_start || '')
  const end = Date.parse(coverage?.requested_end || coverage?.data_through || '')
  if (!Number.isFinite(start) || !Number.isFinite(end) || end <= start) return []
  const width = (end - start) / 18
  const bars = Array.from({ length: 18 }, (_, index) => ({ start: start + index * width, end: start + (index + 1) * width, state: 'unknown' as HealthState, observed: false, buckets: [] as MonitorMatrixBucket[] }))
  const severity: Record<HealthState, number> = { unknown: 0, healthy: 1, warning: 2, critical: 3 }
  for (const bucket of row.buckets) {
    const at = Date.parse(bucket.bucket_start)
    if (at < start || at >= end || !hasMonitorSamples(bucket.metrics)) continue
    const bar = bars[Math.floor((at - start) / width)]
    if (!bar) continue
    bar.observed = true
    // Keep the original bucket metrics: redacted counts cannot weight rates,
    // and bucket medians cannot be combined into a time-window P50.
    bar.buckets.push(bucket)
    if (severity[bucket.health.overall] > severity[bar.state]) bar.state = bucket.health.overall
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
