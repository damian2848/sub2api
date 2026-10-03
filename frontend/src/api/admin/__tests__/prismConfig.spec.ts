import { beforeEach, describe, expect, it, vi } from 'vitest'
import { getPrismSettings, savePrismSettings, resetPrismSettings, validPrismStartupConfig, type PrismStartupConfig } from '../prismConfig'
const client = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn(), delete: vi.fn() }))
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
