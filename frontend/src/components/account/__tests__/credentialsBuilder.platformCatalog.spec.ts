import { afterEach, describe, expect, it } from 'vitest'

import {
  BUILTIN_PLATFORM_CATALOG,
  resetPlatformCatalog,
  setPlatformCatalog,
  type PlatformCatalog
} from '@/constants/platformCatalog'
import {
  DEFAULT_OPENCODE_GO_PROTOCOL_RULES,
  DEFAULT_OPENCODE_ZEN_PROTOCOL_RULES,
  OPENCODE_GO_ANTHROPIC_BASE_URL,
  OPENCODE_GO_BASE_URL,
  OPENCODE_ZEN_ANTHROPIC_BASE_URL,
  OPENCODE_ZEN_BASE_URL,
  cnSupportsNativeResponses,
  defaultCNAdaptiveBaseUrls,
  defaultCNBaseUrl,
  defaultOpenCodeProtocolRules,
  defaultProviderProtocolRules,
  isHeaderOverrideCapable,
  isMultiProtocolApiKeyPlatform,
  providerAccountModes,
  providerNativeProtocols,
  providerRoutesByModel,
  resolveProviderAccountMode,
  type CnApiProtocol
} from '../credentialsBuilder'

// ===== 改为读取平台清单之前的实现，作为等价基准 =====

const legacyOpenCode = {
  goBase: 'https://opencode.ai/zen/go/v1',
  goAnthropic: 'https://opencode.ai/zen/go',
  zenBase: 'https://opencode.ai/zen/v1',
  zenAnthropic: 'https://opencode.ai/zen'
}

const legacyGoRules = [
  { pattern: 'grok-*', protocol: 'responses' },
  { pattern: 'gpt-*', protocol: 'responses' },
  { pattern: 'muse-spark-*', protocol: 'responses' },
  { pattern: 'minimax-*', protocol: 'anthropic' },
  { pattern: 'qwen*', protocol: 'anthropic' }
]

const legacyZenRules = [
  { pattern: 'grok-*', protocol: 'responses' },
  { pattern: 'gpt-*', protocol: 'responses' },
  { pattern: 'muse-spark-*', protocol: 'responses' },
  { pattern: 'claude-*', protocol: 'anthropic' },
  { pattern: 'qwen*', protocol: 'anthropic' }
]

function legacyIsMultiProtocol(platform: string): boolean {
  return ['kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go'].includes(platform)
}

function legacySupportsResponses(platform: string): boolean {
  return ['deepseek', 'kimi', 'minimax', 'opencode_go'].includes(platform)
}

function legacyHeaderOverride(platform: string, type: string): boolean {
  if (['anthropic', 'openai', 'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go'].includes(platform)) {
    return type === 'apikey'
  }
  if (platform === 'grok') return type === 'apikey' || type === 'oauth'
  return false
}

function legacyDefaultCNBaseUrl(platform: string, mode: string, protocol: CnApiProtocol = 'chat_completions'): string {
  if (protocol === 'anthropic') {
    switch (platform) {
      case 'kimi':
        return mode === 'coding' ? 'https://api.kimi.com/coding' : 'https://api.moonshot.cn/anthropic'
      case 'zhipu':
        return 'https://open.bigmodel.cn/api/anthropic'
      case 'deepseek':
        return 'https://api.deepseek.com/anthropic'
      case 'minimax':
        return 'https://api.minimaxi.com/anthropic'
      case 'opencode_go':
        return mode === 'zen' ? legacyOpenCode.zenAnthropic : legacyOpenCode.goAnthropic
      default:
        return ''
    }
  }
  switch (platform) {
    case 'kimi':
      return mode === 'coding' ? 'https://api.kimi.com/coding/v1' : 'https://api.moonshot.cn/v1'
    case 'zhipu':
      return mode === 'coding' ? 'https://open.bigmodel.cn/api/coding/paas/v4' : 'https://open.bigmodel.cn/api/paas/v4'
    case 'deepseek':
      return 'https://api.deepseek.com'
    case 'minimax':
      return 'https://api.minimaxi.com/v1'
    case 'opencode_go':
      return mode === 'zen' ? legacyOpenCode.zenBase : legacyOpenCode.goBase
    default:
      return ''
  }
}

function legacyAdaptiveBaseUrls(platform: string, mode: string) {
  return {
    chat_completions: legacyDefaultCNBaseUrl(platform, mode, 'chat_completions'),
    anthropic: legacyDefaultCNBaseUrl(platform, mode, 'anthropic'),
    responses: legacySupportsResponses(platform) ? legacyDefaultCNBaseUrl(platform, mode, 'responses') : ''
  }
}

// 各平台在旧实现中可取到的接入模式（旧实现对未知模式回落默认模式）。
const legacyModes: Record<string, string[]> = {
  kimi: ['payg', 'coding'],
  zhipu: ['payg', 'coding'],
  deepseek: ['payg'],
  minimax: ['payg', 'coding'],
  opencode_go: ['go', 'zen']
}

const probePlatforms = [
  ...BUILTIN_PLATFORM_CATALOG.platforms.map(spec => spec.id),
  'composite',
  '',
  'bogus'
]
const probeTypes = ['apikey', 'oauth', 'setup-token', 'upstream', 'bedrock', '']
const protocols: CnApiProtocol[] = ['adaptive', 'chat_completions', 'anthropic', 'responses']

afterEach(() => {
  resetPlatformCatalog()
})

describe('credentialsBuilder derives multi-protocol data from the platform catalog', () => {
  it('matches the legacy predicates for every platform', () => {
    for (const platform of probePlatforms) {
      expect(isMultiProtocolApiKeyPlatform(platform), platform).toBe(legacyIsMultiProtocol(platform))
      expect(cnSupportsNativeResponses(platform), platform).toBe(legacySupportsResponses(platform))
      for (const type of probeTypes) {
        expect(isHeaderOverrideCapable(platform, type), `${platform}/${type}`).toBe(legacyHeaderOverride(platform, type))
      }
    }
  })

  it('matches the legacy default endpoints for every platform, mode and protocol', () => {
    for (const [platform, modes] of Object.entries(legacyModes)) {
      for (const mode of [...modes, 'unknown-mode']) {
        const legacyMode = modes.includes(mode) ? mode : modes[0]
        for (const protocol of protocols) {
          expect(defaultCNBaseUrl(platform, mode, protocol), `${platform}/${mode}/${protocol}`).toBe(
            legacyDefaultCNBaseUrl(platform, legacyMode, protocol)
          )
        }
        expect(defaultCNAdaptiveBaseUrls(platform, mode), `${platform}/${mode}`).toEqual(
          legacyAdaptiveBaseUrls(platform, legacyMode)
        )
      }
    }
    for (const platform of ['anthropic', 'openai', 'grok', 'bogus']) {
      expect(defaultCNBaseUrl(platform, 'payg', 'chat_completions')).toBe('')
    }
  })

  it('keeps the exported OpenCode constants and default rules', () => {
    expect(OPENCODE_GO_BASE_URL).toBe(legacyOpenCode.goBase)
    expect(OPENCODE_GO_ANTHROPIC_BASE_URL).toBe(legacyOpenCode.goAnthropic)
    expect(OPENCODE_ZEN_BASE_URL).toBe(legacyOpenCode.zenBase)
    expect(OPENCODE_ZEN_ANTHROPIC_BASE_URL).toBe(legacyOpenCode.zenAnthropic)
    expect(DEFAULT_OPENCODE_GO_PROTOCOL_RULES).toEqual(legacyGoRules)
    expect(DEFAULT_OPENCODE_ZEN_PROTOCOL_RULES).toEqual(legacyZenRules)
    expect(defaultOpenCodeProtocolRules('go')).toEqual(legacyGoRules)
    expect(defaultOpenCodeProtocolRules('zen')).toEqual(legacyZenRules)
    expect(defaultOpenCodeProtocolRules()).toEqual(legacyGoRules)
  })

  it('exposes routing, modes and native protocols of built-in providers', () => {
    expect(providerRoutesByModel('opencode_go')).toBe(true)
    expect(providerRoutesByModel('kimi')).toBe(false)
    expect(providerAccountModes('opencode_go')).toEqual(['go', 'zen'])
    expect(providerAccountModes('deepseek')).toEqual(['payg'])
    expect(providerNativeProtocols('zhipu', 'coding')).toEqual(['chat_completions', 'anthropic'])
    expect(resolveProviderAccountMode('kimi', 'coding')).toBe('coding')
    expect(resolveProviderAccountMode('deepseek', 'coding')).toBe('payg')
    expect(resolveProviderAccountMode('opencode_go', undefined)).toBe('go')
  })
})

describe('credentialsBuilder picks up newly registered providers from the catalog', () => {
  const serverCatalog: PlatformCatalog = {
    platforms: [
      ...BUILTIN_PLATFORM_CATALOG.platforms,
      {
        id: 'command_code',
        display_name: 'Command Code',
        gateway: 'openai',
        cn_provider: false,
        multi_protocol: {
          default_mode: 'standard',
          routing: 'by_model',
          modes: [
            {
              mode: 'standard',
              base_urls: {
                chat_completions: 'https://api.commandcode.ai/provider/v1',
                anthropic: 'https://api.commandcode.ai/provider'
              },
              protocol_rules: [
                { pattern: 'claude-*', protocol: 'anthropic' },
                { pattern: 'bad-*', protocol: 'not-a-protocol' }
              ]
            }
          ]
        }
      }
    ],
    composite_precedence: [...BUILTIN_PLATFORM_CATALOG.composite_precedence, 'command_code']
  }

  it('treats the new provider as a multi-protocol API-key platform', () => {
    expect(isMultiProtocolApiKeyPlatform('command_code')).toBe(false)
    setPlatformCatalog(serverCatalog)

    expect(isMultiProtocolApiKeyPlatform('command_code')).toBe(true)
    expect(isHeaderOverrideCapable('command_code', 'apikey')).toBe(true)
    expect(isHeaderOverrideCapable('command_code', 'oauth')).toBe(false)
    expect(cnSupportsNativeResponses('command_code')).toBe(false)
    expect(providerNativeProtocols('command_code')).toEqual(['chat_completions', 'anthropic'])
    expect(providerRoutesByModel('command_code')).toBe(true)
    expect(resolveProviderAccountMode('command_code', 'payg')).toBe('standard')
    expect(defaultCNBaseUrl('command_code', 'standard', 'anthropic')).toBe('https://api.commandcode.ai/provider')
    expect(defaultCNBaseUrl('command_code', 'standard', 'responses')).toBe('https://api.commandcode.ai/provider/v1')
    expect(defaultCNAdaptiveBaseUrls('command_code', 'standard')).toEqual({
      chat_completions: 'https://api.commandcode.ai/provider/v1',
      anthropic: 'https://api.commandcode.ai/provider',
      responses: ''
    })
    // 非法协议的规则被丢弃。
    expect(defaultProviderProtocolRules('command_code', 'standard')).toEqual([
      { pattern: 'claude-*', protocol: 'anthropic' }
    ])
  })
})
