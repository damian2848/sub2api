import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { reactive, nextTick } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import ChannelPrismView from '../ChannelPrismView.vue'
import { getPrismSettings, savePrismSettings, getPrismRestartStatus, restartPrismSettings, type PrismSettings, type PrismRestartStatus } from '@/api/admin/prismConfig'
import en from '@/i18n/locales/en/prismConfig'
import zh from '@/i18n/locales/zh/prismConfig'
const state = vi.hoisted(() => ({ auth: null as any, translations: null as Record<string, any> | null }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => state.auth }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => state.translations ? key.split('.').reduce((value: any, part) => value?.[part], state.translations) ?? key : key }) }))
vi.mock('@/api/client', () => ({ apiClient: {} }))
vi.mock('@/api/admin/prismConfig', async importOriginal => ({
  ...await importOriginal<typeof import('@/api/admin/prismConfig')>(),
  getPrismSettings: vi.fn(), savePrismSettings: vi.fn(), resetPrismSettings: vi.fn(), getPrismRestartStatus: vi.fn(), restartPrismSettings: vi.fn()
}))
const options = { enabled: true, project_isolation: false, http_cache: false, multiplex_pages: false, prewarm_chat: true, stream_reasoning: true, memory_limit_mib: 0, memory_reserve_mib: 32 }
const ready: PrismSettings = { gateway: { enabled: true, configured: true, base_url: 'http://prism:8080', management_key_configured: true }, availability: 'ready', configuration: { effective: { ...options }, desired: { ...options }, source: 'environment', apply_mode: 'restart', restart_required: false } }
const oldRuntime = 'c3cfb0b9-9c3f-48d1-8d7d-3c63a4af12e8'
const newRuntime = 'ee25a5f8-d522-4bb4-9078-30c1160f21cc'
function runtime(runtime_id = oldRuntime, state: 'ready' | 'restarting' = 'ready'): PrismRestartStatus {
  return { availability: 'ready', runtime: { supported: true, runtime_id, state } }
}
const wrappers: ReturnType<typeof mount>[] = []
function mountPage() {
  const wrapper = mount(ChannelPrismView, { global: { stubs: {
    RouterLink: { template: '<a><slot /></a>' },
    ConfirmDialog: {
      props: ['show', 'title', 'message'], emits: ['confirm', 'cancel'],
      template: '<div v-if="show" data-testid="confirmation"><h3>{{ title }}</h3><p>{{ message }}</p><button data-testid="confirm" @click="$emit(\'confirm\')" /><button data-testid="cancel" @click="$emit(\'cancel\')" /></div>'
    }
  } } })
  wrappers.push(wrapper)
  return wrapper
}
beforeEach(() => {
  vi.resetAllMocks(); state.translations = null; state.auth = reactive({ user: { id: 1, role: 'admin' } })
  vi.mocked(getPrismSettings).mockResolvedValue(structuredClone(ready))
  vi.mocked(getPrismRestartStatus).mockResolvedValue(runtime())
  vi.mocked(restartPrismSettings).mockResolvedValue(runtime(oldRuntime, 'restarting'))
  vi.mocked(savePrismSettings).mockImplementation(async desired => ({ ...structuredClone(ready), configuration: { ...structuredClone(ready.configuration!), desired, source: 'saved', restart_required: true } }))
})
afterEach(async () => {
  for (const wrapper of wrappers.splice(0)) if (wrapper.exists()) wrapper.unmount()
  await flushPromises()
  vi.clearAllTimers(); vi.useRealTimers()
})
async function tick(ms = 1000) { await vi.advanceTimersByTimeAsync(ms); await flushPromises() }
async function confirmedRestart(wrapper: ReturnType<typeof mount>) {
  await wrapper.get('[data-testid="prism-restart"]').trigger('click')
  await wrapper.get('[data-testid="confirm"]').trigger('click')
  await flushPromises()
}

describe('Prism managed restart', () => {
  it('requires a second confirmation, can cancel, and permits restart without pending changes', async () => {
    const w = mountPage(); await flushPromises()
    expect(w.get('[data-testid="prism-restart"]').attributes('disabled')).toBeUndefined()
    await w.get('[data-testid="prism-restart"]').trigger('click')
    expect(w.get('[data-testid="confirmation"]').text()).toContain('prismConfig.restartMessage')
    expect(restartPrismSettings).not.toHaveBeenCalled()
    await w.get('[data-testid="cancel"]').trigger('click')
    expect(w.find('[data-testid="confirmation"]').exists()).toBe(false)
    expect(restartPrismSettings).not.toHaveBeenCalled()
  })
  it('never saves unsaved changes on restart, and closes confirmation if the draft changes', async () => {
    const w = mountPage(); await flushPromises()
    await w.get('[data-testid="prism-http_cache"]').setValue(true)
    expect(w.get('[data-testid="prism-restart"]').attributes('disabled')).toBeDefined()
    expect(w.text()).toContain('prismConfig.restartSaveFirst')
    await w.get('[data-testid="prism-restart"]').trigger('click')
    expect(w.find('[data-testid="confirmation"]').exists()).toBe(false)
    await w.get('[data-testid="prism-http_cache"]').setValue(false)
    await w.get('[data-testid="prism-restart"]').trigger('click')
    await w.get('[data-testid="prism-stream_reasoning"]').setValue(false)
    expect(w.find('[data-testid="confirmation"]').exists()).toBe(false)
    expect(w.text()).toContain('prismConfig.restartChanged')
    expect(restartPrismSettings).not.toHaveBeenCalled(); expect(savePrismSettings).not.toHaveBeenCalled()
  })
  it.each(['runtime', 'desired'] as const)('cancels confirmation if refreshed %s changed', async change => {
    const w = mountPage(); await flushPromises()
    await w.get('[data-testid="prism-restart"]').trigger('click')
    if (change === 'runtime') vi.mocked(getPrismRestartStatus).mockResolvedValue(runtime(newRuntime))
    else vi.mocked(getPrismSettings).mockResolvedValue({ ...ready, configuration: { ...ready.configuration!, desired: { ...options, http_cache: true } } })
    await w.get('[data-testid="prism-refresh"]').trigger('click'); await flushPromises()
    expect(w.find('[data-testid="confirmation"]').exists()).toBe(false)
    expect(w.text()).toContain('prismConfig.restartChanged'); expect(restartPrismSettings).not.toHaveBeenCalled()
  })
  it('requires a new ready boot plus verified effective and desired config, then clears pending', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    const expected = { ...options, http_cache: true }
    const pending: PrismSettings = { ...ready, configuration: { ...ready.configuration!, desired: expected, source: 'saved', restart_required: true } }
    vi.mocked(getPrismSettings).mockResolvedValue(pending)
    const w = mountPage(); await flushPromises()
    await confirmedRestart(w)
    expect(restartPrismSettings).toHaveBeenCalledWith({ expected_runtime_id: oldRuntime, expected_configuration: expected }, expect.any(AbortSignal))
    await tick(2000)
    expect(w.text()).not.toContain('prismConfig.restartSucceeded')
    expect(getPrismSettings).toHaveBeenCalledTimes(1)
    expect(w.text()).toContain('prismConfig.restartWaiting')
    vi.mocked(getPrismRestartStatus).mockResolvedValue(runtime(newRuntime, 'restarting'))
    await tick()
    expect(w.text()).not.toContain('prismConfig.restartSucceeded')
    vi.mocked(getPrismRestartStatus).mockResolvedValue(runtime(newRuntime))
    let resolve!: (value: PrismSettings) => void
    vi.mocked(getPrismSettings).mockImplementation(() => new Promise(done => { resolve = done }))
    await tick()
    expect(w.text()).toContain('prismConfig.restartVerifying')
    expect(w.text()).not.toContain('prismConfig.restartSucceeded')
    resolve({ ...pending, configuration: { ...pending.configuration!, effective: expected, restart_required: false } }); await flushPromises()
    expect(w.text()).toContain('prismConfig.restartSucceeded')
    expect(w.find('[data-testid="prism-restart-required"]').exists()).toBe(false)
    expect(w.get('[data-testid="prism-effective-http_cache"]').text()).toContain('prismConfig.enabled')
    expect(vi.getTimerCount()).toBe(0)
  })
  it('rechecks runtime after config verification and waits if another boot began meanwhile', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    const w = mountPage(); await flushPromises(); await confirmedRestart(w)
    vi.mocked(getPrismRestartStatus).mockResolvedValue(runtime(newRuntime))
    let resolve!: (value: PrismSettings) => void
    vi.mocked(getPrismSettings).mockImplementationOnce(() => new Promise(done => { resolve = done }))
    await tick()
    expect(w.text()).toContain('prismConfig.restartVerifying')
    const thirdRuntime = 'e340c89b-269b-4dcb-9d32-bc617d9e275e'
    vi.mocked(getPrismRestartStatus).mockResolvedValue(runtime(thirdRuntime, 'restarting'))
    resolve(ready); await flushPromises()
    expect(w.text()).not.toContain('prismConfig.restartSucceeded')
    expect(w.text()).toContain('prismConfig.restartWaiting')
    vi.mocked(getPrismRestartStatus).mockResolvedValue(runtime(thirdRuntime))
    await tick()
    expect(w.text()).toContain('prismConfig.restartSucceeded')
    expect(restartPrismSettings).toHaveBeenCalledTimes(1)
  })
  it('protects duplicate confirms and clicks with exactly one POST', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    let resolve!: (value: PrismRestartStatus) => void
    vi.mocked(restartPrismSettings).mockImplementation(() => new Promise(done => { resolve = done }))
    const w = mountPage(); await flushPromises()
    await w.get('[data-testid="prism-restart"]').trigger('click')
    const confirm = w.get('[data-testid="confirm"]')
    await confirm.trigger('click'); await confirm.trigger('click')
    await w.get('[data-testid="prism-restart"]').trigger('click')
    expect(restartPrismSettings).toHaveBeenCalledTimes(1)
    resolve(runtime(oldRuntime, 'restarting')); await flushPromises(); await tick(2000)
    expect(restartPrismSettings).toHaveBeenCalledTimes(1)
  })
  it.each(['effective', 'desired', 'restart_required'] as const)('does not declare success when verified %s is wrong', async field => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    const w = mountPage(); await flushPromises(); await confirmedRestart(w)
    vi.mocked(getPrismRestartStatus).mockResolvedValue(runtime(newRuntime))
    const config = { ...ready.configuration!, [field]: field === 'restart_required' ? true : { ...options, http_cache: true } }
    vi.mocked(getPrismSettings).mockResolvedValue({ ...ready, configuration: config })
    await tick()
    expect(w.get('[role="alert"]').text()).toBe('prismConfig.restartMismatch')
    expect(w.text()).not.toContain('prismConfig.restartSucceeded')
    expect(w.get<HTMLInputElement>('[data-testid="prism-http_cache"]').element.checked).toBe(false)
    expect(vi.getTimerCount()).toBe(0)
  })
  it('times out without mistaking an unchanged boot and matching HTTP 200 config for success', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    const w = mountPage(); await flushPromises(); await confirmedRestart(w)
    const signal = vi.mocked(restartPrismSettings).mock.calls[0]![1]!
    await tick(120000)
    expect(w.get('[role="alert"]').text()).toBe('prismConfig.restartUnconfirmed')
    expect(w.text()).not.toContain('prismConfig.restartSucceeded')
    expect(getPrismSettings).toHaveBeenCalledTimes(1)
    expect(restartPrismSettings).toHaveBeenCalledTimes(1)
    expect(signal.aborted).toBe(true)
    expect(w.find('form').exists()).toBe(true)
    expect(vi.getTimerCount()).toBe(0)
  })
  it.each([0, 503])('only polls GETs after an ambiguous POST failure (%s), allowing verified recovery', async status => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    vi.mocked(restartPrismSettings).mockRejectedValue({ status, message: 'management_key=SECRET' })
    const w = mountPage(); await flushPromises(); await confirmedRestart(w)
    await tick(2000)
    expect(w.text()).not.toContain('SECRET'); expect(w.text()).not.toContain('prismConfig.restartSucceeded')
    vi.mocked(getPrismRestartStatus).mockResolvedValue(runtime(newRuntime)); await tick()
    expect(w.text()).toContain('prismConfig.restartSucceeded')
    expect(restartPrismSettings).toHaveBeenCalledTimes(1)
  })
  it.each([[409, 'restartConflict'], [403, 'restartDenied']] as const)('does not poll or leak upstream details after a definite %s refusal', async (status, key) => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    vi.mocked(restartPrismSettings).mockRejectedValue({ status, reason: 'PRISM_RESTART_CONFLICT', message: 'SECRET' })
    const w = mountPage(); await flushPromises(); await confirmedRestart(w); await tick(5000)
    expect(w.get('[role="alert"]').text()).toBe('prismConfig.' + key)
    expect(w.text()).not.toContain('SECRET'); expect(getPrismRestartStatus).toHaveBeenCalledTimes(1)
    expect(restartPrismSettings).toHaveBeenCalledTimes(1); expect(vi.getTimerCount()).toBe(0)
  })
  it.each(['unsupported', 'unavailable', 'not_configured'] as const)('leaves legacy config writable while %s restart is safely disabled', async availability => {
    vi.mocked(getPrismRestartStatus).mockResolvedValue({ availability, runtime: null })
    const w = mountPage(); await flushPromises()
    expect(w.get('[data-testid="prism-restart"]').attributes('disabled')).toBeDefined()
    expect(w.text()).toContain('prismConfig.restart' + ({ unsupported: 'Unsupported', unavailable: 'Unavailable', not_configured: 'NotConfigured' }[availability]))
    await w.get('[data-testid="prism-http_cache"]').setValue(true)
    await w.get('form').trigger('submit'); await flushPromises()
    expect(savePrismSettings).toHaveBeenCalledTimes(1); expect(w.text()).toContain('prismConfig.savedNotice')
    expect(restartPrismSettings).not.toHaveBeenCalled()
  })
  it('keeps a saved form on transient config or capability failures and refresh recovers capability', async () => {
    const w = mountPage(); await flushPromises()
    vi.mocked(getPrismSettings).mockRejectedValueOnce({ status: 503, message: 'SECRET' })
    vi.mocked(getPrismRestartStatus).mockRejectedValueOnce({ status: 503, message: 'SECRET' })
    await w.get('[data-testid="prism-refresh"]').trigger('click'); await flushPromises()
    expect(w.find('form').exists()).toBe(true)
    expect(w.text()).toContain('prismConfig.loadFailed'); expect(w.text()).not.toContain('SECRET')
    expect(w.get('[data-testid="prism-restart"]').attributes('disabled')).toBeDefined()
    await w.get('[data-testid="prism-refresh"]').trigger('click'); await flushPromises()
    expect(w.get('[data-testid="prism-restart"]').attributes('disabled')).toBeUndefined()
    vi.mocked(getPrismSettings).mockResolvedValueOnce({ ...ready, availability: 'unavailable', configuration: null })
    await w.get('[data-testid="prism-refresh"]').trigger('click'); await flushPromises()
    expect(w.find('form').exists()).toBe(true); expect(w.text()).toContain('prismConfig.loadFailed')
    expect(w.text()).not.toContain('prismConfig.savedNotice')
  })
  it.each(['observer', 'noauth', 'unmount'] as const)('aborts and stops polling on %s, ignoring a late verification result', async change => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    const w = mountPage(); await flushPromises(); await confirmedRestart(w)
    const signal = vi.mocked(restartPrismSettings).mock.calls[0]![1]!
    vi.mocked(getPrismRestartStatus).mockResolvedValue(runtime(newRuntime))
    let resolve!: (value: PrismSettings) => void
    vi.mocked(getPrismSettings).mockImplementation(() => new Promise(done => { resolve = done }))
    await tick()
    expect(w.text()).toContain('prismConfig.restartVerifying')
    if (change === 'unmount') w.unmount()
    else { state.auth.user = change === 'noauth' ? null : { id: 1, role: 'observer' }; await nextTick() }
    expect(signal.aborted).toBe(true)
    const count = vi.mocked(getPrismRestartStatus).mock.calls.length
    resolve(ready); await flushPromises(); await tick(5000)
    expect(getPrismRestartStatus).toHaveBeenCalledTimes(count)
    if (change !== 'unmount') { expect(w.text()).not.toContain('prismConfig.restartSucceeded'); expect(w.find('form').exists()).toBe(false) }
    expect(vi.getTimerCount()).toBe(0)
  })
  it.each([[en, 'Restart and apply configuration', 'all in-progress and queued Prism requests', 'not the gateway'], [zh, '重启并应用配置', '全部 Prism 正在进行或排队的请求', '不重启网关']] as const)('renders the restart action and disruption warning in both locales', async (messages, action, warning, gateway) => {
    state.translations = { prismConfig: messages }
    const w = mountPage(); await flushPromises()
    expect(w.get('[data-testid="prism-restart"]').text()).toBe(action)
    await w.get('[data-testid="prism-restart"]').trigger('click')
    expect(w.get('[data-testid="confirmation"]').text()).toContain(warning)
    expect(w.get('[data-testid="confirmation"]').text()).toContain(gateway)
    expect(restartPrismSettings).not.toHaveBeenCalled()
  })
})
