import { afterEach, describe, expect, it } from 'vitest'
import { computed } from 'vue'
import { CONCRETE_PLATFORM_OPTIONS, GROUP_PLATFORM_OPTIONS } from '@/constants/platforms'
import {
  BUILTIN_PLATFORM_CATALOG,
  compositePrecedencePlatformIds,
  listPlatformIds,
  platformDisplayName,
  resetPlatformCatalog,
  setPlatformCatalog
} from '@/constants/platformCatalog'
import { platformLabel } from '@/utils/platformColors'
import { normalizePlatformQuotasMap, sanitizePlatformQuotasMap } from '@/api/admin/settings'
import { platformQuotaPlatforms } from '@/api/admin/users'

const concretePlatforms = [
  'anthropic',
  'openai',
  'gemini',
  'antigravity',
  'grok',
  'kimi',
  'zhipu',
  'deepseek',
  'minimax',
  'opencode_go',
  'typesafe'
]

describe('platform option catalogs', () => {
  it('exposes every concrete account platform', () => {
    expect(CONCRETE_PLATFORM_OPTIONS.map((option) => option.value)).toEqual(concretePlatforms)
  })

  it('adds composite for group-backed filters', () => {
    expect(GROUP_PLATFORM_OPTIONS.map((option) => option.value)).toEqual([
      ...concretePlatforms,
      'composite'
    ])
  })
})

describe('platform catalog with a newly registered platform', () => {
  const withNewProvider = {
    platforms: [
      ...BUILTIN_PLATFORM_CATALOG.platforms,
      { id: 'command_code', display_name: 'Command Code', gateway: 'openai', cn_provider: false }
    ],
    composite_precedence: [...BUILTIN_PLATFORM_CATALOG.composite_precedence, 'command_code']
  }

  afterEach(() => {
    resetPlatformCatalog()
  })

  it('keeps the built-in catalog equal to the legacy literals', () => {
    expect(listPlatformIds()).toEqual(concretePlatforms)
    expect(compositePrecedencePlatformIds()).toEqual([
      'anthropic', 'gemini', 'openai', 'antigravity', 'grok',
      'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go', 'typesafe'
    ])
    expect(CONCRETE_PLATFORM_OPTIONS.map((option) => option.label)).toEqual([
      'Anthropic', 'OpenAI', 'Gemini', 'Antigravity', 'Grok',
      'Kimi', 'Zhipu GLM', 'DeepSeek', 'MiniMax', 'OpenCode', 'TypeSafe / Jev'
    ])
    expect(platformLabel('zhipu')).toBe('Zhipu GLM')
    expect(platformLabel('composite')).toBe('Composite')
    expect(platformLabel('')).toBe('API')
  })

  it('adds newly registered platforms to options, labels and quota lists reactively', () => {
    const optionValues = computed(() => CONCRETE_PLATFORM_OPTIONS.map((option) => option.value))
    expect(optionValues.value).not.toContain('command_code')

    setPlatformCatalog(withNewProvider)

    expect(optionValues.value).toEqual([...concretePlatforms, 'command_code'])
    expect(GROUP_PLATFORM_OPTIONS.map((option) => option.value)).toEqual([
      ...concretePlatforms,
      'command_code',
      'composite'
    ])
    expect(platformLabel('command_code')).toBe('Command Code')
    expect(platformDisplayName('command_code')).toBe('Command Code')
    expect(platformLabel('unregistered')).toBe('unregistered')
    expect(platformQuotaPlatforms()).toContain('command_code')
    expect(Object.keys(normalizePlatformQuotasMap())).toEqual([...concretePlatforms, 'command_code'])
    expect(sanitizePlatformQuotasMap({ command_code: { daily: 5, weekly: -1, monthly: null } }).command_code).toEqual({
      daily: 5,
      weekly: null,
      monthly: null
    })
  })

  it('restores the built-in catalog on reset', () => {
    setPlatformCatalog(withNewProvider)
    resetPlatformCatalog()
    expect(CONCRETE_PLATFORM_OPTIONS.map((option) => option.value)).toEqual(concretePlatforms)
  })
})
