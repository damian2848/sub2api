import { flushPromises, shallowMount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ref } from 'vue'
import type { MonitorMetric } from '@/api/channelMonitorV2'
import MetricCell from '@/features/channel-monitor-v2/MetricCell.vue'
import ChannelStatusV2View from '../ChannelStatusV2View.vue'

const api = vi.hoisted(() => ({ getDimensions: vi.fn(), getSnapshot: vi.fn(), getMatrix: vi.fn(), getObservations: vi.fn(), getModels: vi.fn(), getErrors: vi.fn(), getUsers: vi.fn() }))
const showError = vi.hoisted(() => vi.fn())
vi.mock('@/api/channelMonitorV2', () => api)
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ isAdmin: false }) }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError }) }))
vi.mock('@/utils/featureFlags', () => ({ isChannelMonitorThroughputHidden: () => true, isChannelMonitorUserRankingHidden: () => false }))
vi.mock('vue-router', () => ({ useRoute: () => ({ query: {} }), useRouter: () => ({ replace: vi.fn() }) }))
vi.mock('vue-i18n', async (importOriginal) => ({ ...await importOriginal<typeof import('vue-i18n')>(), useI18n: () => ({ locale: ref('en'), t: (key: string) => key, te: () => false }) }))

const coverage = { requested_start: '2026-10-03T00:00:00Z', requested_end: '2026-10-03T01:30:00Z', coverage_start: '2026-10-03T00:00:00Z', data_through: '2026-10-03T01:30:00Z', computed_at: '2026-10-03T01:30:00Z', aggregation_lag_seconds: 0, coverage_complete: true, bucket_seconds: 300 }
const health = { overall: 'unknown', error_rate: 'unknown', ttft: 'unknown', cache: 'unknown', minimum_sample: 50 }
const emptyMetric = (): MonitorMetric => ({ has_samples: false, request_count: 0, success_requests: 0, error_requests: 0, token_count: 0, rpm: 0, tpm: 0, error_rate: 0, cache_rate: 0, cache_rate_numerator: 0, cache_rate_denominator: 0, ttft: { sample_count: 0, p50_ms: null, p95_ms: null, avg_ms: null }, duration: { sample_count: 0, p50_ms: null, p95_ms: null, avg_ms: null } })
const mounted: ReturnType<typeof shallowMount>[] = []

beforeEach(() => {
  vi.clearAllMocks()
  api.getDimensions.mockResolvedValue({ platforms: [], groups: [], models: [] })
  api.getMatrix.mockResolvedValue({ group_by: 'platform_group', coverage, items: [] })
  api.getObservations.mockResolvedValue(null)
  api.getErrors.mockResolvedValue({ items: [] })
  api.getUsers.mockResolvedValue({ items: [{ display_label: 'Me', is_self: true, rank: 0, metrics: emptyMetric() }] })
})
afterEach(() => { for (const wrapper of mounted.splice(0)) wrapper.unmount() })

async function mountView(metric: MonitorMetric) {
  api.getSnapshot.mockResolvedValue({ metrics: metric, health, coverage, config: { refresh_interval_seconds: 60, platforms: [] }, trend: [] })
  api.getModels.mockResolvedValue({ items: [{ platform: 'openai', model: 'probe-only', metrics: metric, health }] })
  const wrapper = shallowMount(ChannelStatusV2View, { global: { stubs: { AppLayout: { template: '<div><slot /></div>' }, MetricCell: false } } })
  mounted.push(wrapper)
  await flushPromises()
  return wrapper
}

describe('channel status availability', () => {
  it.each([1, 0.75, 0])('shows probe availability %s in the card summary, analytics KPI and model table', async (rate) => {
    const metric = { ...emptyMetric(), has_samples: true, error_rate: 1 - rate, availability_source: 'probe' as const }
    const expected = rate === 0 ? '0.00%' : `${(rate * 100).toFixed(1)}%`
    const wrapper = await mountView(metric)
    expect(wrapper.text()).toContain(expected)
    expect(wrapper.text()).toContain('channelMonitorV2.sources.probe')
    await wrapper.get('[data-testid="monitor-layout-toggle"]').trigger('click')
    await flushPromises()
    const cells = wrapper.findAllComponents(MetricCell)
    expect(cells[0].props('value')).toBe(expected)
    expect(cells[0].props('detail')).toContain('channelMonitorV2.sources.probe')
    expect(cells[2].props('value')).toBe('0.00%')
    expect(wrapper.get('tbody tr td:nth-child(2)').text()).toContain(expected)
    expect(wrapper.get('tbody tr td:nth-child(2)').text()).toContain('channelMonitorV2.sources.probe')
    expect(showError).not.toHaveBeenCalled()
  })

  it('keeps pending availability unknown in the KPI, model table and unranked user row', async () => {
    const wrapper = await mountView(emptyMetric())
    expect(wrapper.text()).not.toContain('100.0%')
    await wrapper.get('[data-testid="monitor-layout-toggle"]').trigger('click')
    await flushPromises()
    expect(wrapper.findAllComponents(MetricCell)[0].props('value')).toBe('—')
    expect(wrapper.get('tbody tr td:nth-child(2)').text()).toContain('—')
    const usersTab = wrapper.findAll('[role="tab"]').find(tab => tab.text() === 'channelMonitorV2.tabs.users')!
    await usersTab.trigger('click')
    await flushPromises()
    expect(wrapper.get('tbody tr td:nth-child(3)').text()).toContain('—')
    expect(wrapper.text()).not.toContain('100.0%')
    expect(showError).not.toHaveBeenCalled()
  })

  it('prefers privacy-redacted business samples over probe results', async () => {
    const wrapper = await mountView({ ...emptyMetric(), has_samples: true, error_rate: 0.2, probe_availability: 1, availability_source: 'business' })
    expect(wrapper.text()).toContain('80.0%')
    expect(wrapper.text()).toContain('80.0% · channelMonitorV2.sources.business')
    await wrapper.get('[data-testid="monitor-layout-toggle"]').trigger('click')
    await flushPromises()
    expect(wrapper.findAllComponents(MetricCell)[0].props('value')).toBe('80.0%')
    expect(wrapper.get('tbody tr td:nth-child(2)').text()).toContain('80.0%')
  })
})

it('reloads channel metrics with source filtering while defaulting to all requests', async () => {
 const wrapper = await mountView(emptyMetric())
 expect(api.getSnapshot.mock.calls.at(-1)?.[0].source).toBe('all')
 await wrapper.get('[data-testid="monitor-request-source"]').setValue('probe')
 await flushPromises()
 expect(api.getSnapshot.mock.calls.at(-1)?.[0].source).toBe('probe')
 expect(api.getMatrix.mock.calls.at(-1)?.[0].source).toBe('probe')
})

it('does not trust legacy observation-derived rates without actual request samples', async () => {
 const wrapper = await mountView({ ...emptyMetric(), probe_availability: 1, availability_source: 'probe' })
 expect(wrapper.text()).not.toContain('100.0%')
 await wrapper.get('[data-testid="monitor-layout-toggle"]').trigger('click')
 await flushPromises()
 expect(wrapper.findAllComponents(MetricCell)[0].props('value')).toBe('—')
})
