import { createPinia } from 'pinia'
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import AvailableChannelsView from '../AvailableChannelsView.vue'
import type { UserAvailableGroupView } from '@/api/channels'

const getAvailable = vi.hoisted(() => vi.fn())
const getUserGroupRates = vi.hoisted(() => vi.fn())

vi.mock('@/api/channels', () => ({ default: { getAvailable }, getAvailable }))
vi.mock('@/api/groups', () => ({ default: { getUserGroupRates } }))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

function view(id: number, name: string, platform: string, models: string[], channel = 'base'): UserAvailableGroupView {
  return {
    group: {
      id,
      name,
      platform,
      subscription_type: 'standard',
      rate_multiplier: 1,
      peak_rate_enabled: false,
      peak_start: '',
      peak_end: '',
      peak_rate_multiplier: 1,
      is_exclusive: false,
    },
    channels: [{ name: channel, description: '' }],
    models: models.map((name) => ({ name, platform, pricing: null })),
  }
}

const data: UserAvailableGroupView[] = [
  view(1, 'gpt-pro', 'openai', ['gpt-5', 'o3']),
  view(2, 'gpt-enterprise', 'openai', ['gpt-5', 'gpt-4o'], 'enterprise-line'),
  view(3, 'claude-kiro', 'anthropic', ['claude-opus-4-7']),
]

// 把子组件替换成可断言的最小 stub，只验证视图层的过滤与联动。
const CardsStub = {
  props: ['rows', 'forceExpand'],
  template:
    '<div data-cards :data-force-expand="forceExpand"><span v-for="r in rows" :key="r.group.id" data-row>{{ r.group.name }}[{{ r.models.map(m => m.name).join("|") }}]</span></div>',
}

async function mountView() {
  const wrapper = mount(AvailableChannelsView, {
    global: {
      plugins: [createPinia()],
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: { template: '<div><slot name="filters" /><slot name="table" /></div>' },
        Icon: { template: '<i />' },
        PlatformIcon: { template: '<i />' },
        AvailableGroupCards: CardsStub,
      },
    },
  })
  await flushPromises()
  return wrapper
}

const rowTexts = (w: Awaited<ReturnType<typeof mountView>>) => w.findAll('[data-row]').map((r) => r.text())

describe('AvailableChannelsView', () => {
  beforeEach(() => {
    getAvailable.mockResolvedValue(data)
    getUserGroupRates.mockResolvedValue({})
  })

  it('lists every group and builds platform tabs with counts', async () => {
    const wrapper = await mountView()

    expect(rowTexts(wrapper)).toHaveLength(3)
    const tabs = wrapper.findAll('[data-testid="platform-tab"]').map((t) => t.text())
    expect(tabs).toEqual(['availableChannels.allPlatforms3', 'anthropic1', 'openai2'])
  })

  it('filters cards by the active platform tab', async () => {
    const wrapper = await mountView()

    await wrapper.findAll('[data-testid="platform-tab"]')[2].trigger('click')
    expect(rowTexts(wrapper)).toEqual(['gpt-pro[gpt-5|o3]', 'gpt-enterprise[gpt-5|gpt-4o]'])
  })

  it('keeps the whole card when the group or channel name matches', async () => {
    const wrapper = await mountView()

    await wrapper.get('input').setValue('enterprise')
    expect(rowTexts(wrapper)).toEqual(['gpt-enterprise[gpt-5|gpt-4o]'])
    expect(wrapper.get('[data-cards]').attributes('data-force-expand')).toBe('true')
  })

  it('narrows a card to the matching models when only models match', async () => {
    const wrapper = await mountView()

    await wrapper.get('input').setValue('4o')
    expect(rowTexts(wrapper)).toEqual(['gpt-enterprise[gpt-4o]'])
  })

  it('falls back to "all" when the active platform disappears after searching', async () => {
    const wrapper = await mountView()

    await wrapper.findAll('[data-testid="platform-tab"]')[1].trigger('click') // anthropic
    expect(rowTexts(wrapper)).toEqual(['claude-kiro[claude-opus-4-7]'])

    await wrapper.get('input').setValue('gpt-5') // 只有 openai 分组命中
    expect(rowTexts(wrapper)).toHaveLength(2)
  })

  it('hides the tab bar when only one platform is present', async () => {
    getAvailable.mockResolvedValue([data[0], data[1]])
    const wrapper = await mountView()

    expect(wrapper.find('[data-testid="platform-tabs"]').exists()).toBe(false)
  })
})
