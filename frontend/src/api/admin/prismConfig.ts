import { apiClient } from '../client'

export interface PrismStartupConfig {
  project_isolation: boolean
  http_cache: boolean
  multiplex_pages: boolean
  prewarm_chat: boolean
  stream_reasoning: boolean
  memory_limit_mib: number
  memory_reserve_mib: number
}

export interface PrismConfigurationState {
  effective: PrismStartupConfig
  desired: PrismStartupConfig
  restart_required: boolean
  source: 'environment' | 'saved'
  apply_mode: 'restart'
}

export interface PrismSettings {
  gateway: {
    enabled: boolean
    configured: boolean
    base_url: string
    management_key_configured: boolean
  }
  configuration: PrismConfigurationState | null
  availability: 'ready' | 'not_configured' | 'unsupported' | 'unavailable'
}

export const prismBooleanFields = [
  'project_isolation', 'http_cache', 'multiplex_pages', 'prewarm_chat', 'stream_reasoning'
] as const
export const prismMemoryFields = ['memory_limit_mib', 'memory_reserve_mib'] as const
export const prismMemoryMaximum = 1048576

export function validPrismStartupConfig(value: unknown): value is PrismStartupConfig {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false
  const config = value as Record<string, unknown>
  if (Object.keys(config).length !== 7 || !prismBooleanFields.every(key => typeof config[key] === 'boolean')) return false
  if (!prismMemoryFields.every(key => typeof config[key] === 'number' && Number.isInteger(config[key]) && (config[key] as number) >= 0 && (config[key] as number) <= prismMemoryMaximum)) return false
  return config.memory_limit_mib === 0 || (config.memory_limit_mib as number) > (config.memory_reserve_mib as number)
}

// Fail closed on old/incompatible sidecars: never populate a writable form with invented defaults.
function parseSettings(value: unknown): PrismSettings {
  const result = value as PrismSettings | undefined
  const gateway = result?.gateway
  if (!gateway || typeof gateway.enabled !== 'boolean' || typeof gateway.configured !== 'boolean' ||
      typeof gateway.management_key_configured !== 'boolean' || typeof gateway.base_url !== 'string' ||
      !['ready', 'not_configured', 'unsupported', 'unavailable'].includes(result?.availability ?? '')) {
    throw new Error('Invalid Prism settings response')
  }
  const config = result!.configuration
  if (result!.availability === 'ready') {
    if (!config || !validPrismStartupConfig(config.effective) || !validPrismStartupConfig(config.desired) ||
        typeof config.restart_required !== 'boolean' || !['environment', 'saved'].includes(config.source) || config.apply_mode !== 'restart') {
      throw new Error('Invalid Prism configuration response')
    }
  } else if (config !== null) {
    throw new Error('Invalid Prism configuration response')
  }
  return result!
}

export async function getPrismSettings(signal?: AbortSignal): Promise<PrismSettings> {
  const { data } = await apiClient.get('/admin/settings/prism', { signal })
  return parseSettings(data)
}

export async function savePrismSettings(settings: PrismStartupConfig, signal?: AbortSignal): Promise<PrismSettings> {
  if (!validPrismStartupConfig(settings)) throw new Error('Invalid Prism configuration')
  const { data } = await apiClient.put('/admin/settings/prism', settings, { signal })
  const result = parseSettings(data)
  if (result.availability !== 'ready') throw new Error('Prism configuration save was not confirmed')
  return result
}

export async function resetPrismSettings(signal?: AbortSignal): Promise<PrismSettings> {
  const { data } = await apiClient.delete('/admin/settings/prism', { signal })
  const result = parseSettings(data)
  if (result.availability !== 'ready') throw new Error('Prism configuration reset was not confirmed')
  return result
}
