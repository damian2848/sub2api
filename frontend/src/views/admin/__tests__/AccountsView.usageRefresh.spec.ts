import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, shallowMount } from '@vue/test-utils'
import AccountsView from '../AccountsView.vue'
import type { AccountUsageInfo } from '@/types'

enableAutoUnmount(afterEach)

const { getBatchUsage } = vi.hoisted(() => ({ getBatchUsage: vi.fn() }))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      getManagementCapabilities: vi.fn().mockResolvedValue({}),
      list: vi.fn().mockResolvedValue({
        items: [{ id: 12, name: 'Gemini Web', platform: 'gemini', type: 'gemini_web', status: 'active' }],
        total: 1, page: 1, page_size: 20, pages: 1
      }),
      listWithEtag: vi.fn().mockResolvedValue({ notModified: true, etag: null, data: null }),
      getBatchTodayStats: vi.fn().mockResolvedValue({ stats: {} }),
      getUpstreamBillingProbeSettings: vi.fn().mockResolvedValue({ enabled: true, interval_minutes: 30 }),
      getBatchUsage
    },
    proxies: { getAll: vi.fn().mockResolvedValue([]) },
    groups: { getAll: vi.fn().mockResolvedValue([]) }
  }
}))
vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn(), showInfo: vi.fn() })
}))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ token: 'test-token', isSimpleMode: false }) }))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key })
}))

const UsageCellStub = {
  props: ['account', 'batchedUsage', 'batchedUsageLoading', 'batchedUsageError', 'requestBatchedUsage'],
  emits: ['usage-loaded'],
  template: '<div />'
}

const makeUsage = (utilization: number): AccountUsageInfo => ({
  updated_at: '2026-10-17T00:00:00Z',
  five_hour: { utilization, resets_at: null, remaining_seconds: 0 },
  seven_day: null,
  seven_day_sonnet: null
})

const mountView = () => shallowMount(AccountsView, {
  global: {
    stubs: {
      AppLayout: { template: '<div><slot /></div>' },
      TablePageLayout: { template: '<div><slot name="table" /></div>' },
      DataTable: {
        props: ['data'],
        template: '<div><div v-for="row in data" :key="row.id"><slot name="cell-usage" :row="row" /></div></div>'
      },
      AccountUsageCell: UsageCellStub
    }
  }
})

describe('AccountsView active usage refresh', () => {
  beforeEach(() => {
    localStorage.clear()
    getBatchUsage.mockReset()
    vi.useFakeTimers()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('reuses the active result when the row requests its cached usage again', async () => {
    const oldUsage = makeUsage(10)
    const freshUsage = makeUsage(50)
    getBatchUsage.mockResolvedValue({ usage: { 12: oldUsage } })
    const wrapper = mountView()
    await flushPromises()
    const cell = wrapper.getComponent(UsageCellStub)

    cell.props('requestBatchedUsage')(cell.props('account'))
    await vi.advanceTimersByTimeAsync(1)
    expect(cell.props('batchedUsage')).toEqual(oldUsage)

    cell.vm.$emit('usage-loaded', freshUsage)
    await flushPromises()
    cell.props('requestBatchedUsage')(cell.props('account'))
    await vi.advanceTimersByTimeAsync(1)

    expect(cell.props('batchedUsage')).toEqual(freshUsage)
    expect(cell.props('batchedUsageLoading')).toBe(false)
    expect(getBatchUsage).toHaveBeenCalledTimes(1)
  })

  it('ignores an older in-flight batch response after a cell finishes an active query', async () => {
    const freshUsage = makeUsage(60)
    let resolveBatch!: (result: { usage: Record<string, AccountUsageInfo> }) => void
    getBatchUsage.mockImplementationOnce(() => new Promise(resolve => { resolveBatch = resolve }))
    const wrapper = mountView()
    await flushPromises()
    const cell = wrapper.getComponent(UsageCellStub)

    cell.props('requestBatchedUsage')(cell.props('account'), { force: true })
    await vi.advanceTimersByTimeAsync(1)
    expect(getBatchUsage).toHaveBeenCalledWith([12], true)
    expect(cell.props('batchedUsageLoading')).toBe(true)
    cell.vm.$emit('usage-loaded', freshUsage)
    await flushPromises()
    expect(cell.props('batchedUsageLoading')).toBe(false)

    resolveBatch({ usage: { 12: makeUsage(5) } })
    await flushPromises()
    expect(cell.props('batchedUsage')).toEqual(freshUsage)
    expect(cell.props('batchedUsageError')).toBeNull()
  })
})
