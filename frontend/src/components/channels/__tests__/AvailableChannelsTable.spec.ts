import { createPinia } from 'pinia'
import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import AvailableChannelsTable from '../AvailableChannelsTable.vue'
import type { UserAvailableChannel } from '@/api/channels'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

const rows: UserAvailableChannel[] = [
  {
    name: 'Primary channel',
    description: 'Fast and reliable access',
    platforms: [
      {
        platform: 'anthropic',
        groups: [
          {
            supported_models: [{ name: 'claude-pro', platform: 'anthropic', pricing: null }],
            id: 1,
            name: 'Exclusive Pro',
            platform: 'anthropic',
            subscription_type: 'standard',
            rate_multiplier: 1.2,
            peak_rate_enabled: true,
            peak_start: '08:00',
            peak_end: '10:00',
            peak_rate_multiplier: 1.5,
            is_exclusive: true,
          },
          {
            supported_models: [{ name: 'claude-public', platform: 'anthropic', pricing: null }],
            id: 2,
            name: 'Public',
            platform: 'anthropic',
            subscription_type: 'standard',
            rate_multiplier: 1,
            peak_rate_enabled: false,
            peak_start: '',
            peak_end: '',
            peak_rate_multiplier: 1,
            is_exclusive: false,
          },
        ],
        supported_models: [{ name: 'claude-test', platform: 'anthropic', pricing: null }],
      },
    ],
  },
]

const baseProps = {
  columns: {
    name: 'Channel',
    description: 'Description',
    platform: 'Platform',
    groups: 'Groups and rates',
    supportedModels: 'Models and pricing',
  },
  rows,
  loading: false,
  pricingKeyPrefix: 'availableChannels.pricing',
  noPricingLabel: 'No pricing',
  emptyLabel: 'No channels',
  userGroupRates: { 1: 0.8 },
}

function mountTable(props = {}) {
  return mount(AvailableChannelsTable, {
    props: { ...baseProps, ...props },
    global: {
      plugins: [createPinia()],
      stubs: {
        Icon: { props: ['name'], template: '<i :data-icon="name" />' },
        PlatformIcon: { template: '<i data-platform-icon />' },
        GroupBadge: {
          props: ['name', 'rateMultiplier', 'userRateMultiplier'],
          template:
            '<span data-group-badge>{{ name }}:{{ rateMultiplier }}:{{ userRateMultiplier }}</span>',
        },
        SupportedModelChip: {
          props: ['model', 'noPricingLabel'],
          template: '<span data-model-chip>{{ model.name }}:{{ noPricingLabel }}</span>',
        },
      },
    },
  })
}

describe('AvailableChannelsTable supported groups', () => {
  it('keeps model lists separate while preserving rates and scroll integration', () => {
    const wrapper = mountTable()
    expect(wrapper.classes()).toContain('table-wrapper')
    expect(wrapper.findAll('[data-testid="channel-platform"]')).toHaveLength(1)
    const groups = wrapper.findAll('[data-testid="channel-group"]')
    expect(groups).toHaveLength(2)
    expect(groups[0].text()).toContain('Exclusive Pro:1.2:0.8')
    expect(groups[0].text()).toContain('claude-pro')
    expect(groups[0].text()).not.toContain('claude-public')
    expect(groups[1].text()).toContain('claude-public')
    expect(groups[1].text()).not.toContain('claude-pro')
    expect(wrapper.text()).not.toContain('claude-test')
    expect(groups[0].text()).toContain('08:00')
    expect(groups[0].text()).toContain('×1.5')
    expect(wrapper.find('table').exists()).toBe(false)
  })
  it('searches groups and models without unrelated siblings', async () => {
    const wrapper = mountTable({ searchQuery: 'Exclusive Pro' })
    expect(wrapper.findAll('[data-testid="channel-group"]')).toHaveLength(1)
    expect(wrapper.text()).not.toContain('claude-public')
    await wrapper.setProps({ searchQuery: 'claude-public' })
    expect(wrapper.findAll('[data-model-chip]')).toHaveLength(1)
    expect(wrapper.text()).not.toContain('claude-pro')
    await wrapper.setProps({ searchQuery: 'anthropic' })
    expect(wrapper.findAll('[data-testid="channel-group"]')).toHaveLength(2)
  })
  it('hides empty groups instead of falling back to platform pricing', () => {
    const emptyRows = structuredClone(rows)
    emptyRows[0].platforms[0].groups.forEach(group => { group.supported_models = [] })
    const wrapper = mountTable({ rows: emptyRows })
    expect(wrapper.findAll('[data-testid="channel-platform"]')).toHaveLength(0)
    expect(wrapper.get('[data-testid="channels-empty"]').text()).toContain('No channels')
    expect(wrapper.text()).not.toContain('claude-test')
  })
  it('excludes wildcard and cross-platform entries', () => {
    const mixedRows = structuredClone(rows)
    mixedRows[0].platforms[0].groups[0].supported_models.push(
      { name: 'claude-*', platform: 'anthropic', pricing: null },
      { name: 'gpt-5', platform: 'openai', pricing: null },
    )
    expect(mountTable({ rows: mixedRows }).findAll('[data-model-chip]')).toHaveLength(2)
  })
  it('provides loading and empty states', async () => {
    const wrapper = mountTable({ loading: true, rows: [] })
    expect(wrapper.get('[data-testid="channels-loading"] [data-icon="refresh"]')).toBeTruthy()
    await wrapper.setProps({ loading: false })
    expect(wrapper.get('[data-testid="channels-empty"]').text()).toContain('No channels')
  })
})
