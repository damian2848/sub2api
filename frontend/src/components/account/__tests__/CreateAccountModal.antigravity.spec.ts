import { defineComponent } from 'vue'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

enableAutoUnmount(afterEach)

const { create, fetchDefaults, generateAuthUrl, exchangeCode, refreshToken, syncModels, showInfo } = vi.hoisted(() => ({
  create: vi.fn(),
  fetchDefaults: vi.fn(),
  generateAuthUrl: vi.fn(),
  exchangeCode: vi.fn(),
  refreshToken: vi.fn(),
  syncModels: vi.fn(),
  showInfo: vi.fn(),
}))

vi.mock('@/api/admin/credentialEncryption', () => ({
  getCredentialEncryption: vi.fn().mockResolvedValue({ configured: true, source: 'server_config' }),
  initializeCredentialEncryption: vi.fn(),
}))
vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn(), showWarning: vi.fn(), showInfo }),
}))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ isSimpleMode: true }) }))
vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      getManagementCapabilities: vi.fn().mockResolvedValue({}),
      create,
      syncUpstreamModels: syncModels,
      probeUpstreamBilling: vi.fn().mockResolvedValue({}),
      checkMixedChannelRisk: vi.fn().mockResolvedValue({ has_risk: false }),
    },
    antigravity: { generateAuthUrl, exchangeCode, refreshAntigravityToken: refreshToken },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({}),
    },
    tlsFingerprintProfiles: { list: vi.fn().mockResolvedValue([]) },
  },
}))
vi.mock('@/composables/useModelWhitelist', async () => ({
  ...await vi.importActual<typeof import('@/composables/useModelWhitelist')>('@/composables/useModelWhitelist'),
  fetchAntigravityDefaultMappings: fetchDefaults,
}))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key }),
}))

import CreateAccountModal from '../CreateAccountModal.vue'

const DialogStub = defineComponent({
  props: { show: Boolean },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>',
})
const WhitelistStub = defineComponent({
  name: 'ModelWhitelistSelector',
  props: ['modelValue', 'modelMappings', 'platform', 'syncCredentials'],
  emits: ['update:modelValue', 'upstream-synced'],
  template: '<div data-testid="whitelist-editor" />',
})
const OAuthStub = defineComponent({
  name: 'OAuthAuthorizationFlow',
  data: () => ({ inputMethod: 'manual', authCode: 'test-code', oauthState: 'test-state' }),
  methods: { reset() {} },
  emits: ['generate-url', 'validate-refresh-token'],
  template: '<button data-testid="generate-auth-url" @click="$emit(\'generate-url\')">authorize</button>',
})

function mountModal() {
  return mount(CreateAccountModal, {
    props: { show: true, proxies: [], groups: [] },
    global: {
      stubs: {
        BaseDialog: DialogStub,
        OAuthAuthorizationFlow: OAuthStub,
        ModelWhitelistSelector: WhitelistStub,
        ConfirmDialog: true, Select: true, Icon: true, PlatformIcon: true,
        ProxySelector: true, ProxyAdBanner: true, GroupSelector: true, QuotaLimitCard: true,
      },
    },
  })
}

async function clickText(wrapper: ReturnType<typeof mountModal>, text: string) {
  const button = wrapper.findAll('button').find(candidate => candidate.text().includes(text))
  expect(button, text).toBeDefined()
  await button!.trigger('click')
  await flushPromises()
}

async function openAntigravity(wrapper: ReturnType<typeof mountModal>) {
  await clickText(wrapper, 'Antigravity')
  await wrapper.get('form#create-account-form input[type="text"]').setValue('AG account')
}

async function editBoth(wrapper: ReturnType<typeof mountModal>) {
  wrapper.findComponent(WhitelistStub).vm.$emit('update:modelValue', ['gemini-4-argon-high', 'client-alias'])
  await wrapper.get('[data-testid="antigravity-mapping-tab"]').trigger('click')
  const settings = wrapper.get('[data-testid="antigravity-model-settings"]')
  await settings.get('input[placeholder="admin.accounts.requestModel"]').setValue('client-alias')
  await settings.get('input[placeholder="admin.accounts.actualModel"]').setValue('gemini-3.8-flash-high')
  await wrapper.get('[data-testid="antigravity-whitelist-tab"]').trigger('click')
}

async function submitUpstream(wrapper: ReturnType<typeof mountModal>) {
  await clickText(wrapper, 'admin.accounts.types.antigravityApikey')
  await wrapper.get('input[placeholder="https://cloudcode-pa.googleapis.com"]').setValue('https://relay.example')
  await wrapper.get('form#create-account-form input[type="password"]').setValue('test-key')
  await wrapper.get('form#create-account-form').trigger('submit.prevent')
  await flushPromises()
}

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>(done => { resolve = done })
  return { promise, resolve }
}

const defaults = [
  { from: 'gemini-4-argon-high', to: 'gemini-4-argon-high' },
  { from: 'default-alias', to: 'gemini-4-argon-high' },
]
const combined = {
  'gemini-4-argon-high': 'gemini-4-argon-high',
  'client-alias': 'gemini-3.8-flash-high',
}

describe('CreateAccountModal Antigravity combined model settings', () => {
  beforeEach(() => {
    fetchDefaults.mockReset().mockResolvedValue(defaults)
    create.mockReset().mockResolvedValue({ id: 42, platform: 'antigravity', type: 'apikey' })
    syncModels.mockReset().mockResolvedValue({ models: [] })
    showInfo.mockReset()
    generateAuthUrl.mockReset().mockResolvedValue({
      auth_url: 'https://example.test/auth', session_id: 'test-session', state: 'test-state',
    })
    const tokens = { access_token: 'test-access', refresh_token: 'test-refresh', project_id: 'test-project' }
    exchangeCode.mockReset().mockResolvedValue(tokens)
    refreshToken.mockReset().mockResolvedValue(tokens)
  })

  it('splits defaults and keeps both edited views when switching tabs', async () => {
    const wrapper = mountModal()
    await openAntigravity(wrapper)
    expect(wrapper.findComponent(WhitelistStub).props('modelValue')).toEqual(['gemini-4-argon-high'])
    expect(wrapper.findComponent(WhitelistStub).props('modelMappings')).toEqual([defaults[1]])
    await editBoth(wrapper)
    expect(wrapper.findComponent(WhitelistStub).props('modelValue')).toEqual(['gemini-4-argon-high', 'client-alias'])
    expect(wrapper.findComponent(WhitelistStub).props('modelMappings')).toEqual([
      { from: 'client-alias', to: 'gemini-3.8-flash-high' },
    ])
    await wrapper.get('[data-testid="antigravity-mapping-tab"]').trigger('click')
    expect(wrapper.get('input[placeholder="admin.accounts.requestModel"]').element).toHaveProperty('value', 'client-alias')
  })

  it.each(['apikey', 'authorization code', 'refresh token batch'])('saves both sides through %s creation, with mapping precedence', async path => {
    const wrapper = mountModal()
    await openAntigravity(wrapper)
    await editBoth(wrapper)
    if (path === 'apikey') {
      await submitUpstream(wrapper)
    } else {
      await wrapper.get('form#create-account-form').trigger('submit.prevent')
      await flushPromises()
      if (path === 'authorization code') {
        await wrapper.get('[data-testid="generate-auth-url"]').trigger('click')
        await flushPromises()
        await clickText(wrapper, 'admin.accounts.oauth.completeAuth')
      } else {
        wrapper.findComponent(OAuthStub).vm.$emit('validate-refresh-token', 'first-refresh\nsecond-refresh')
        await flushPromises()
      }
    }
    expect(create).toHaveBeenCalledTimes(path === 'refresh token batch' ? 2 : 1)
    for (const [payload] of create.mock.calls) {
      expect(payload.platform).toBe('antigravity')
      expect(payload.type).toBe(path === 'apikey' ? 'apikey' : 'oauth')
      expect(payload.credentials.model_mapping).toEqual(combined)
    }
  })

  it('passes combined settings to upstream model preview and preserves aliases after sync', async () => {
    const wrapper = mountModal()
    await openAntigravity(wrapper)
    await editBoth(wrapper)
    await clickText(wrapper, 'admin.accounts.types.antigravityApikey')
    await wrapper.get('input[placeholder="https://cloudcode-pa.googleapis.com"]').setValue('https://relay.example')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('test-key')
    const whitelist = wrapper.findComponent(WhitelistStub)
    expect(whitelist.props('syncCredentials')).toMatchObject({
      platform: 'antigravity', type: 'apikey', base_url: 'https://relay.example', api_key: 'test-key',
      model_mapping: combined,
    })
    whitelist.vm.$emit('update:modelValue', ['gemini-4-argon-high', 'synced-model'])
    whitelist.vm.$emit('upstream-synced')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()
    expect(create.mock.calls[0][0].credentials.model_mapping).toEqual({
      'gemini-4-argon-high': 'gemini-4-argon-high', 'synced-model': 'synced-model',
      'client-alias': 'gemini-3.8-flash-high',
    })
    expect(syncModels).toHaveBeenCalledWith(42)
  })

  it('leaves both empty to use system defaults and retains mapping presets', async () => {
    const wrapper = mountModal()
    await openAntigravity(wrapper)
    wrapper.findComponent(WhitelistStub).vm.$emit('update:modelValue', [])
    await wrapper.get('[data-testid="antigravity-mapping-tab"]').trigger('click')
    expect(wrapper.get('[data-testid="antigravity-model-settings"]').findAll('button').some(button => button.text().startsWith('+ '))).toBe(true)
    await wrapper.get('[data-testid="antigravity-remove-mapping"]').trigger('click')
    expect(wrapper.text()).toContain('admin.accounts.antigravityDefaultModelsHint')
    await submitUpstream(wrapper)
    expect(create.mock.calls[0][0].credentials).not.toHaveProperty('model_mapping')
  })

  it('puts identity presets in the whitelist and rejects duplicates from either view', async () => {
    fetchDefaults.mockResolvedValueOnce([])
    const wrapper = mountModal()
    await openAntigravity(wrapper)
    await wrapper.get('[data-testid="antigravity-mapping-tab"]').trigger('click')
    await clickText(wrapper, '+ 3.8 Flash High')
    await clickText(wrapper, '+ 3.8 Flash High')
    await clickText(wrapper, '+ Opus 5.5→High')
    await clickText(wrapper, '+ Opus 5.5→High')
    expect(showInfo).toHaveBeenCalledTimes(2)
    await wrapper.get('[data-testid="antigravity-whitelist-tab"]').trigger('click')
    expect(wrapper.findComponent(WhitelistStub).props('modelValue')).toEqual(['gemini-3.8-flash-high'])
    expect(wrapper.findComponent(WhitelistStub).props('modelMappings')).toEqual([
      { from: 'claude-opus-5-5', to: 'claude-opus-5-5-high' },
    ])
    await submitUpstream(wrapper)
    expect(create.mock.calls[0][0].credentials.model_mapping).toEqual({
      'gemini-3.8-flash-high': 'gemini-3.8-flash-high',
      'claude-opus-5-5': 'claude-opus-5-5-high',
    })
  })

  it.each(['whitelist', 'mapping'])('does not overwrite %s edits with late defaults', async side => {
    const pending = deferred<typeof defaults>()
    fetchDefaults.mockReturnValueOnce(pending.promise)
    const wrapper = mountModal()
    await openAntigravity(wrapper)
    if (side === 'whitelist') {
      wrapper.findComponent(WhitelistStub).vm.$emit('update:modelValue', ['manual-model'])
    } else {
      await wrapper.get('[data-testid="antigravity-mapping-tab"]').trigger('click')
      await wrapper.get('[data-testid="antigravity-add-mapping"]').trigger('click')
      await wrapper.get('input[placeholder="admin.accounts.requestModel"]').setValue('manual-alias')
      await wrapper.get('input[placeholder="admin.accounts.actualModel"]').setValue('manual-target')
    }
    pending.resolve(defaults)
    await flushPromises()
    await submitUpstream(wrapper)
    expect(create.mock.calls[0][0].credentials.model_mapping).toEqual(side === 'whitelist'
      ? { 'manual-model': 'manual-model' }
      : { 'manual-alias': 'manual-target' })
  })

  it('ignores stale default responses after switching away and returning', async () => {
    const oldDefaults = deferred<typeof defaults>()
    fetchDefaults.mockReturnValueOnce(oldDefaults.promise)
    const wrapper = mountModal()
    await openAntigravity(wrapper)
    await clickText(wrapper, 'OpenAI')
    await openAntigravity(wrapper)
    await editBoth(wrapper)
    oldDefaults.resolve([{ from: 'stale-model', to: 'stale-model' }])
    await flushPromises()
    await submitUpstream(wrapper)
    expect(create.mock.calls[0][0].credentials.model_mapping).toEqual(combined)
  })

  it('invalidates pending defaults when the modal closes and reopens', async () => {
    const oldDefaults = deferred<typeof defaults>()
    fetchDefaults.mockReturnValueOnce(oldDefaults.promise)
    const wrapper = mountModal()
    await openAntigravity(wrapper)
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    await openAntigravity(wrapper)
    oldDefaults.resolve([{ from: 'closed-dialog-model', to: 'closed-dialog-model' }])
    await flushPromises()
    expect(wrapper.findComponent(WhitelistStub).props('modelValue')).toEqual(['gemini-4-argon-high'])
    expect(wrapper.findComponent(WhitelistStub).props('modelMappings')).toEqual([defaults[1]])
  })
})
