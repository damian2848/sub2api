import { beforeEach, describe, expect, it, vi } from 'vitest'
import { getPrismSettings, savePrismSettings, resetPrismSettings, getPrismRestartStatus, restartPrismSettings, validPrismStartupConfig, type PrismStartupConfig } from '../prismConfig'
const client = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn(), delete: vi.fn(), post: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: client }))
const config: PrismStartupConfig = { project_isolation: false, http_cache: false, multiplex_pages: false, prewarm_chat: true, stream_reasoning: true, memory_limit_mib: 0, memory_reserve_mib: 32 }
const ready = { gateway: { enabled: true, configured: true, base_url: 'http://prism:8080', management_key_configured: true }, availability: 'ready', configuration: { effective: config, desired: config, source: 'environment', apply_mode: 'restart', restart_required: false } }
beforeEach(() => { vi.resetAllMocks(); for (const method of Object.values(client)) method.mockResolvedValue({ data: structuredClone(ready) }) })
describe('Prism configuration API', () => {
  it('uses the authenticated gateway only, with cancellation and no URL/key payload', async () => {
    const signal = new AbortController().signal
    expect(await getPrismSettings(signal)).toEqual(ready)
    await savePrismSettings(config, signal); await resetPrismSettings(signal)
    expect(client.get).toHaveBeenCalledWith('/admin/settings/prism', { signal })
    expect(client.put).toHaveBeenCalledWith('/admin/settings/prism', config, { signal })
    expect(client.delete).toHaveBeenCalledWith('/admin/settings/prism', { signal })
  })
  it.each([
    { ...config, http_cache: 'false' }, { ...config, extra: true }, { ...config, memory_limit_mib: -1 },
    { ...config, memory_limit_mib: 1048577 }, { ...config, memory_reserve_mib: 1.5 },
    { ...config, memory_limit_mib: 32, memory_reserve_mib: 32 }, { ...config, prewarm_chat: null }
  ])('rejects invalid or extra startup fields without a request (%j)', async value => {
    expect(validPrismStartupConfig(value)).toBe(false)
    await expect(savePrismSettings(value as PrismStartupConfig)).rejects.toThrow()
    expect(client.put).not.toHaveBeenCalled()
  })
  it('permits reserve with the admission guard off and a valid positive limit', () => {
    expect(validPrismStartupConfig(config)).toBe(true)
    expect(validPrismStartupConfig({ ...config, memory_limit_mib: 33 })).toBe(true)
    const { stream_reasoning: _omitted, ...missing } = config
    expect(validPrismStartupConfig(missing)).toBe(false)
  })
  it.each([
    {}, { ...ready, configuration: null }, { ...ready, availability: 'unsupported' },
    { ...ready, configuration: { ...ready.configuration, apply_mode: 'live' } },
    { ...ready, configuration: { ...ready.configuration, desired: { ...config, memory_limit_mib: 3 } } },
    { ...ready, gateway: { ...ready.gateway, management_key_configured: 'yes' } }
  ])('fails closed on incompatible responses', async data => {
    client.get.mockResolvedValue({ data })
    await expect(getPrismSettings()).rejects.toThrow()
  })
  it('allows a known unavailable envelope without inventing configuration', async () => {
    const data = { ...ready, availability: 'unsupported', configuration: null }
    client.get.mockResolvedValue({ data })
    expect(await getPrismSettings()).toEqual(data)
  })
  it('never announces persistence for an unavailable mutation response, even with HTTP 200', async () => {
    const data = { ...ready, availability: 'unavailable', configuration: null }
    client.put.mockResolvedValue({ data }); client.delete.mockResolvedValue({ data })
    await expect(savePrismSettings(config)).rejects.toThrow()
    await expect(resetPrismSettings()).rejects.toThrow()
  })
})

const runtimeId = 'c3cfb0b9-9c3f-48d1-8d7d-3c63a4af12e8'
const restartReady = { availability: 'ready', runtime: { supported: true, runtime_id: runtimeId, state: 'ready' } }
const restartAccepted = { ...restartReady, runtime: { ...restartReady.runtime, state: 'restarting' } }
const restartRequest = { expected_runtime_id: runtimeId, expected_configuration: config }
describe('Prism restart API', () => {
  it('uses a separate capability probe and sends the complete optimistic-concurrency snapshot', async () => {
    const signal = new AbortController().signal
    client.get.mockResolvedValue({ data: restartReady })
    client.post.mockResolvedValue({ status: 202, data: restartAccepted })
    expect(await getPrismRestartStatus(signal)).toEqual(restartReady)
    expect(await restartPrismSettings(restartRequest, signal)).toEqual(restartAccepted)
    expect(client.get).toHaveBeenCalledWith('/admin/settings/prism/restart', { signal })
    expect(client.post).toHaveBeenCalledWith('/admin/settings/prism/restart', restartRequest, { signal, _retry: true })
  })
  it.each(['unsupported', 'unavailable', 'not_configured'])('accepts %s only without runtime metadata', async availability => {
    client.get.mockResolvedValue({ data: { availability, runtime: null } })
    expect(await getPrismRestartStatus()).toEqual({ availability, runtime: null })
    client.get.mockResolvedValue({ data: { availability, runtime: restartReady.runtime } })
    await expect(getPrismRestartStatus()).rejects.toThrow()
  })
  it.each([
    {}, { availability: 'ready', runtime: null }, { availability: 'other', runtime: null },
    { ...restartReady, secret: 'private' }, { ...restartReady, runtime: { ...restartReady.runtime, extra: true } },
    { ...restartReady, runtime: { ...restartReady.runtime, supported: false } },
    { ...restartReady, runtime: { ...restartReady.runtime, runtime_id: 'not-a-uuid' } },
    { ...restartReady, runtime: { ...restartReady.runtime, runtime_id: runtimeId.toUpperCase() } },
    { ...restartReady, runtime: { ...restartReady.runtime, runtime_id: 'c3cfb0b9-9c3f-18d1-8d7d-3c63a4af12e8' } },
    { ...restartReady, runtime: { ...restartReady.runtime, runtime_id: 'c3cfb0b9-9c3f-48d1-7d7d-3c63a4af12e8' } },
    { ...restartReady, runtime: { ...restartReady.runtime, state: 'stopping' } },
    { availability: 'unsupported' }
  ])('fails closed on malformed restart capabilities (%j)', async data => {
    client.get.mockResolvedValue({ data })
    await expect(getPrismRestartStatus()).rejects.toThrow()
  })
  it.each([
    { status: 202, data: { ...restartAccepted, runtime: { ...restartAccepted.runtime, runtime_id: 'ee25a5f8-d522-4bb4-9078-30c1160f21cc' } } },
    { status: 200, data: restartAccepted }, { status: 202, data: restartReady },
    { status: 202, data: { availability: 'unsupported', runtime: null } }
  ])('does not treat an incompatible POST response as acceptance (%j)', async response => {
    client.post.mockResolvedValue(response)
    await expect(restartPrismSettings(restartRequest)).rejects.toThrow()
    expect(client.post).toHaveBeenCalledTimes(1)
  })
  it.each([
    { ...restartRequest, expected_runtime_id: 'old' },
    { ...restartRequest, expected_configuration: { ...config, extra: true } },
    { ...restartRequest, extra: true }
  ])('refuses incomplete or incompatible restart snapshots before POST (%j)', async request => {
    await expect(restartPrismSettings(request)).rejects.toThrow()
    expect(client.post).not.toHaveBeenCalled()
  })
  it.each([401, 409, 403, 503, 0])('does not retry a failed POST (%s)', async status => {
    client.post.mockRejectedValue({ status, reason: 'PRISM_RESTART_CONFLICT', message: 'secret' })
    await expect(restartPrismSettings(restartRequest)).rejects.toEqual(expect.objectContaining({ status }))
    expect(client.post).toHaveBeenCalledTimes(1)
  })
})
