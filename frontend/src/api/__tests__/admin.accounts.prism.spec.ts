import { beforeEach, describe, expect, it, vi } from 'vitest'

const { get, post } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: { get, post } }))
import { createPrism, getPrismStatus, reconnectPrism } from '@/api/admin/accounts'

describe('Prism account API', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    get.mockResolvedValue({ data: { phase: 'ready' } })
    post.mockResolvedValue({ data: { phase: 'provisioning' } })
  })

  it('sends only creation options with a bounded timeout and cancellation signal', async () => {
    const signal = new AbortController().signal
    await createPrism(32, { name: 'Prism', group_ids: [1] }, signal)
    expect(post).toHaveBeenCalledWith('/admin/accounts/32/prism', { name: 'Prism', group_ids: [1] }, { signal, timeout: 15000 })
  })

  it('uses cancellable status and reconnect endpoints', async () => {
    const signal = new AbortController().signal
    await getPrismStatus(45, signal)
    await reconnectPrism(45, signal)
    expect(get).toHaveBeenCalledWith('/admin/accounts/45/prism/status', { signal, timeout: 15000 })
    expect(post).toHaveBeenCalledWith('/admin/accounts/45/prism/reconnect', undefined, { signal, timeout: 15000 })
  })
})
