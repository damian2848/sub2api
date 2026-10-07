import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import RuleList from '../AccountOpsRuleList.vue'
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
describe('saved threshold currency', () => {
  it('shows the rule unit separately from a changed sample unit', () => {
    const wrapper = mount(RuleList, { props: { accounts: [{ account_id: 1, account_name: 'key', platform: 'openai', type: 'apikey', balance: 36, unit: 'CNY', balance_status: 'ok', received_at: null, usage_windows: [] }], config: { enabled: false, recipient: '', balance_low: true, weekly_quota: true, cooldown_minutes: 60, balance_thresholds: [{ account_id: 1, enabled: true, threshold: 5, unit: 'USD' }] }, loading: false, ready: true, error: '' } })
    expect(wrapper.text()).toContain('36.00 CNY')
    expect(wrapper.text()).toContain('≤ 5.00 USD')
    expect(wrapper.text()).toContain('accountOps.ruleStates.unknown')
    wrapper.unmount()
  })
})
