import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import CodexQuickConfigModal from '../CodexQuickConfigModal.vue'

const fetchCodexModelsManifest = vi.hoisted(() => vi.fn())

vi.mock('@/api/codex', () => ({ fetchCodexModelsManifest }))
vi.mock('file-saver', () => ({ saveAs: vi.fn() }))
vi.mock('@/components/common/BaseDialog.vue', () => ({
  default: { template: '<div><slot /><slot name="footer" /></div>' }
}))
vi.mock('@/components/icons/Icon.vue', () => ({ default: { template: '<span />' } }))
vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) => params
      ? `${key}:${JSON.stringify(params)}`
      : key
  })
}))

function mountModal() {
  return mount(CodexQuickConfigModal, {
    props: {
      show: true,
      apiKey: 'sk-test',
      baseUrl: 'https://example.com',
      platform: 'openai'
    }
  })
}

function decodeBase64Utf8(value: string): string {
  const bytes = atob(value).split('').map((character) => character.charCodeAt(0))
  return new TextDecoder().decode(new Uint8Array(bytes))
}

describe('CodexQuickConfigModal', () => {
  beforeEach(() => {
    fetchCodexModelsManifest.mockReset()
    Object.defineProperty(window.navigator, 'userAgent', { configurable: true, value: 'Mozilla/5.0' })
  })

  it('starts with a usable script and switches platforms', async () => {
    const wrapper = mountModal()
    expect(wrapper.get('[data-testid="quick-config-copy"]').attributes('disabled')).toBeUndefined()
    const unixPayload = wrapper.get('pre').text().match(/PAYLOAD='([^']+)'/)?.[1]
    expect(unixPayload).toBeDefined()
    expect(decodeBase64Utf8(unixPayload!)).toContain('experimental_bearer_token')

    await wrapper.get('[data-testid="quick-config-windows-tab"]').trigger('click')
    expect(wrapper.get('pre').text()).toContain('@echo off')
    expect(wrapper.get('[data-testid="quick-config-windows-tab"]').attributes('aria-selected')).toBe('true')
  })

  it('waits for the model catalog before enabling copy and download', async () => {
    let resolveRequest!: (value: { content: string; modelCount: number }) => void
    fetchCodexModelsManifest.mockReturnValue(new Promise((resolve) => { resolveRequest = resolve }))
    const wrapper = mountModal()

    await wrapper.get('[data-testid="quick-config-import-catalog"]').setValue(true)
    await flushPromises()
    expect(wrapper.get('[data-testid="quick-config-catalog-loading"]').exists()).toBe(true)
    expect(wrapper.get('[data-testid="quick-config-copy"]').attributes('disabled')).toBeDefined()

    resolveRequest({ content: '{"models":[]}', modelCount: 0 })
    await flushPromises()
    expect(wrapper.get('[data-testid="quick-config-catalog-ready"]').exists()).toBe(true)
    expect(wrapper.get('[data-testid="quick-config-download"]').attributes('disabled')).toBeUndefined()
  })

  it('shows a recoverable catalog error', async () => {
    fetchCodexModelsManifest.mockRejectedValue(new Error('unavailable'))
    const wrapper = mountModal()
    await wrapper.get('[data-testid="quick-config-import-catalog"]').setValue(true)
    await flushPromises()
    expect(wrapper.get('[data-testid="quick-config-catalog-error"]').exists()).toBe(true)
    expect(wrapper.get('[data-testid="quick-config-copy"]').attributes('disabled')).toBeDefined()
  })
})
