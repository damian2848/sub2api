import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { formatDateTime } from '@/utils/format'
import enAccounts from '@/i18n/locales/en/admin/accounts'
import zhAccounts from '@/i18n/locales/zh/admin/accounts'
import enCommon from '@/i18n/locales/en/common'
import zhCommon from '@/i18n/locales/zh/common'
import type { GrokModelDiagnostics } from '@/types'

const { tMock } = vi.hoisted(() => ({
  tMock: vi.fn((key: string) => key)
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => tMock(key)
    })
  }
})

import GrokModelDiagnosticsPanel from '../GrokModelDiagnosticsPanel.vue'

const NOT_LISTED_ZH = '未在该账号此端点的当前目录中发现'
const SAVED_CONFIG_ZH = '诊断基于已保存配置，请保存后重新同步'
const REQUESTS_NOT_BLOCKED_ZH = '请求不会被阻断'
const REQUIRED_KEYS = [
  'title',
  'configuredModels',
  'upstreamModels',
  'lastSuccess',
  'lastAttempt',
  'fresh',
  'stale',
  'unknown',
  'sourceChanged',
  'notListed',
  'unverifiable',
  'outOfScope',
  'compatibilityNotice',
  'configuredFallback',
  'savedConfigOnly',
  'refreshFailed',
  'syncSavedConfig'
] as const

const START = Date.parse('2026-10-25T12:00:00.000Z')
const FETCHED_AT = '2026-10-25T11:00:00.000Z'
const EXPIRES_AT = '2026-10-25T12:00:05.000Z'
const LONG_ENDPOINT = `https://relay.example.com/${'endpoint-segment-'.repeat(12)}v1/models`
const LONG_MODEL = `grok-${'very-long-model-id-'.repeat(8)}custom`

let locale: 'en' | 'zh' = 'zh'

function lookup(key: string): string {
  const root = locale === 'zh'
    ? { ...zhCommon, admin: zhAccounts }
    : { ...enCommon, admin: enAccounts }
  let current: unknown = root
  for (const segment of key.split('.')) {
    if (!current || typeof current !== 'object') return key
    current = (current as Record<string, unknown>)[segment]
  }
  return typeof current === 'string' ? current : key
}

function mountPanel(
  props: Record<string, unknown> = {},
  nextLocale: 'en' | 'zh' = 'zh'
) {
  locale = nextLocale
  tMock.mockImplementation(lookup)
  return mount(GrokModelDiagnosticsPanel, {
    props: {
      diagnostics: null,
      ...props
    }
  })
}

function freshDiagnostics(overrides: Partial<GrokModelDiagnostics> = {}): GrokModelDiagnostics {
  return {
    state: 'fresh',
    endpoint: 'https://cli-chat-proxy.grok.com/v1/models',
    fetched_at: FETCHED_AT,
    expires_at: EXPIRES_AT,
    models: ['grok-4.5', 'grok-4.6', 'grok-4.7', 'grok-4.7-build'],
    last_attempt: {
      started_at: FETCHED_AT,
      finished_at: '2026-10-25T11:00:01.000Z',
      status: 'success',
      scope: {
        account_id: 25,
        account_type: 'oauth',
        endpoint: 'https://cli-chat-proxy.grok.com/v1/models',
        fingerprint: 'abc'
      }
    },
    mappings: [
      { requested: 'composer-2.5', upstream: 'grok-composer-2.5-fast', status: 'not_listed' },
      { requested: 'friendly', upstream: 'grok-4.7', status: 'listed' },
      { requested: 'custom-*', status: 'unverifiable' },
      { requested: 'grok-imagine-image', upstream: 'grok-imagine-image', status: 'out_of_scope' }
    ],
    ...overrides
  }
}

const mounted: VueWrapper[] = []

function track(wrapper: VueWrapper) {
  mounted.push(wrapper)
  return wrapper
}

describe('GrokModelDiagnosticsPanel', () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ['Date', 'setTimeout', 'clearTimeout', 'setInterval', 'clearInterval'] })
    vi.setSystemTime(START)
  })

  afterEach(() => {
    for (const wrapper of mounted.splice(0)) wrapper.unmount()
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('keeps English and Chinese diagnostic copy isomorphic and non-empty', () => {
    const en = (enAccounts as { accounts: { grokModelDiagnostics?: Record<string, string> } }).accounts.grokModelDiagnostics
    const zh = (zhAccounts as { accounts: { grokModelDiagnostics?: Record<string, string> } }).accounts.grokModelDiagnostics
    expect(en).toBeTruthy()
    expect(zh).toBeTruthy()
    expect(Object.keys(en!).sort()).toEqual(Object.keys(zh!).sort())
    for (const key of REQUIRED_KEYS) {
      expect(en![key]?.trim()).toBeTruthy()
      expect(zh![key]?.trim()).toBeTruthy()
    }
    expect(zh!.notListed).toBe(NOT_LISTED_ZH)
    expect(zh!.savedConfigOnly).toBe(SAVED_CONFIG_ZH)
    expect(zh!.compatibilityNotice).toBe(REQUESTS_NOT_BLOCKED_ZH)
    expect(zhCommon.common.loading.trim()).toBeTruthy()
    expect(enCommon.common.loading.trim()).toBeTruthy()
    expect(zhCommon.common.refresh.trim()).toBeTruthy()
    expect(enCommon.common.refresh.trim()).toBeTruthy()
  })

  it('shows source, last success, the latest attempt, and mapping differences for a fresh catalog', () => {
    const wrapper = track(mountPanel({ diagnostics: freshDiagnostics() }))
    const text = wrapper.text()
    expect(text).toContain('https://cli-chat-proxy.grok.com/v1/models')
    expect(text).toContain(formatDateTime(FETCHED_AT))
    expect(text).toContain('最后成功')
    expect(text).toContain('最近尝试')
    expect(text).toContain('success')
    expect(text).toContain('当前有效')
    expect(text).toContain('grok-4.5')
    expect(text).toContain('grok-4.6')
    expect(text).toContain('grok-4.7')
    expect(text).toContain('grok-4.7-build')
    expect(text).toContain('composer-2.5')
    expect(text).toContain('grok-composer-2.5-fast')
    expect(text).toContain(NOT_LISTED_ZH)
    expect(text).toContain(REQUESTS_NOT_BLOCKED_ZH)
    expect(text).toContain('friendly')
    expect(text).toContain('custom-*')
    expect(text).toContain('无法逐项核验')
    expect(text).toContain('grok-imagine-image')
    expect(text).toContain('不在此文本目录范围内')
    expect(text).toContain('本地配置')
    expect(text).toContain('上游目录')
  })

  it('renders the same sections in English', () => {
    const wrapper = track(mountPanel({ diagnostics: freshDiagnostics() }, 'en'))
    const text = wrapper.text()
    const copy = (enAccounts as { accounts: { grokModelDiagnostics: Record<string, string> } }).accounts.grokModelDiagnostics
    expect(text).toContain(copy.title)
    expect(text).toContain(copy.fresh)
    expect(text).toContain(copy.notListed)
    expect(text).toContain(copy.compatibilityNotice)
    expect(text).toContain(copy.unverifiable)
    expect(text).toContain(copy.outOfScope)
    expect(text).toContain(copy.lastSuccess)
    expect(text).toContain(copy.lastAttempt)
  })

  it('becomes stale at ExpiresAt and stops claiming current catalog gaps without a request', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(START)
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response())
    const wrapper = track(mountPanel({ diagnostics: freshDiagnostics() }))
    expect(wrapper.text()).toContain(NOT_LISTED_ZH)
    expect(wrapper.text()).toContain('当前有效')

    await vi.advanceTimersByTimeAsync(5000)

    const text = wrapper.text()
    expect(text).toContain('历史目录')
    expect(text).not.toContain('当前有效')
    expect(text).not.toContain(NOT_LISTED_ZH)
    expect(text).not.toContain(REQUESTS_NOT_BLOCKED_ZH)
    expect(text).toContain('composer-2.5')
    expect(text).toContain(formatDateTime(FETCHED_AT))
    expect(fetchSpy).not.toHaveBeenCalled()
  })

  it('shows unknown for invalid timestamps instead of parsing them in local time', () => {
    const invalid = '2026-10-25 12:00:00'
    const wrapper = track(mountPanel({
      diagnostics: freshDiagnostics({
        state: 'fresh',
        fetched_at: invalid,
        expires_at: 'tomorrow',
        mappings: [
          { requested: 'composer-2.5', upstream: 'grok-composer-2.5-fast', status: 'not_listed' }
        ]
      })
    }))
    const text = wrapper.text()
    expect(text).toContain('未知')
    expect(text).not.toContain(invalid)
    expect(text).not.toContain('tomorrow')
    expect(text).not.toContain(NOT_LISTED_ZH)
    expect(text).not.toContain('当前有效')
  })

  it('does not present a future fetched_at echo as a successful observation', () => {
    vi.useFakeTimers()
    vi.setSystemTime(START)
    const future = '2026-10-25T13:00:00.000Z'
    const wrapper = track(mountPanel({
      diagnostics: freshDiagnostics({
        state: 'unknown',
        fetched_at: future,
        expires_at: undefined,
        models: ['grok-4.7'],
        mappings: [
          { requested: 'friendly', upstream: 'grok-4.7', status: 'listed' },
          { requested: 'composer-2.5', upstream: 'grok-composer-2.5-fast', status: 'not_listed' }
        ]
      })
    }))
    const text = wrapper.text()
    expect(text).toContain('未知')
    expect(text).not.toContain(formatDateTime(future))
    expect(text).not.toContain(NOT_LISTED_ZH)
    expect(text).not.toContain('当前有效')
    expect(text).toContain('grok-4.7')
  })

  it('treats listed rows in a stale catalog as history rather than a current result', () => {
    const wrapper = track(mountPanel({
      diagnostics: freshDiagnostics({
        state: 'stale',
        fetched_at: '2026-10-25T05:00:00.000Z',
        expires_at: '2026-10-25T11:00:00.000Z',
        mappings: [
          { requested: 'friendly', upstream: 'grok-4.7', status: 'listed' },
          { requested: 'composer-2.5', upstream: 'grok-composer-2.5-fast', status: 'not_listed' }
        ]
      })
    }))
    const text = wrapper.text()
    expect(text).toContain('历史目录')
    expect(text).toContain('friendly')
    expect(text).toContain('grok-4.7')
    expect(text).not.toContain(NOT_LISTED_ZH)
    expect(text).not.toContain('当前有效')
  })

  it('shows a source change without borrowing the previous catalog as current', () => {
    const wrapper = track(mountPanel({
      diagnostics: freshDiagnostics({
        state: 'source_changed',
        endpoint: LONG_ENDPOINT,
        mappings: [
          { requested: 'composer-2.5', upstream: 'grok-composer-2.5-fast', status: 'not_listed' }
        ]
      })
    }))
    const text = wrapper.text()
    expect(text).toContain('来源已变更')
    expect(text).toContain(LONG_ENDPOINT)
    expect(text).not.toContain(NOT_LISTED_ZH)
    expect(wrapper.get('[data-testid="grok-diagnostics-endpoint"]').classes()).toContain('break-all')
  })

  it('shows the latest failure together with the previous successful catalog', () => {
    const wrapper = track(mountPanel({
      diagnostics: freshDiagnostics({
        last_attempt: {
          started_at: '2026-10-25T11:50:00.000Z',
          finished_at: '2026-10-25T11:50:02.000Z',
          status: 'failed',
          error_code: 'upstream_unavailable'
        }
      })
    }))
    const text = wrapper.text()
    expect(text).toContain('当前有效')
    expect(text).toContain(formatDateTime(FETCHED_AT))
    expect(text).toContain('failed')
    expect(text).toContain('upstream_unavailable')
    expect(text).toContain(formatDateTime('2026-10-25T11:50:02.000Z'))
    expect(text).toContain(NOT_LISTED_ZH)
  })

  it('keeps the historical catalog visible when refreshing the latest status fails', () => {
    const wrapper = track(mountPanel({
      diagnostics: freshDiagnostics(),
      refreshFailed: true
    }))
    const text = wrapper.text()
    expect(text).toContain('无法获取最新状态')
    expect(text).toContain('grok-4.7')
    expect(text).toContain(NOT_LISTED_ZH)
    expect(text).toContain('当前有效')
  })

  it('warns that unsaved edits are not part of the diagnosis without hiding gaps', () => {
    const wrapper = track(mountPanel({
      diagnostics: freshDiagnostics(),
      dirty: true
    }))
    const text = wrapper.text()
    expect(text).toContain(SAVED_CONFIG_ZH)
    expect(text).toContain(NOT_LISTED_ZH)
    expect(text).toContain('当前有效')
  })

  it('shows unknown when no diagnostics are available and does not offer sync by default', () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response())
    const wrapper = track(mountPanel())
    expect(wrapper.text()).toContain('未知')
    expect(wrapper.find('[data-testid="grok-diagnostics-sync"]').exists()).toBe(false)
    expect(fetchSpy).not.toHaveBeenCalled()
  })

  it('emits sync for a saved-config refresh and ignores clicks while syncing', async () => {
    const wrapper = track(mountPanel({ diagnostics: freshDiagnostics(), canSync: true }))
    const button = wrapper.get('[data-testid="grok-diagnostics-sync"]')
    expect(button.text()).toContain('同步已保存配置的上游目录')
    await button.trigger('click')
    expect(wrapper.emitted('sync')).toEqual([[]])

    await wrapper.setProps({ syncing: true })
    await wrapper.get('[data-testid="grok-diagnostics-sync"]').trigger('click')
    expect(wrapper.emitted('sync')).toEqual([[]])
    expect(wrapper.get('[data-testid="grok-diagnostics-sync"]').attributes('disabled')).toBeDefined()
  })

  it('shows the existing loading copy without hiding the catalog', () => {
    const wrapper = track(mountPanel({ diagnostics: freshDiagnostics(), loading: true }))
    expect(wrapper.text()).toContain(zhCommon.common.loading)
    expect(wrapper.text()).toContain('grok-4.7')
  })

  it('wraps long model ids and stays inside a narrow dark layout', () => {
    const wrapper = track(mountPanel({
      diagnostics: freshDiagnostics({
        endpoint: LONG_ENDPOINT,
        models: [LONG_MODEL],
        mappings: [{ requested: LONG_MODEL, upstream: LONG_MODEL, status: 'listed' }]
      })
    }))
    const root = wrapper.get('[data-testid="grok-model-diagnostics"]')
    expect(root.classes()).toEqual(expect.arrayContaining(['max-w-full', 'overflow-hidden', 'min-w-0']))
    expect(root.classes().some(className => className.startsWith('dark:'))).toBe(true)
    expect(wrapper.get('[data-testid="grok-diagnostics-endpoint"]').classes()).toContain('break-all')
    const model = wrapper.get('[data-testid="grok-diagnostics-model"]')
    expect(model.classes()).toContain('break-all')
    expect(model.text()).toContain(LONG_MODEL)
    expect(wrapper.findAll('details').length).toBeGreaterThanOrEqual(2)
  })
})
