import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, onMounted, onBeforeUnmount } from 'vue'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import ChannelOperationsView from '../ChannelOperationsView.vue'
import { CHANNEL_OPERATIONS_PATH } from '@/utils/channelOperations'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<main data-testid="app-layout"><slot /></main>' } }))

const mounted = new Set<string>()
const polls: Record<string, number> = {}
let wrapper: VueWrapper | undefined

function page(name: string) {
  return defineComponent({
    props: { embedded: Boolean },
    setup() {
      let timer: ReturnType<typeof setInterval>
      onMounted(() => { mounted.add(name); timer = setInterval(() => { polls[name] = (polls[name] ?? 0) + 1 }, 1000) })
      onBeforeUnmount(() => { mounted.delete(name); clearInterval(timer) })
      return { name }
    },
    template: '<section data-testid="business-page" :data-page="name" :data-embedded="embedded">{{ name }}</section>',
  })
}

async function workspace(path = 'bps') {
  const children = ['bps', 'harvest', 'prism', 'smart/quality', 'smart/priority', 'smart/alerts', 'credentials', 'credentials/legacy', 'pelican']
    .map(section => ({ path: section, component: page(section), props: { embedded: true } }))
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: CHANNEL_OPERATIONS_PATH, component: ChannelOperationsView, children }],
  })
  await router.push(`${CHANNEL_OPERATIONS_PATH}/${path}`)
  await router.isReady()
  wrapper = mount(defineComponent({ template: '<router-view />' }), { global: { plugins: [router], stubs: { Icon: true } } })
  await flushPromises()
  return { router, wrapper }
}

describe('ChannelOperationsView', () => {
  beforeEach(() => { vi.useFakeTimers(); mounted.clear(); for (const key of Object.keys(polls)) delete polls[key] })
  afterEach(() => { wrapper?.unmount(); wrapper = undefined; vi.useRealTimers() })

  it('renders six module tabs in one layout and only mounts the active page', async () => {
    const { wrapper } = await workspace()
    expect(wrapper.findAll('[data-testid="app-layout"]')).toHaveLength(1)
    expect(wrapper.findAll('[data-tab]')).toHaveLength(6)
    expect(wrapper.get('[data-tab="bps"]').attributes('aria-current')).toBe('page')
    expect(wrapper.findAll('[data-testid="business-page"]')).toHaveLength(1)
    expect(wrapper.get('[data-testid="business-page"]').attributes('data-embedded')).toBe('true')
    expect([...mounted]).toEqual(['bps'])
    expect(wrapper.find('[data-testid="channel-operations-sections"]').exists()).toBe(false)
  })

  it('navigates all six modules without mounting hidden panels', async () => {
    const { wrapper } = await workspace()
    for (const [tab, section] of [['harvest', 'harvest'], ['prism', 'prism'], ['smart', 'smart/quality'], ['credentials', 'credentials'], ['pelican', 'pelican'], ['bps', 'bps']]) {
      await wrapper.get(`[data-tab="${tab}"]`).trigger('click')
      await flushPromises()
      expect([...mounted]).toEqual([section])
      expect(wrapper.get(`[data-tab="${tab}"]`).attributes('aria-current')).toBe('page')
      expect(wrapper.findAll('[data-testid="app-layout"]')).toHaveLength(1)
    }
  })

  it('keeps the parent module selected across smart and credential subpages', async () => {
    const { router, wrapper } = await workspace('smart/priority')
    expect(wrapper.get('[data-tab="smart"]').attributes('aria-current')).toBe('page')
    expect(wrapper.get('[data-testid="channel-operations-sections"]').findAll('a')).toHaveLength(3)
    expect(wrapper.get('a[href="/admin/channel-ops/smart/priority"]').attributes('aria-current')).toBe('page')
    await router.push(`${CHANNEL_OPERATIONS_PATH}/credentials/legacy`)
    await flushPromises()
    expect(wrapper.get('[data-tab="credentials"]').attributes('aria-current')).toBe('page')
    expect(wrapper.get('[data-testid="channel-operations-sections"]').findAll('a')).toHaveLength(2)
    expect([...mounted]).toEqual(['credentials/legacy'])
  })

  it('unmounts the prior page and stops its poller when switching tabs', async () => {
    const { router } = await workspace('harvest')
    await vi.advanceTimersByTimeAsync(2000)
    expect(polls.harvest).toBe(2)
    await router.push(`${CHANNEL_OPERATIONS_PATH}/prism`)
    await flushPromises()
    await vi.advanceTimersByTimeAsync(2000)
    expect(polls.harvest).toBe(2)
    expect(polls.prism).toBe(2)
    wrapper?.unmount(); wrapper = undefined
    await vi.advanceTimersByTimeAsync(2000)
    expect(polls.prism).toBe(2)
    expect(mounted.size).toBe(0)
  })
})
