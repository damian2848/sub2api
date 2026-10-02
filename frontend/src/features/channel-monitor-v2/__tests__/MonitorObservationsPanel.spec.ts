import { mount } from '@vue/test-utils'
import { ref } from 'vue'
import { describe, expect, it, vi } from 'vitest'
import type { MonitorMetric, MonitorObservationRow, MonitorObservations, ProbeUsage } from '@/api/channelMonitorV2'
import en from '@/i18n/locales/en/channelMonitorV2'
import MonitorObservationsPanel from '../MonitorObservationsPanel.vue'

function message(key: string): string {
  let value: unknown = en
  for (const part of key.split('.')) value = value && typeof value === 'object' ? (value as Record<string, unknown>)[part] : undefined
  return typeof value === 'string' ? value : key
}
vi.mock('vue-i18n', async (importOriginal) => ({ ...await importOriginal<typeof import('vue-i18n')>(), useI18n: () => ({
  locale: ref('en'), te: (key: string) => message(key) !== key,
  t: (key: string, params: Record<string, unknown> = {}) => message(key).replace(/\{(\w+)\}/g, (_, name: string) => String(params[name] ?? '')),
}) }))

const usage: ProbeUsage = { source: 'probe', request_count: 2, input_tokens: 10, output_tokens: 20, cache_read_tokens: 30, cache_creation_tokens: 40, cache_creation_5m_tokens: 40, cache_creation_1h_tokens: 0, reasoning_tokens: 15, cost_usd: 0.01, cost_incomplete: false, usage_incomplete: false }
const observation = (type: MonitorObservationRow['type'], verdict: string): MonitorObservationRow => ({
  id: type, type, name: type, platform: 'openai', group_id: 7, group_name: 'Group A', model: 'model-a', enabled: true, interval_seconds: 60, verdict,
  checked_at: '2026-10-02T00:00:00Z', sample_count: 2, passed_count: 0, failed_count: 1, inconclusive_count: 1, usage,
  history: [{ checked_at: '2026-10-01T00:00:00Z', verdict: 'healthy' }, { checked_at: '2026-10-02T00:00:00Z', verdict, message: 'Private diagnostic detail' }],
})
const data: MonitorObservations = {
  range: '24h', start: '2026-10-01T00:00:00Z', end: '2026-10-02T00:00:00Z', computed_at: '2026-10-02T00:00:00Z', group_by: 'platform_group_model',
  items: [observation('connectivity', 'failed'), observation('candy', 'incorrect'), observation('state_probe', 'inconclusive')],
  summary: { probe_count: 6, passed_count: 0, failed_count: 3, inconclusive_count: 3, usage },
  business_usage: { request_count: 123, token_count: 98765 } as MonitorMetric,
  total_tokens: 98865,
}
const mountPanel = (exactBusiness = true, showUsage = true) => mount(MonitorObservationsPanel, {
  props: { data, loading: false, exactBusiness, showUsage },
})

describe('unified monitor observations', () => {
  it('separates connection failures, logic errors, and inconclusive state probes', () => {
    const wrapper = mountPanel()
    const rows = wrapper.findAll('[data-testid="monitor-observation-row"]')
    expect(rows[0].text()).toContain('Connection failed')
    expect(rows[1].text()).toContain('Wrong answer')
    expect(rows[2].text()).toContain('Inconclusive')
    expect(wrapper.text()).toContain('2 requests · 100 tokens')
    expect(wrapper.text()).toContain('98,865')
    expect(wrapper.text()).toContain('Private diagnostic detail')
    const points = rows[2].findAll('ol li')
    expect(points[0].text()).toContain('Inconclusive')
    expect(points[1].text()).toContain('Healthy')
  })
  it('masks unavailable exact counts and hidden probe usage', () => {
    const wrapper = mountPanel(false, false)
    expect(wrapper.text()).toContain('- requests · - tokens')
    expect(wrapper.text()).not.toContain('98,765')
    expect(wrapper.text()).not.toContain('98,865')
    expect(wrapper.text()).not.toContain('100 tokens')
    expect(wrapper.text()).not.toContain('$0.01')
    expect(wrapper.text()).not.toContain('Private diagnostic detail')
    expect(wrapper.text()).toContain('Wrong answer')
  })
})
