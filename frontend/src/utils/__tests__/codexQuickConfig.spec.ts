import { describe, expect, it } from 'vitest'
import {
  buildCodexQuickConfigToml,
  buildMacLinuxCodexQuickConfigScript,
  buildWindowsCmdCodexQuickConfigScript,
  encodeUtf8Base64,
  normalizeCodexBaseUrl
} from '@/utils/codexQuickConfig'

function decodeBase64Utf8(value: string): string {
  const bytes = atob(value).split('').map((character) => character.charCodeAt(0))
  return new TextDecoder().decode(new Uint8Array(bytes))
}

describe('codexQuickConfig', () => {
  it('normalizes the API base and escapes TOML values', () => {
    expect(normalizeCodexBaseUrl(' https://example.com/// ')).toBe('https://example.com/v1')
    expect(normalizeCodexBaseUrl('https://example.com/v1')).toBe('https://example.com/v1')

    const config = buildCodexQuickConfigToml({
      apiKey: 'sk-quick-"test"',
      baseUrl: 'https://example.com',
      platform: 'openai'
    })
    expect(config).toContain('experimental_bearer_token = "sk-quick-\\"test\\""')
    expect(config).toContain('base_url = "https://example.com/v1"')
    expect(config).not.toContain('model_catalog_json')
  })

  it('embeds model catalog data only when requested', () => {
    const catalog = '{"models":[{"slug":"gpt-test"}]}'
    const config = buildCodexQuickConfigToml({
      apiKey: 'sk-catalog',
      baseUrl: 'https://example.com/v1',
      modelCatalogContent: catalog,
      modelCatalogPath: '~/.codex/codex-models.json'
    })
    expect(config).toContain('model_catalog_json = "~/.codex/codex-models.json"')

    const script = buildMacLinuxCodexQuickConfigScript({
      apiKey: 'sk-catalog',
      baseUrl: 'https://example.com',
      modelCatalogContent: catalog
    })
    const payload = script.match(/CATALOG_PAYLOAD='([^']+)'/)?.[1]
    expect(payload).toBeDefined()
    expect(decodeBase64Utf8(payload!)).toBe(catalog)
    expect(script).toContain('codex-models.json')
  })

  it('generates rerunnable Unix and Windows scripts without exposing raw key text', () => {
    const input = { apiKey: 'sk-secret-value', baseUrl: 'https://example.com', platform: 'openai' as const }
    const unixScript = buildMacLinuxCodexQuickConfigScript(input)
    const windowsScript = buildWindowsCmdCodexQuickConfigScript(input)

    expect(unixScript).toContain('#!/usr/bin/env bash')
    expect(unixScript).toContain('mv -f')
    expect(windowsScript).toContain('@echo off')
    expect(windowsScript).toContain('powershell.exe -NoLogo -NoProfile -NonInteractive')
    expect(windowsScript).toContain('move /Y')
    expect(unixScript).not.toContain('sk-secret-value')
    expect(windowsScript).not.toContain('sk-secret-value')

    const payload = windowsScript.match(/set "SUB2API_CODEX_PAYLOAD=([^\"]+)"/)?.[1]
    expect(payload).toBeDefined()
    expect(decodeBase64Utf8(payload!)).toContain('experimental_bearer_token')
    expect(encodeUtf8Base64('中文')).not.toBe('')
  })
})
