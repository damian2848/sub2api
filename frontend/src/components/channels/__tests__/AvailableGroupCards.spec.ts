import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { createPinia } from 'pinia'
import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import AvailableGroupCards from '../AvailableGroupCards.vue'
import type { UserAvailableGroup, UserAvailableGroupView } from '@/api/channels'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key}:${Object.values(params).join(',')}` : key,
    }),
  }
})

const componentPath = resolve(dirname(fileURLToPath(import.meta.url)), '../AvailableGroupCards.vue')
const componentSource = readFileSync(componentPath, 'utf8')

describe('AvailableGroupCards scroll integration', () => {
  // #4555：根元素必须是 TablePageLayout 滚动链约定的 .table-wrapper，
  // 否则内容超出视口高度时被外层 overflow-hidden 裁剪且没有滚动条。
  it('mounts the cards on the .table-wrapper scroll hook', () => {
    expect(componentSource).toMatch(/<template>\s*<!--[\s\S]*?-->\s*<div class="table-wrapper">/)
  })

  it('does not clip content with its own overflow-hidden card wrapper', () => {
    expect(componentSource).not.toMatch(/<div class="card overflow-hidden">/)
  })
})

function group(over: Partial<UserAvailableGroup> & Pick<UserAvailableGroup, 'id' | 'name'>): UserAvailableGroup {
  return {
    platform: 'openai',
    subscription_type: 'standard',
    rate_multiplier: 1,
    peak_rate_enabled: false,
    peak_start: '',
    peak_end: '',
    peak_rate_multiplier: 1,
    is_exclusive: false,
    ...over,
  }
}

function models(prefix: string, n: number, platform = 'openai') {
  return Array.from({ length: n }, (_, i) => ({ name: `${prefix}-${i}`, platform, pricing: null }))
}

const rows: UserAvailableGroupView[] = [
  {
    group: group({ id: 1, name: 'gpt-pro', rate_multiplier: 0.23 }),
    channels: [{ name: 'base', description: '' }, { name: 'extra', description: '' }],
    models: models('gpt-a', 3),
  },
  {
    // 同平台的第二个分组：必须独立成卡，而不是并入 gpt-pro。
    group: group({
      id: 2,
      name: 'gpt-enterprise',
      rate_multiplier: 0.35,
      is_exclusive: true,
      peak_rate_enabled: true,
      peak_start: '08:00',
      peak_end: '10:00',
      peak_rate_multiplier: 1.5,
    }),
    channels: [{ name: 'base', description: '' }],
    models: models('gpt-b', 30),
  },
  {
    group: group({ id: 3, name: 'empty-group', platform: 'anthropic' }),
    channels: [{ name: 'base', description: '' }],
    models: [],
  },
]

const baseProps = {
  rows,
  loading: false,
  pricingKeyPrefix: 'availableChannels.pricing',
  noPricingLabel: 'No pricing',
  noModelsLabel: 'No models',
  emptyLabel: 'No channels',
  userGroupRates: { 1: 0.2 },
}

function mountCards(props = {}) {
  return mount(AvailableGroupCards, {
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
          props: ['model', 'showPlatform'],
          template: '<span data-model-chip>{{ model.name }}</span>',
        },
      },
    },
  })
}

describe('AvailableGroupCards', () => {
  it('renders one card per group, never merging same-platform groups', () => {
    const wrapper = mountCards()
    const cards = wrapper.findAll('[data-testid="group-card"]')

    expect(cards).toHaveLength(3)
    const badges = cards.map((c) => c.get('[data-group-badge]').text())
    expect(badges).toEqual(['gpt-pro:0.23:0.2', 'gpt-enterprise:0.35:', 'empty-group:1:'])
    // 每张卡只含自己分组的模型。
    expect(cards[0].findAll('[data-model-chip]').map((c) => c.text())).toEqual(['gpt-a-0', 'gpt-a-1', 'gpt-a-2'])
    expect(cards[1].text()).not.toContain('gpt-a-0')
  })

  it('shows channel names, exclusive/public marker, peak window and model count', () => {
    const [first, second] = mountCards().findAll('[data-testid="group-card"]')

    expect(first.text()).toContain('base · extra')
    expect(first.text()).toContain('availableChannels.public')
    expect(first.text()).toContain('availableChannels.modelCount:3')
    expect(second.text()).toContain('availableChannels.exclusive')
    expect(second.get('[data-icon="clock"]')).toBeTruthy()
    expect(second.text()).toContain('08:00')
    expect(second.text()).toContain('×1.5')
  })

  it('collapses long model lists to 24 chips and expands/collapses on demand', async () => {
    const second = mountCards().findAll('[data-testid="group-card"]')[1]

    expect(second.findAll('[data-model-chip]')).toHaveLength(24)
    const toggle = second.get('[data-testid="toggle-models"]')
    expect(toggle.text()).toBe('availableChannels.expandAll:6')

    await toggle.trigger('click')
    expect(second.findAll('[data-model-chip]')).toHaveLength(30)
    expect(second.get('[data-testid="toggle-models"]').text()).toBe('availableChannels.collapse')

    await second.get('[data-testid="toggle-models"]').trigger('click')
    expect(second.findAll('[data-model-chip]')).toHaveLength(24)
  })

  it('does not render a toggle for short lists', () => {
    const first = mountCards().findAll('[data-testid="group-card"]')[0]
    expect(first.find('[data-testid="toggle-models"]').exists()).toBe(false)
  })

  it('expands every card when forceExpand is set (active search)', () => {
    const second = mountCards({ forceExpand: true }).findAll('[data-testid="group-card"]')[1]

    expect(second.findAll('[data-model-chip]')).toHaveLength(30)
    expect(second.find('[data-testid="toggle-models"]').exists()).toBe(false)
  })

  it('shows the no-models placeholder for a group without callable models', () => {
    const third = mountCards().findAll('[data-testid="group-card"]')[2]

    expect(third.text()).toContain('No models')
    expect(third.findAll('[data-model-chip]')).toHaveLength(0)
  })

  it('provides loading and empty states', async () => {
    const wrapper = mountCards({ loading: true, rows: [] })
    expect(wrapper.get('[data-testid="channels-loading"] [data-icon="refresh"]')).toBeTruthy()

    await wrapper.setProps({ loading: false })
    expect(wrapper.get('[data-testid="channels-empty"]').text()).toContain('No channels')
  })
})
