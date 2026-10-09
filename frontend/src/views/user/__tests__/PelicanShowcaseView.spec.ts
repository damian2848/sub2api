import { flushPromises, mount, RouterLinkStub } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import PelicanShowcaseView from '../PelicanShowcaseView.vue'
import type { PelicanShowcaseItem, PelicanShowcaseView as ShowcaseData } from '@/api/pelicanShowcase'

const { getShowcase, getShowcaseItem, removeShowcaseItem, showError, showSuccess, auth } = vi.hoisted(() => ({
  getShowcase: vi.fn(),
  getShowcaseItem: vi.fn(),
  removeShowcaseItem: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn(),
  auth: { isAdmin: false },
}))
vi.mock('@/api/pelicanShowcase', () => ({ getShowcase, getShowcaseItem, removeShowcaseItem }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError, showSuccess }) }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => auth }))
vi.mock('@/composables/useClipboard', () => ({ useClipboard: () => ({ copyToClipboard: vi.fn() }) }))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string, named?: Record<string, unknown>) => (named ? `${key} ${JSON.stringify(named)}` : key) }),
}))

const item = (id: number, groupId: number, modelId = 'gpt-6-astra'): PelicanShowcaseItem => ({
  id, group_id: groupId, model_id: modelId, reasoning_effort: 'high', latency_ms: 42300,
  generated_at: '2026-09-24T08:30:00Z',
})
const showcase = (overrides: Partial<ShowcaseData> = {}): ShowcaseData => ({
  enabled: true,
  api_enabled: true,
  max_items: 20,
  retention_days: 7,
  groups: [
    { id: 1, name: 'Claude Max', platform: 'anthropic', items: Array.from({ length: 10 }, (_, i) => item(100 + i, 1, i === 9 ? 'gpt-6-sol' : 'gpt-6-astra')) },
    { id: 2, name: 'GPT Plus', platform: 'openai', items: [item(200, 2)] },
    { id: 3, name: 'Empty', platform: 'gemini', items: [] },
  ],
  ...overrides,
})
const modelShowcase = (ids: number[]) => showcase({
  groups: [{ id: 1, name: 'GPT Plus', platform: 'openai', items: ids.map((id) => item(id, 1)) }],
})

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((accept) => { resolve = accept })
  return { promise, resolve }
}

const mountView = () => mount(PelicanShowcaseView, {
  global: {
    stubs: {
      AppLayout: { template: '<div><slot /></div>' },
      Icon: true,
      PlatformIcon: true,
      RouterLink: RouterLinkStub,
      EmptyState: { props: ['title', 'description'], template: '<div class="empty-state">{{ title }}</div>' },
      ConfirmDialog: {
        props: ['show'], emits: ['confirm', 'cancel'],
        template: '<div v-if="show" class="confirm"><button class="confirm-yes" @click="$emit(\'confirm\')" /></div>',
      },
      BaseDialog: {
        props: ['show', 'title'], emits: ['close'],
        template: '<div v-if="show" class="dialog"><h3>{{ title }}</h3><slot /><slot name="footer" /></div>',
      },
    },
  },
})

// The shared test setup installs an observer that never fires; cards here are on screen.
class OnScreenObserver {
  constructor(private readonly callback: IntersectionObserverCallback) {}
  observe(target: Element) {
    this.callback([{ isIntersecting: true, target } as IntersectionObserverEntry], this as unknown as IntersectionObserver)
  }
  disconnect() {}
  unobserve() {}
}

// Cards report being on screen after a short dwell (see PelicanShowcaseCard); then their HTML is fetched.
async function settle() {
  await flushPromises()
  await vi.advanceTimersByTimeAsync(1000)
  await flushPromises()
}

let wrapper: ReturnType<typeof mountView>
beforeEach(() => {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
  vi.stubGlobal('IntersectionObserver', OnScreenObserver)
  vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
  getShowcase.mockReset()
  getShowcaseItem.mockReset().mockImplementation(async (id: number) => ({
    ...item(id, 0),
    response_text: id === 201 ? '21' : `<svg data-item="${id}"></svg>`,
  }))
  removeShowcaseItem.mockReset().mockResolvedValue(undefined)
  showError.mockReset()
  showSuccess.mockReset()
  auth.isAdmin = false
})
afterEach(() => {
  wrapper?.unmount()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

describe('PelicanShowcaseView', () => {
  it('explains that the gallery is closed without asking for items', async () => {
    getShowcase.mockResolvedValue(showcase({ enabled: false, groups: [] }))
    wrapper = mountView()
    await flushPromises()
    expect(wrapper.get('.empty-state').text()).toBe('pelicanShowcase.disabled.title')
    expect(wrapper.findAll('[data-testid="pelican-showcase-card"]')).toHaveLength(0)
    expect(getShowcaseItem).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid="showcase-api-open"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="showcase-notice"]').exists()).toBe(false)
  })

  it('puts the notice about imperfect drawings at the top of the page while there are groups', async () => {
    getShowcase.mockResolvedValue(showcase())
    wrapper = mountView()
    await flushPromises()

    const notice = wrapper.get('[data-testid="showcase-notice"]')
    expect(notice.text()).toBe('pelicanShowcase.notice')
    expect(notice.attributes('role')).toBe('note')
    // First element of the page, directly above the toolbar with the gallery rules.
    expect(notice.element.previousElementSibling).toBeNull()
    expect(notice.element.nextElementSibling?.querySelector('[data-testid="showcase-keep-rule"]')).not.toBeNull()
  })

  it('leaves the notice out of an empty gallery', async () => {
    getShowcase.mockResolvedValue(showcase({ groups: [] }))
    wrapper = mountView()
    await flushPromises()

    expect(wrapper.get('.empty-state').text()).toBe('pelicanShowcase.empty.title')
    expect(wrapper.find('[data-testid="showcase-notice"]').exists()).toBe(false)
  })

  it('opens API examples with the effective access state and an existing result ID', async () => {
    getShowcase.mockResolvedValue(showcase())
    wrapper = mountView()
    await flushPromises()

    expect(wrapper.find('[data-testid="showcase-api-dialog"]').exists()).toBe(false)
    await wrapper.get('[data-testid="showcase-api-open"]').trigger('click')
    const dialog = wrapper.get('[data-testid="showcase-api-dialog"]')
    expect(dialog.get('[data-testid="showcase-api-status"]').text()).toBe('pelicanShowcase.api.available')
    expect(dialog.get('[data-testid="showcase-api-item-url"]').text()).toContain('/items/100')
    expect(dialog.get('[data-testid="showcase-api-command"]').text()).toContain('Bearer YOUR_API_KEY')
    await dialog.get('[data-testid="showcase-api-keys"]').trigger('click')
    expect(wrapper.find('[data-testid="showcase-api-dialog"]').exists()).toBe(false)
  })

  it.each([false, undefined])('keeps API information available when api_enabled is %s', async (apiEnabled) => {
    getShowcase.mockResolvedValue(showcase({ api_enabled: apiEnabled }))
    wrapper = mountView()
    await flushPromises()

    await wrapper.get('[data-testid="showcase-api-open"]').trigger('click')
    const dialog = wrapper.get('[data-testid="showcase-api-dialog"]')
    expect(dialog.get('[data-testid="showcase-api-status"]').text()).toBe('pelicanShowcase.api.unavailable')
    expect(dialog.text()).toContain('pelicanShowcase.api.unavailableHint')
    expect(dialog.get('[data-testid="showcase-api-command"]').text()).toContain('/api/v1/public/pelican-showcase')
    // One card per group and model; older results of that model sit in its history.
    expect(wrapper.findAll('[data-testid="pelican-showcase-card"]')).toHaveLength(3)
  })

  it('lists every group as one row with the gallery rules and loads visible cards in a sandbox', async () => {
    getShowcase.mockResolvedValue(showcase())
    wrapper = mountView()
    await settle()

    expect(wrapper.get('[data-testid="showcase-keep-rule"]').text()).toContain('"count":20')
    expect(wrapper.get('[data-testid="showcase-retention-rule"]').text()).toContain('"days":7')
    expect(wrapper.findAll('[role="tab"]').map((tab) => tab.text())).toEqual([
      'pelicanShowcase.allGroups', 'Claude Max10', 'GPT Plus1', 'Empty0',
    ])
    expect(wrapper.get('[data-testid="showcase-group-3"]').text()).toContain('pelicanShowcase.groupEmpty')

    // Each model gets one card; its newest result is shown first.
    const firstRow = wrapper.get('[data-testid="showcase-group-1"] [data-testid="pelican-showcase-row"]')
    expect(firstRow.findAll('iframe').map((frame) => frame.attributes('srcdoc').match(/data-item="(\d+)"/)?.[1]))
      .toEqual(['108', '109'])
    expect(wrapper.find('[data-testid="showcase-group-3"] [data-testid="pelican-showcase-row"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="showcase-group-1"] [role="scrollbar"]').attributes('aria-label'))
      .toBe('pelicanShowcase.scrollLabel {"group":"Claude Max"}')

    const cards = wrapper.findAll('[data-testid="pelican-showcase-card"]')
    expect(cards).toHaveLength(3)
    expect(cards[0].classes()).toContain('shrink-0')
    expect(getShowcaseItem).toHaveBeenCalledTimes(3)
    const frame = cards[0].get('iframe')
    expect(frame.attributes('sandbox')).toBe('allow-scripts')
    expect(frame.attributes('referrerpolicy')).toBe('no-referrer')
    expect(frame.attributes('srcdoc')).toContain('Content-Security-Policy')
    expect(frame.attributes('srcdoc')).toContain('data-item="108"')
    expect(cards[0].text()).toContain('gpt-6-astra')
    expect(cards[0].text()).toContain('"seconds":"42.3"')
    expect(cards[0].text()).toContain('pelicanShowcase.efforts.high')
    expect(cards[0].get('[data-testid="pelican-showcase-history-prev"]').attributes('disabled')).toBeUndefined()
    expect(cards[0].get('[data-testid="pelican-showcase-history-next"]').attributes('disabled')).toBeDefined()
    await cards[0].get('[data-testid="pelican-showcase-history-prev"]').trigger('click')
    await flushPromises()
    await vi.advanceTimersByTimeAsync(250)
    await flushPromises()
    expect(cards[0].text()).toContain('pelicanShowcase.historyPosition {"current":2,"total":9}')
    expect(cards[0].get('iframe').attributes('srcdoc')).toContain('data-item="107"')

    await wrapper.get('[data-testid="showcase-tab-2"]').trigger('click')
    expect(wrapper.find('[data-testid="showcase-group-1"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="showcase-group-2"]').exists()).toBe(true)
  })

  it('fetches HTML only for cards that reach the viewport', async () => {
    vi.unstubAllGlobals() // back to the setup observer, which never reports a card as visible
    getShowcase.mockResolvedValue(showcase())
    wrapper = mountView()
    await settle()
    expect(wrapper.findAll('[data-testid="pelican-showcase-card"]')).toHaveLength(3)
    expect(getShowcaseItem).not.toHaveBeenCalled()
  })

  it('shows a readable state for output without HTML and for failed loads', async () => {
    getShowcase.mockResolvedValue(showcase({
      groups: [{ id: 2, name: 'GPT Plus', platform: 'openai', items: [item(201, 2, 'gpt-6-astra'), item(202, 2, 'gpt-6-sol')] }],
    }))
    getShowcaseItem.mockImplementation(async (id: number) => {
      if (id === 202) throw new Error('boom')
      return { ...item(id, 2), response_text: '21' }
    })
    wrapper = mountView()
    await settle()
    const cards = wrapper.findAll('[data-testid="pelican-showcase-card"]')
    expect(cards[0].find('iframe').exists()).toBe(false)
    expect(cards[0].text()).toContain('pelicanShowcase.invalidHtml')
    expect(cards[1].text()).toContain('pelicanShowcase.itemLoadError')

    // Refresh retries a failed card even though it already reported being on screen.
    getShowcaseItem.mockImplementation(async (id: number) => ({ ...item(id, 2), response_text: `<svg data-item="${id}"></svg>` }))
    await wrapper.get('button[aria-label="common.refresh"]').trigger('click')
    await flushPromises()
    expect(getShowcaseItem).toHaveBeenCalledTimes(3)
    expect(wrapper.findAll('[data-testid="pelican-showcase-card"]')[1].get('iframe').attributes('srcdoc')).toContain('data-item="202"')
  })

  it('previews a card, and only admins can take it down', async () => {
    getShowcase.mockResolvedValue(showcase())
    wrapper = mountView()
    await settle()

    await wrapper.get('[data-testid="showcase-group-2"] [data-testid="pelican-showcase-open"]').trigger('click')
    await flushPromises()
    const dialog = wrapper.get('[data-testid="showcase-preview"]')
    expect(dialog.get('iframe').attributes('srcdoc')).toContain('data-item="200"')
    expect(dialog.get('iframe').attributes('sandbox')).toBe('allow-scripts')
    expect(dialog.get('[data-testid="showcase-preview-fit"]').attributes('aria-pressed')).toBe('true')
    await dialog.get('[data-testid="showcase-preview-actual"]').trigger('click')
    expect(dialog.get('[data-testid="showcase-preview-actual"]').attributes('aria-pressed')).toBe('true')
    expect(wrapper.find('[data-testid="showcase-remove"]').exists()).toBe(false)
    expect(wrapper.findAll('[data-testid="pelican-showcase-card"] iframe')).toHaveLength(0)
    expect(getShowcaseItem).toHaveBeenCalledTimes(3)
    wrapper.unmount()

    auth.isAdmin = true
    wrapper = mountView()
    await settle()
    await wrapper.get('[data-testid="showcase-group-2"] [data-testid="pelican-showcase-open"]').trigger('click')
    await wrapper.get('[data-testid="showcase-remove"]').trigger('click')
    await wrapper.get('.confirm-yes').trigger('click')
    await flushPromises()
    expect(removeShowcaseItem).toHaveBeenCalledWith(200)
    expect(showSuccess).toHaveBeenCalledWith('pelicanShowcase.removed')
    expect(wrapper.find('[data-testid="showcase-preview"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="showcase-group-2"]').text()).toContain('pelicanShowcase.groupEmpty')
  })

  it('keeps history navigation within its boundaries and reuses a revisited result', async () => {
    getShowcase.mockResolvedValue(modelShowcase([3, 2, 1]))
    wrapper = mountView()
    await settle()
    const card = wrapper.get('[data-testid="pelican-showcase-card"]')
    const previous = () => card.get('[data-testid="pelican-showcase-history-prev"]')
    const next = () => card.get('[data-testid="pelican-showcase-history-next"]')
    expect(card.attributes('data-result-id')).toBe('3')
    expect(next().attributes('disabled')).toBeDefined()

    await previous().trigger('click')
    await flushPromises()
    expect(card.attributes('data-result-id')).toBe('2')
    await next().trigger('click')
    await flushPromises()
    expect(card.get('iframe').attributes('srcdoc')).toContain('data-item="3"')
    expect(getShowcaseItem).toHaveBeenCalledTimes(2)
    await previous().trigger('click')
    await flushPromises()
    expect(getShowcaseItem).toHaveBeenCalledTimes(2)

    await previous().trigger('click')
    await flushPromises()
    expect(card.attributes('data-result-id')).toBe('1')
    expect(previous().attributes('disabled')).toBeDefined()
    await previous().trigger('click')
    expect(card.attributes('data-result-id')).toBe('1')
    expect(wrapper.findAll('iframe')).toHaveLength(1)
  })

  it('selects the newest result after refresh, even when browsing an older result', async () => {
    getShowcase.mockResolvedValueOnce(modelShowcase([3, 2, 1])).mockResolvedValue(modelShowcase([4, 3, 2, 1]))
    wrapper = mountView()
    await settle()
    const card = wrapper.get('[data-testid="pelican-showcase-card"]')
    await card.get('[data-testid="pelican-showcase-history-prev"]').trigger('click')
    await flushPromises()
    expect(card.attributes('data-result-id')).toBe('2')

    await wrapper.get('button[aria-label="common.refresh"]').trigger('click')
    await flushPromises()
    expect(card.attributes('data-result-id')).toBe('4')
    expect(card.get('iframe').attributes('srcdoc')).toContain('data-item="4"')
    expect(card.get('[data-testid="pelican-showcase-history-next"]').attributes('disabled')).toBeDefined()
  })

  it('moves to a newer result on the first click after deleting the oldest result', async () => {
    auth.isAdmin = true
    getShowcase.mockResolvedValue(modelShowcase([3, 2, 1]))
    wrapper = mountView()
    await settle()
    const card = wrapper.get('[data-testid="pelican-showcase-card"]')
    await card.get('[data-testid="pelican-showcase-history-prev"]').trigger('click')
    await flushPromises()
    await card.get('[data-testid="pelican-showcase-history-prev"]').trigger('click')
    await flushPromises()
    expect(card.attributes('data-result-id')).toBe('1')
    await card.get('[data-testid="pelican-showcase-open"]').trigger('click')
    await wrapper.get('[data-testid="showcase-remove"]').trigger('click')
    await wrapper.get('.confirm-yes').trigger('click')
    await flushPromises()
    expect(removeShowcaseItem).toHaveBeenCalledWith(1)
    expect(card.attributes('data-result-id')).toBe('2')

    await card.get('[data-testid="pelican-showcase-history-next"]').trigger('click')
    await flushPromises()
    expect(card.attributes('data-result-id')).toBe('3')
    expect(card.get('iframe').attributes('srcdoc')).toContain('data-item="3"')
  })

  it('removes a previewed result from the current gallery when refresh finishes while the preview is open', async () => {
    auth.isAdmin = true
    const refreshed = deferred<ShowcaseData>()
    getShowcase.mockResolvedValueOnce(modelShowcase([3, 2, 1])).mockReturnValueOnce(refreshed.promise)
    wrapper = mountView()
    await settle()
    await wrapper.get('button[aria-label="common.refresh"]').trigger('click')
    await wrapper.get('[data-testid="pelican-showcase-open"]').trigger('click')
    refreshed.resolve(modelShowcase([4, 3, 2, 1]))
    await flushPromises()
    expect(wrapper.get('[data-testid="showcase-preview"] iframe').attributes('srcdoc')).toContain('data-item="3"')
    await wrapper.get('[data-testid="showcase-remove"]').trigger('click')
    await wrapper.get('.confirm-yes').trigger('click')
    await flushPromises()
    expect(removeShowcaseItem).toHaveBeenCalledWith(3)
    const card = wrapper.get('[data-testid="pelican-showcase-card"]')
    expect(card.attributes('data-result-id')).toBe('4')
    await card.get('[data-testid="pelican-showcase-history-prev"]').trigger('click')
    await flushPromises()
    expect(card.attributes('data-result-id')).toBe('2')
    expect(card.text()).toContain('pelicanShowcase.historyPosition {"current":2,"total":3}')
  })

  it('does not restore a deleted result when an earlier refresh responds after removal', async () => {
    auth.isAdmin = true
    const refreshed = deferred<ShowcaseData>()
    getShowcase.mockResolvedValueOnce(modelShowcase([3, 2, 1])).mockReturnValueOnce(refreshed.promise)
    wrapper = mountView()
    await settle()
    await wrapper.get('button[aria-label="common.refresh"]').trigger('click')
    const refreshSignal = getShowcase.mock.calls[1][0].signal as AbortSignal
    await wrapper.get('[data-testid="pelican-showcase-open"]').trigger('click')
    await wrapper.get('[data-testid="showcase-remove"]').trigger('click')
    await wrapper.get('.confirm-yes').trigger('click')
    await flushPromises()
    expect(removeShowcaseItem).toHaveBeenCalledWith(3)
    expect(refreshSignal.aborted).toBe(true)
    expect(wrapper.get('[data-testid="pelican-showcase-card"]').attributes('data-result-id')).toBe('2')

    refreshed.resolve(modelShowcase([3, 2, 1]))
    await flushPromises()
    const card = wrapper.get('[data-testid="pelican-showcase-card"]')
    expect(card.attributes('data-result-id')).toBe('2')
    expect(card.get('iframe').attributes('srcdoc')).toContain('data-item="2"')
    expect(card.text()).toContain('pelicanShowcase.historyPosition {"current":1,"total":2}')
    await card.get('[data-testid="pelican-showcase-history-prev"]').trigger('click')
    await flushPromises()
    expect(card.attributes('data-result-id')).toBe('1')
    expect(getShowcaseItem.mock.calls.map(([id]) => id)).toEqual([3, 2, 1])
  })

  it('aborts in-flight body requests when the view unmounts', async () => {
    const pending = deferred<PelicanShowcaseItem>()
    getShowcase.mockResolvedValue(modelShowcase([1]))
    getShowcaseItem.mockReturnValue(pending.promise)
    wrapper = mountView()
    await settle()
    const signal = getShowcaseItem.mock.calls[0][1].signal as AbortSignal
    expect(signal.aborted).toBe(false)
    wrapper.unmount()
    expect(signal.aborted).toBe(true)
    pending.resolve({ ...item(1, 1), response_text: '<svg></svg>' })
    await flushPromises()
    expect(showError).not.toHaveBeenCalled()
  })

  it('aborts an obsolete result when refresh replaces it and ignores its late response', async () => {
    const obsolete = deferred<PelicanShowcaseItem>()
    getShowcase.mockResolvedValueOnce(modelShowcase([1])).mockResolvedValue(modelShowcase([2]))
    getShowcaseItem.mockImplementation((id: number) => id === 1
      ? obsolete.promise
      : Promise.resolve({ ...item(id, 1), response_text: `<svg data-item="${id}"></svg>` }))
    wrapper = mountView()
    await settle()
    const signal = getShowcaseItem.mock.calls[0][1].signal as AbortSignal
    await wrapper.get('button[aria-label="common.refresh"]').trigger('click')
    await flushPromises()
    expect(signal.aborted).toBe(true)
    const card = wrapper.get('[data-testid="pelican-showcase-card"]')
    expect(card.get('iframe').attributes('srcdoc')).toContain('data-item="2"')
    obsolete.resolve({ ...item(1, 1), response_text: '<svg data-item="1"></svg>' })
    await flushPromises()
    expect(card.attributes('data-result-id')).toBe('2')
    expect(card.get('iframe').attributes('srcdoc')).toContain('data-item="2"')
    expect(getShowcaseItem).toHaveBeenCalledTimes(2)
  })

  it('reloads a rapidly revisited result after its canceled request settles without displaying the stale response', async () => {
    const canceled = deferred<PelicanShowcaseItem>()
    const replacement = deferred<PelicanShowcaseItem>()
    let olderRequests = 0
    getShowcase.mockResolvedValue(modelShowcase([3, 2]))
    getShowcaseItem.mockImplementation((id: number) => {
      if (id === 2) return olderRequests++ === 0 ? canceled.promise : replacement.promise
      return Promise.resolve({ ...item(id, 1), response_text: `<svg data-item="${id}"></svg>` })
    })
    wrapper = mountView()
    await settle()
    const card = wrapper.get('[data-testid="pelican-showcase-card"]')
    await card.get('[data-testid="pelican-showcase-history-prev"]').trigger('click')
    await flushPromises()
    const canceledSignal = getShowcaseItem.mock.calls[1][1].signal as AbortSignal
    expect(canceledSignal.aborted).toBe(false)

    await card.get('[data-testid="pelican-showcase-history-next"]').trigger('click')
    await flushPromises()
    expect(canceledSignal.aborted).toBe(true)
    expect(card.get('iframe').attributes('srcdoc')).toContain('data-item="3"')
    await card.get('[data-testid="pelican-showcase-history-prev"]').trigger('click')
    await flushPromises()
    expect(card.attributes('data-result-id')).toBe('2')
    expect(getShowcaseItem.mock.calls.map(([id]) => id)).toEqual([3, 2])

    canceled.resolve({ ...item(2, 1), response_text: '<svg data-item="2" data-response="stale"></svg>' })
    await flushPromises()
    expect(getShowcaseItem.mock.calls.map(([id]) => id)).toEqual([3, 2, 2])
    expect(card.find('iframe').exists()).toBe(false)
    expect((getShowcaseItem.mock.calls[2][1].signal as AbortSignal).aborted).toBe(false)
    replacement.resolve({ ...item(2, 1), response_text: '<svg data-item="2" data-response="fresh"></svg>' })
    await flushPromises()
    const document = card.get('iframe').attributes('srcdoc')
    expect(document).toContain('data-response="fresh"')
    expect(document).not.toContain('data-response="stale"')
  })

  it('renders and fetches one result per model when many historical results exist', async () => {
    getShowcase.mockResolvedValue(showcase({
      groups: [{
        id: 1, name: 'GPT Plus', platform: 'openai',
        items: Array.from({ length: 100 }, (_, index) => item(index + 1, 1, index % 2 ? 'gpt-6-astra' : 'gpt-6-sol')),
      }],
    }))
    wrapper = mountView()
    await settle()
    expect(wrapper.findAll('[data-testid="pelican-showcase-card"]')).toHaveLength(2)
    expect(wrapper.findAll('iframe')).toHaveLength(2)
    expect(getShowcaseItem).toHaveBeenCalledTimes(2)
    expect(getShowcaseItem.mock.calls.map(([id]) => id)).toEqual([99, 100])
  })
})
