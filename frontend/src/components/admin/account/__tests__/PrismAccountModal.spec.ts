import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import PrismAccountModal from '../PrismAccountModal.vue'
import type { Account } from '@/types'
import type { PrismStatus } from '@/api/admin/accounts'

const { createPrism, getPrismStatus, reconnectPrism } = vi.hoisted(() => ({
  createPrism: vi.fn(), getPrismStatus: vi.fn(), reconnectPrism: vi.fn()
}))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { createPrism, getPrismStatus, reconnectPrism } } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const source = { id: 32, name: 'Source', group_ids: [4, 7], credentials: { access_token: 'must-not-send' } } as Account
const managed = { id: 45, name: 'Source (Prism)' } as Account
const pending: PrismStatus = { phase: 'provisioning', ready: false, models: [], source_account_id: 32, account_id: 45, enabled: true }
const ready: PrismStatus = { ...pending, phase: 'ready', ready: true, models: ['gpt-6.1-sol', 'gpt-6-luna'] }

function mountModal(mode: 'create' | 'status' | 'reconnect' = 'status') {
  return mount(PrismAccountModal, {
    props: { show: true, account: mode === 'create' ? source : managed, mode },
    global: {
      stubs: {
        BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' },
        Icon: true
      }
    }
  })
}

describe('PrismAccountModal', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.clearAllMocks()
    createPrism.mockResolvedValue({ account: managed, status: pending })
    getPrismStatus.mockResolvedValue(ready)
    reconnectPrism.mockResolvedValue(pending)
  })
  afterEach(() => { vi.useRealTimers() })

  it('creates once with name and source groups, without sending credentials, then polls actual models', async () => {
    const wrapper = mountModal('create')
    await wrapper.get('#prism-account-name').setValue('Prism Work')
    await wrapper.get('[data-testid="prism-create"]').trigger('click')
    await flushPromises()
    expect(createPrism).toHaveBeenCalledTimes(1)
    expect(createPrism).toHaveBeenCalledWith(32, { name: 'Prism Work', group_ids: [4, 7] }, expect.any(AbortSignal))
    expect(wrapper.get('[data-testid="prism-phase"]').text()).toContain('provisioning')
    expect(wrapper.find('[data-testid="prism-create"]').exists()).toBe(false)
    expect(wrapper.emitted('updated')).toHaveLength(1)

    await vi.advanceTimersByTimeAsync(5000)
    expect(getPrismStatus).toHaveBeenCalledWith(45, expect.any(AbortSignal))
    expect(wrapper.get('[data-testid="prism-models"]').text()).toContain('gpt-6.1-sol')
    expect(wrapper.get('[data-testid="prism-models"]').text()).not.toContain('astra')
    expect(wrapper.emitted('updated')).toHaveLength(2)
    wrapper.unmount()
  })

  it('allows creating without inheriting groups and guards repeated clicks during the request', async () => {
    let resolve!: (value: { account: Account; status: PrismStatus }) => void
    createPrism.mockReturnValue(new Promise(done => { resolve = done }))
    const wrapper = mountModal('create')
    await wrapper.get('input[type="checkbox"]').setValue(false)
    const button = wrapper.get('[data-testid="prism-create"]')
    await button.trigger('click')
    expect(button.attributes('disabled')).toBeDefined()
    await button.trigger('click')
    expect(createPrism).toHaveBeenCalledTimes(1)
    expect(createPrism.mock.calls[0][1]).toEqual({ name: 'Source (Prism)', group_ids: [] })
    resolve({ account: managed, status: pending })
    await flushPromises()
    wrapper.unmount()
  })

  it('keeps the default name within the account name limit for long source names', async () => {
    const wrapper = mountModal('create')
    const longName = '\u{1F680}'.repeat(100)
    await wrapper.setProps({ account: { ...source, id: 33, name: longName } })
    const expectedName = `${'\u{1F680}'.repeat(92)} (Prism)`
    expect((wrapper.get('#prism-account-name').element as HTMLInputElement).value).toBe(expectedName)
    expect(Array.from(expectedName)).toHaveLength(100)
    await wrapper.get('[data-testid="prism-create"]').trigger('click')
    await flushPromises()
    expect(createPrism).toHaveBeenCalledWith(33, { name: expectedName, group_ids: [4, 7] }, expect.any(AbortSignal))
    wrapper.unmount()
  })

  it('stops polling on close and aborts an in-flight status request', async () => {
    const wrapper = mountModal()
    await flushPromises()
    getPrismStatus.mockReturnValue(new Promise(() => {}))
    await vi.advanceTimersByTimeAsync(5000)
    const signal = getPrismStatus.mock.calls[1][1] as AbortSignal
    await wrapper.setProps({ show: false })
    expect(signal.aborted).toBe(true)
    await vi.advanceTimersByTimeAsync(30000)
    expect(getPrismStatus).toHaveBeenCalledTimes(2)
    wrapper.unmount()
  })

  it('clears polling and aborts requests on unmount', async () => {
    const wrapper = mountModal()
    await flushPromises()
    getPrismStatus.mockReturnValue(new Promise(() => {}))
    await vi.advanceTimersByTimeAsync(5000)
    const signal = getPrismStatus.mock.calls[1][1] as AbortSignal
    wrapper.unmount()
    expect(signal.aborted).toBe(true)
    await vi.advanceTimersByTimeAsync(30000)
    expect(getPrismStatus).toHaveBeenCalledTimes(2)
  })

  it('does not schedule stale responses after the modal closes', async () => {
    let resolve!: (value: PrismStatus) => void
    getPrismStatus.mockReturnValue(new Promise(done => { resolve = done }))
    const wrapper = mountModal()
    await wrapper.setProps({ show: false })
    resolve(ready)
    await flushPromises()
    await vi.advanceTimersByTimeAsync(30000)
    expect(getPrismStatus).toHaveBeenCalledTimes(1)
    expect(wrapper.emitted('updated')).toBeUndefined()
    wrapper.unmount()
  })

  it('shows adapter configuration failure, disables reconnect, and stops polling', async () => {
    getPrismStatus.mockResolvedValue({ ...pending, phase: 'disabled', enabled: false })
    const wrapper = mountModal()
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('notConfigured')
    expect(wrapper.get('[data-testid="prism-reconnect"]').attributes('disabled')).toBeDefined()
    await vi.advanceTimersByTimeAsync(30000)
    expect(getPrismStatus).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })

  it('keeps timeout recovery available without repeating creation automatically', async () => {
    createPrism.mockRejectedValueOnce({ code: 'ECONNABORTED' })
    const wrapper = mountModal('create')
    await wrapper.get('[data-testid="prism-create"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('requestTimeout')
    await vi.advanceTimersByTimeAsync(30000)
    expect(createPrism).toHaveBeenCalledTimes(1)
    await wrapper.get('[data-testid="prism-create"]').trigger('click')
    await flushPromises()
    expect(createPrism).toHaveBeenCalledTimes(2)
    expect(createPrism.mock.calls[0][1]).toEqual(createPrism.mock.calls[1][1])
    wrapper.unmount()
  })

  it('reconnects an existing account explicitly and resumes initialization polling', async () => {
    const wrapper = mountModal('reconnect')
    await flushPromises()
    expect(reconnectPrism).toHaveBeenCalledWith(45, expect.any(AbortSignal))
    expect(wrapper.get('[data-testid="prism-reconnect"]').attributes('disabled')).toBeDefined()
    await vi.advanceTimersByTimeAsync(5000)
    expect(getPrismStatus).toHaveBeenCalledTimes(1)
    expect(wrapper.get('[data-testid="prism-reconnect"]').attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })
})
