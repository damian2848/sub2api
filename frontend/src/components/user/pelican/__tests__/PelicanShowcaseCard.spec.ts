import { mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick } from 'vue'
import PelicanShowcaseCard from '../PelicanShowcaseCard.vue'
import type { PelicanShowcaseItem } from '@/api/pelicanShowcase'

vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key }),
}))

// Lets a test move the card in and out of view.
let report: (visible: boolean) => void = () => {}
let observerOptions: IntersectionObserverInit | undefined
class ManualObserver {
  constructor(callback: IntersectionObserverCallback, options?: IntersectionObserverInit) {
    observerOptions = options
    report = (visible) => callback([{ isIntersecting: visible } as IntersectionObserverEntry], this as unknown as IntersectionObserver)
  }
  observe() {}
  disconnect() {}
  unobserve() {}
}

const item: PelicanShowcaseItem = {
  id: 1, group_id: 1, model_id: 'gpt-6-astra', reasoning_effort: 'medium', latency_ms: 1000, generated_at: '2026-09-28T04:00:00Z',
}
let wrapper: ReturnType<typeof mount>
let pageHidden = false

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
  vi.stubGlobal('IntersectionObserver', ManualObserver)
  pageHidden = false
  vi.spyOn(document, 'hidden', 'get').mockImplementation(() => pageHidden)
})
afterEach(() => {
  wrapper?.unmount()
  vi.restoreAllMocks()
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe('PelicanShowcaseCard', () => {
  it('asks for its HTML only once it stays in view, so cards a drag sweeps past load nothing', async () => {
    wrapper = mount(PelicanShowcaseCard, { props: { item, groupName: 'GPT Plus' } })
    // The same lookahead covers cards below the fold and cards still hidden in their row.
    expect(observerOptions).toEqual({ rootMargin: '200px', scrollMargin: '200px' })

    report(true)
    vi.advanceTimersByTime(100)
    report(false)
    vi.advanceTimersByTime(1000)
    await nextTick()
    expect(wrapper.emitted('visibility')).toBeUndefined()

    report(true)
    vi.advanceTimersByTime(150)
    await nextTick()
    expect(wrapper.emitted('visibility')).toEqual([[true]])
  })

  it('drops a pending report when it goes away', () => {
    wrapper = mount(PelicanShowcaseCard, { props: { item, groupName: 'GPT Plus' } })
    report(true)
    expect(vi.getTimerCount()).toBe(1)
    wrapper.unmount()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('unmounts an offscreen iframe and restores it when the card returns', async () => {
    wrapper = mount(PelicanShowcaseCard, {
      props: { item, groupName: 'GPT Plus', body: { status: 'ready', html: '<svg data-result="1"></svg>' } },
    })
    expect(wrapper.find('iframe').exists()).toBe(false)
    report(true)
    vi.advanceTimersByTime(150)
    await nextTick()
    expect(wrapper.get('iframe').attributes('srcdoc')).toContain('data-result="1"')

    report(false)
    await nextTick()
    expect(wrapper.find('iframe').exists()).toBe(false)
    report(true)
    vi.advanceTimersByTime(150)
    await nextTick()
    expect(wrapper.get('iframe').attributes('srcdoc')).toContain('data-result="1"')
    expect(wrapper.emitted('visibility')).toEqual([[true], [false], [true]])
  })

  it('suspends the card animation while a dialog is open or the page is hidden', async () => {
    wrapper = mount(PelicanShowcaseCard, {
      props: { item, groupName: 'GPT Plus', body: { status: 'ready', html: '<svg></svg>' } },
    })
    report(true)
    vi.advanceTimersByTime(150)
    await nextTick()
    expect(wrapper.find('iframe').exists()).toBe(true)

    await wrapper.setProps({ suspended: true })
    expect(wrapper.find('iframe').exists()).toBe(false)
    await wrapper.setProps({ suspended: false })
    expect(wrapper.find('iframe').exists()).toBe(true)

    pageHidden = true
    document.dispatchEvent(new Event('visibilitychange'))
    await nextTick()
    expect(wrapper.find('iframe').exists()).toBe(false)
    pageHidden = false
    document.dispatchEvent(new Event('visibilitychange'))
    await nextTick()
    expect(wrapper.find('iframe').exists()).toBe(true)
    expect(wrapper.emitted('visibility')).toEqual([[true], [false], [true], [false], [true]])
  })
})
