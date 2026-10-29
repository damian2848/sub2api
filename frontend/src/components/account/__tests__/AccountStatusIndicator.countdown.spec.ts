import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { nextTick } from 'vue'
import { i18n } from '@/i18n'
import enCommon from '@/i18n/locales/en/common'
import enAccounts from '@/i18n/locales/en/admin/accounts'
import { compileMessages } from '@/__tests__/compileMessages'
import type { Account } from '@/types'
import AccountStatusIndicator from '../AccountStatusIndicator.vue'

const startedAt = Date.parse('2026-10-20T00:00:00Z')
const deadline = (seconds: number) => new Date(startedAt + seconds * 1000).toISOString()
const wrappers: VueWrapper[] = []

function makeAccount(overrides: Partial<Account> = {}): Account {
  return {
    id: 1,
    name: 'account',
    platform: 'antigravity',
    type: 'oauth',
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    status: 'active',
    error_message: null,
    last_used_at: null,
    expires_at: null,
    auto_pause_on_expired: true,
    created_at: deadline(0),
    updated_at: deadline(0),
    schedulable: true,
    rate_limited_at: null,
    rate_limit_reset_at: null,
    overload_until: null,
    temp_unschedulable_until: null,
    temp_unschedulable_reason: null,
    session_window_start: null,
    session_window_end: null,
    session_window_status: null,
    ...overrides,
  }
}

function render(account: Account) {
  const wrapper = mount(AccountStatusIndicator, {
    props: { account },
    global: {
      plugins: [i18n],
      stubs: {
        Icon: true,
        ExcelBPSCredentialStatus: true,
        HelpTooltip: { template: '<span><slot name="trigger" /></span>' },
      },
    },
  })
  wrappers.push(wrapper)
  return wrapper
}

beforeAll(() => {
  i18n.global.setLocaleMessage('en', compileMessages({ ...enCommon, admin: enAccounts }))
  i18n.global.locale.value = 'en'
})

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(startedAt)
})

afterEach(() => {
  for (const wrapper of wrappers.splice(0)) wrapper.unmount()
  vi.useRealTimers()
})

describe('AccountStatusIndicator cooldown clock', () => {
  it('counts down and removes a model cooldown without replacing or mutating the account', async () => {
    const account = makeAccount({
      extra: {
        model_rate_limits: {
          gemini_text: { rate_limited_at: deadline(0), rate_limit_reset_at: deadline(59) },
        },
      },
    })
    const snapshot = JSON.stringify(account)
    const wrapper = render(account)
    const renderedAccount = wrapper.props('account')
    expect(wrapper.text()).toContain('Gemini 59s')

    await vi.advanceTimersByTimeAsync(1_000)
    expect(wrapper.text()).toContain('Gemini 58s')
    await vi.advanceTimersByTimeAsync(57_000)
    expect(wrapper.text()).toContain('Gemini 1s')
    await vi.advanceTimersByTimeAsync(1_000)
    expect(wrapper.text()).not.toContain('Gemini')
    expect(wrapper.props('account')).toBe(renderedAccount)
    expect(JSON.stringify(account)).toBe(snapshot)
  })

  it.each([
    ['rate_limit_reset_at', 'rateLimited'],
    ['overload_until', 'overloaded'],
    ['temp_unschedulable_until', 'tempUnschedulable'],
  ] as const)('clears %s at its deadline without a server refresh', async (field, statusKey) => {
    const wrapper = render(makeAccount({ [field]: deadline(2) }))
    const label = i18n.global.t(`admin.accounts.status.${statusKey}`)
    expect(wrapper.text()).toContain(label)
    if (field !== 'temp_unschedulable_until') expect(wrapper.text()).toContain('2s')

    await vi.advanceTimersByTimeAsync(2_000)
    expect(wrapper.text()).not.toContain(label)
    expect(wrapper.text()).toContain(i18n.global.t('admin.accounts.status.active'))
  })

  it('updates credit overage availability when the credit cooldown expires', async () => {
    const wrapper = render(makeAccount({
      extra: {
        allow_overages: true,
        model_rate_limits: {
          gemini_text: { rate_limited_at: deadline(0), rate_limit_reset_at: deadline(4) },
          AICredits: { rate_limited_at: deadline(0), rate_limit_reset_at: deadline(2) },
        },
      },
    }))
    expect(wrapper.text()).toContain(i18n.global.t('admin.accounts.status.creditsExhausted'))
    expect(wrapper.text()).not.toContain('⚡')

    await vi.advanceTimersByTimeAsync(2_000)
    expect(wrapper.text()).not.toContain(i18n.global.t('admin.accounts.status.creditsExhausted'))
    expect(wrapper.text()).toContain('⚡ Gemini 2s')
    await vi.advanceTimersByTimeAsync(2_000)
    expect(wrapper.text()).not.toContain('Gemini')
  })

  it('shares one timer across rows and stops it when the last row unmounts', async () => {
    const first = render(makeAccount({ rate_limit_reset_at: deadline(10) }))
    const second = render(makeAccount({ id: 2, overload_until: deadline(10) }))
    await nextTick()
    expect(vi.getTimerCount()).toBe(1)

    first.unmount()
    expect(vi.getTimerCount()).toBe(1)
    await vi.advanceTimersByTimeAsync(1_000)
    expect(second.text()).toContain('9s')
    second.unmount()
    expect(vi.getTimerCount()).toBe(0)
    wrappers.splice(0)

    // Reopening the page gets the current time, not the disposed clock's value.
    vi.setSystemTime(startedAt + 11_000)
    const reopened = render(makeAccount({ rate_limit_reset_at: deadline(10) }))
    expect(reopened.text()).not.toContain(i18n.global.t('admin.accounts.status.rateLimited'))
    expect(vi.getTimerCount()).toBe(1)
  })
})
