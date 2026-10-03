import { beforeEach, describe, expect, it, vi } from 'vitest'
import { reactive, nextTick } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import ChannelPrismView from '../ChannelPrismView.vue'
import { getPrismSettings, savePrismSettings, resetPrismSettings, type PrismSettings } from '@/api/admin/prismConfig'
const state = vi.hoisted(() => ({ auth: null as any }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => state.auth }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@/api/client', () => ({ apiClient: {} }))
vi.mock('@/api/admin/prismConfig', async importOriginal => ({ ...await importOriginal<typeof import('@/api/admin/prismConfig')>(), getPrismSettings: vi.fn(), savePrismSettings: vi.fn(), resetPrismSettings: vi.fn() }))
const options = { project_isolation: false, http_cache: false, multiplex_pages: false, prewarm_chat: true, stream_reasoning: true, memory_limit_mib: 0, memory_reserve_mib: 32 }
const ready: PrismSettings = { gateway: { enabled: true, configured: true, base_url: 'http://prism:8080', management_key_configured: true }, availability: 'ready', configuration: { effective: { ...options }, desired: { ...options }, source: 'environment', apply_mode: 'restart', restart_required: false } }
function mountPage() { return mount(ChannelPrismView, { global: { stubs: { RouterLink: { template: '<a><slot /></a>' }, ConfirmDialog: { props: ['show'], emits: ['confirm', 'cancel'], template: '<div v-if="show" data-testid="confirmation"><button data-testid="confirm" @click="$emit(\'confirm\')" /><button data-testid="cancel" @click="$emit(\'cancel\')" /></div>' } } } }) }
beforeEach(() => {
  vi.resetAllMocks(); state.auth = reactive({ user: { id: 1, role: 'admin' } })
  vi.mocked(getPrismSettings).mockResolvedValue(structuredClone(ready))
  vi.mocked(savePrismSettings).mockImplementation(async desired => ({ ...structuredClone(ready), configuration: { ...structuredClone(ready.configuration!), desired, source: 'saved', restart_required: true } }))
  vi.mocked(resetPrismSettings).mockResolvedValue(structuredClone(ready))
})
describe('ChannelPrismView', () => {
  it('saves desired configuration, keeping effective values and restart warning distinct', async () => {
    const w = mountPage(); await flushPromises()
    expect(w.find('[data-testid="prism-restart-required"]').exists()).toBe(false)
    expect(w.get('[data-testid="prism-save"]').attributes('disabled')).toBeDefined()
    await w.get('[data-testid="prism-http_cache"]').setValue(true)
    await w.get('form').trigger('submit'); await flushPromises()
    expect(savePrismSettings).toHaveBeenCalledWith({ ...options, http_cache: true }, expect.any(AbortSignal))
    expect(w.get('[data-testid="prism-restart-required"]').text()).toBe('prismConfig.pending')
    expect(w.get('[data-testid="prism-effective-http_cache"]').text()).toContain('prismConfig.disabled')
    expect(w.get<HTMLInputElement>('[data-testid="prism-http_cache"]').element.checked).toBe(true)
    expect(w.text()).toContain('prismConfig.savedNotice'); w.unmount()
  })
  it.each(['unsupported', 'unavailable', 'not_configured'] as const)('disables writing when %s', async availability => {
    vi.mocked(getPrismSettings).mockResolvedValue({ ...ready, availability, configuration: null })
    const w = mountPage(); await flushPromises()
    expect(w.find('form').exists()).toBe(false)
    expect(w.text()).toContain('prismConfig.availability.' + availability)
    expect(savePrismSettings).not.toHaveBeenCalled(); w.unmount()
  })
  it('does not request as observer and cancels/ignores a late response after role loss', async () => {
    let resolve!: (value: PrismSettings) => void
    vi.mocked(getPrismSettings).mockImplementation(() => new Promise(done => { resolve = done }))
    const w = mountPage(); await nextTick()
    const signal = vi.mocked(getPrismSettings).mock.calls[0]![0]!
    state.auth.user.role = 'observer'; await nextTick()
    expect(signal.aborted).toBe(true)
    resolve(ready); await flushPromises()
    expect(w.find('form').exists()).toBe(false); expect(w.text()).toContain('prismConfig.forbidden'); w.unmount()
    vi.mocked(getPrismSettings).mockClear()
    const observer = mountPage(); await flushPromises(); expect(getPrismSettings).not.toHaveBeenCalled(); observer.unmount()
  })
  it('requires reset confirmation and restores environment without claiming a hot apply', async () => {
    vi.mocked(getPrismSettings).mockResolvedValue({ ...ready, configuration: { ...ready.configuration!, source: 'saved' } })
    const w = mountPage(); await flushPromises()
    await w.get('[data-testid="prism-reset"]').trigger('click')
    expect(resetPrismSettings).not.toHaveBeenCalled()
    await w.get('[data-testid="cancel"]').trigger('click')
    expect(resetPrismSettings).not.toHaveBeenCalled()
    await w.get('[data-testid="prism-reset"]').trigger('click'); await w.get('[data-testid="confirm"]').trigger('click'); await flushPromises()
    expect(resetPrismSettings).toHaveBeenCalledTimes(1)
    expect(w.text()).toContain('prismConfig.resetNotice')
    expect(w.get('[data-testid="prism-reset"]').attributes('disabled')).toBeDefined(); w.unmount()
  })
  it('rejects invalid memory and protects duplicate saves with a snapshot', async () => {
    const w = mountPage(); await flushPromises()
    await w.get('[data-testid="prism-memory_limit_mib"]').setValue(32); await w.get('form').trigger('submit')
    expect(savePrismSettings).not.toHaveBeenCalled(); expect(w.get('[role="alert"]').text()).toBe('prismConfig.invalid')
    await w.get('[data-testid="prism-memory_limit_mib"]').setValue(128)
    let resolve!: (value: PrismSettings) => void
    vi.mocked(savePrismSettings).mockImplementation(() => new Promise(done => { resolve = done }))
    await w.get('form').trigger('submit'); await w.get('form').trigger('submit')
    expect(savePrismSettings).toHaveBeenCalledTimes(1)
    const signal = vi.mocked(savePrismSettings).mock.calls[0]![1]!
    w.unmount(); expect(signal.aborted).toBe(true); resolve(ready); await flushPromises()
  })
  it('shows a safe error, preserves unsaved inputs and does not announce success on failure', async () => {
    vi.mocked(savePrismSettings).mockRejectedValue(new Error('management_key=SECRET OAuth token'))
    const w = mountPage(); await flushPromises(); await w.get('[data-testid="prism-http_cache"]').setValue(true)
    await w.get('form').trigger('submit'); await flushPromises()
    expect(w.get('[role="alert"]').text()).toBe('prismConfig.saveFailed')
    expect(w.text()).not.toContain('SECRET'); expect(w.text()).not.toContain('prismConfig.savedNotice')
    expect(w.get<HTMLInputElement>('[data-testid="prism-http_cache"]').element.checked).toBe(true); w.unmount()
  })
  it('offers retry after an initial read failure without showing default configuration', async () => {
    vi.mocked(getPrismSettings).mockRejectedValueOnce(new Error('secret'))
    const w = mountPage(); await flushPromises(); expect(w.find('form').exists()).toBe(false)
    expect(w.text()).toContain('prismConfig.loadFailed'); expect(w.text()).not.toContain('secret')
    await w.get('[data-testid="prism-refresh"]').trigger('click'); await flushPromises(); expect(w.find('form').exists()).toBe(true); w.unmount()
  })
})
