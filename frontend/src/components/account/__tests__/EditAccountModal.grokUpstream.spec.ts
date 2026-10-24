import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'

const {
  updateAccountMock,
  checkMixedChannelRiskMock,
  getByIdMock,
  syncUpstreamModelsMock,
  showWarningMock,
  showSuccessMock,
  showErrorMock,
  authIsSimpleMode
} = vi.hoisted(() => ({
  updateAccountMock: vi.fn(),
  checkMixedChannelRiskMock: vi.fn(),
  getByIdMock: vi.fn(),
  syncUpstreamModelsMock: vi.fn(),
  showWarningMock: vi.fn(),
  showSuccessMock: vi.fn(),
  showErrorMock: vi.fn(),
  authIsSimpleMode: { value: true }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: showErrorMock,
    showSuccess: showSuccessMock,
    showInfo: vi.fn(),
    showWarning: showWarningMock
  })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    get isSimpleMode() {
      return authIsSimpleMode.value
    }
  })
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      getManagementCapabilities: vi.fn().mockResolvedValue({ web_search_enabled: false, account_quota_notify_enabled: false }),
      update: updateAccountMock,
      getById: getByIdMock,
      syncUpstreamModels: syncUpstreamModelsMock,
      checkMixedChannelRisk: checkMixedChannelRiskMock
    },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({})
    },
    tlsFingerprintProfiles: {
      list: vi.fn().mockResolvedValue([])
    }
  }
}))

vi.mock('@/api/admin/accounts', () => ({
  getAntigravityDefaultModelMapping: vi.fn()
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

import EditAccountModal from '../EditAccountModal.vue'

const ModelWhitelistSelectorStub = defineComponent({
  name: 'ModelWhitelistSelector',
  props: {
    modelValue: { type: Array, default: () => [] },
    modelMappings: { type: Array, default: () => [] },
    platform: { type: String, default: '' },
    accountId: { type: Number, default: undefined }
  },
  emits: ['update:modelValue', 'upstream-synced', 'upstream-sync-finished'],
  template: '<div data-testid="model-whitelist-selector"><button type="button" data-testid="emit-upstream-sync-finished" @click="$emit(\'upstream-sync-finished\')">finished</button></div>'
})

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((done) => {
    resolve = done
  })
  return { promise, resolve }
}

function diagnostics(model: string, endpoint = 'https://cli-chat-proxy.grok.com/v1/models') {
  return {
    state: 'fresh',
    endpoint,
    fetched_at: '2026-10-25T11:00:00.000Z',
    expires_at: '2099-01-01T00:00:00.000Z',
    models: [model],
    mappings: [{ requested: 'friendly', upstream: model, status: 'listed' }]
  }
}

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: {
    show: {
      type: Boolean,
      default: false
    }
  },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

function buildGrokOAuthAccount(
  credentials: Record<string, unknown> = {},
  extra: Record<string, unknown> = {}
) {
  return {
    id: 5,
    name: 'Grok OAuth',
    notes: '',
    platform: 'grok',
    type: 'oauth',
    credentials: {
      expires_at: '2027-01-01T00:00:00Z',
      token_type: 'Bearer',
      ...credentials
    },
    credentials_status: { has_access_token: true, has_refresh_token: true },
    extra,
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    rate_multiplier: 1,
    status: 'active',
    group_ids: [],
    expires_at: null,
    auto_pause_on_expired: false
  } as any
}

const mounted: Array<{ unmount: () => void }> = []

function mountModal(account: any) {
  const wrapper = mount(EditAccountModal, {
    props: {
      show: true,
      account,
      proxies: [],
      groups: []
    },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        Select: true,
        Icon: true,
        ProxySelector: true,
        GroupSelector: true,
        ModelWhitelistSelector: ModelWhitelistSelectorStub
      }
    }
  })
  mounted.push(wrapper)
  return wrapper
}

describe('EditAccountModal Grok OAuth upstream config', () => {
  beforeEach(() => {
    authIsSimpleMode.value = true
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    getByIdMock.mockReset()
    getByIdMock.mockResolvedValue({ grok_model_diagnostics: null })
    syncUpstreamModelsMock.mockReset()
    syncUpstreamModelsMock.mockResolvedValue({ models: [], discovery_source: 'upstream' })
    showWarningMock.mockReset()
    showSuccessMock.mockReset()
    showErrorMock.mockReset()
  })

  afterEach(() => {
    for (const wrapper of mounted.splice(0)) wrapper.unmount()
  })

  it('enabling the custom base URL toggle and saving persists base_url', async () => {
    const account = buildGrokOAuthAccount({ base_url: 'https://cli-chat-proxy.grok.com/v1' })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    // 官方地址 → 开关初始为关（视同未定制）
    const toggle = wrapper.get('[data-testid="grok-custom-base-url-toggle"]')
    await toggle.trigger('click')

    const input = wrapper.get('[data-testid="grok-custom-base-url-input"]')
    await input.setValue('https://my-relay.example.com')

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await vi.waitFor(() => expect(updateAccountMock).toHaveBeenCalledTimes(1))

    const payload = updateAccountMock.mock.calls[0]?.[1]
    expect(payload?.credentials?.base_url).toBe('https://my-relay.example.com')
  })

  it('accepts the official API host as a manual endpoint switch and persists it', async () => {
    const account = buildGrokOAuthAccount({ base_url: 'https://cli-chat-proxy.grok.com/v1' })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('[data-testid="grok-custom-base-url-toggle"]').trigger('click')
    await wrapper.get('[data-testid="grok-custom-base-url-input"]').setValue('https://api.x.ai/v1')

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await vi.waitFor(() => expect(updateAccountMock).toHaveBeenCalledTimes(1))

    const payload = updateAccountMock.mock.calls[0]?.[1]
    expect(payload?.credentials?.base_url).toBe('https://api.x.ai/v1')
  })

  it('echoes a stored official API endpoint with the toggle on', async () => {
    const account = buildGrokOAuthAccount({ base_url: 'https://us-west-2.api.x.ai/v1' })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    const input = wrapper.get('[data-testid="grok-custom-base-url-input"]')
    expect((input.element as HTMLInputElement).value).toBe('https://us-west-2.api.x.ai/v1')
  })

  it('fills the input from an endpoint preset chip', async () => {
    const account = buildGrokOAuthAccount({ base_url: 'https://cli-chat-proxy.grok.com/v1' })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('[data-testid="grok-custom-base-url-toggle"]').trigger('click')
    const presets = wrapper.findAll('[data-testid="grok-base-url-preset"]')
    expect(presets.length).toBe(5)

    // 第二个预设为官方 API (api.x.ai/v1)
    await presets[1].trigger('click')
    const input = wrapper.get('[data-testid="grok-custom-base-url-input"]')
    expect((input.element as HTMLInputElement).value).toBe('https://api.x.ai/v1')
  })

  it('loads an existing custom base_url with the toggle on and keeps it on save', async () => {
    const account = buildGrokOAuthAccount({ base_url: 'https://my-relay.example.com' })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    const input = wrapper.get('[data-testid="grok-custom-base-url-input"]')
    expect((input.element as HTMLInputElement).value).toBe('https://my-relay.example.com')

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await vi.waitFor(() => expect(updateAccountMock).toHaveBeenCalledTimes(1))

    const payload = updateAccountMock.mock.calls[0]?.[1]
    expect(payload?.credentials?.base_url).toBe('https://my-relay.example.com')
  })

  it('keeps stored header overrides intact on an untouched save', async () => {
    const account = buildGrokOAuthAccount({
      header_override_enabled: true,
      header_overrides: {
        'user-agent': 'grok-pager/0.2.93',
        'x-grok-client-identifier': 'grok-pager',
        'x-grok-client-version': '0.2.93',
        'x-xai-token-auth': 'xai-grok-cli'
      }
    })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await vi.waitFor(() => expect(updateAccountMock).toHaveBeenCalledTimes(1))

    const payload = updateAccountMock.mock.calls[0]?.[1]
    expect(payload?.credentials?.header_override_enabled).toBe(true)
    expect(payload?.credentials?.header_overrides).toEqual({
      'user-agent': 'grok-pager/0.2.93',
      'x-grok-client-identifier': 'grok-pager',
      'x-grok-client-version': '0.2.93',
      'x-xai-token-auth': 'xai-grok-cli'
    })
  })

  it('shows the client-tool cache switch only for Grok OAuth accounts', () => {
    const grokOAuthWrapper = mountModal(buildGrokOAuthAccount())
    expect(grokOAuthWrapper.find('[data-testid="grok-client-tool-cache-toggle"]').exists()).toBe(true)

    const grokAPIKeyWrapper = mountModal({
      ...buildGrokOAuthAccount(),
      type: 'apikey',
      credentials: { api_key: 'xai-test', base_url: 'https://api.x.ai/v1' }
    })
    expect(grokAPIKeyWrapper.find('[data-testid="grok-client-tool-cache-toggle"]').exists()).toBe(false)

    const openAIOAuthWrapper = mountModal({
      ...buildGrokOAuthAccount(),
      platform: 'openai'
    })
    expect(openAIOAuthWrapper.find('[data-testid="grok-client-tool-cache-toggle"]').exists()).toBe(false)
  })

  it('loads and disables client-tool caching while preserving unrelated extra fields', async () => {
    const account = buildGrokOAuthAccount({}, {
      grok_client_tool_cache_enabled: true,
      custom_setting: 'keep-me'
    })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)
    const toggle = wrapper.get('[data-testid="grok-client-tool-cache-toggle"]')
    expect(toggle.attributes('aria-checked')).toBe('true')

    await toggle.trigger('click')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await vi.waitFor(() => expect(updateAccountMock).toHaveBeenCalledTimes(1))

    const payload = updateAccountMock.mock.calls[0]?.[1]
    expect(payload?.extra?.custom_setting).toBe('keep-me')
    expect(payload?.extra?.grok_client_tool_cache_enabled).toBe(false)
  })

  it('defaults client-tool caching on when the setting is missing and persists explicit true', async () => {
    const account = buildGrokOAuthAccount({}, { custom_setting: 'keep-me' })
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)
    const toggle = wrapper.get('[data-testid="grok-client-tool-cache-toggle"]')
    expect(toggle.attributes('aria-checked')).toBe('true')

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await vi.waitFor(() => expect(updateAccountMock).toHaveBeenCalledTimes(1))

    const payload = updateAccountMock.mock.calls[0]?.[1]
    expect(payload?.extra).toMatchObject({
      grok_client_tool_cache_enabled: true,
      custom_setting: 'keep-me'
    })
  })

  it('keeps an explicit false opt-out when saving an untouched account', async () => {
    const account = buildGrokOAuthAccount(
      {},
      {
        grok_client_tool_cache_enabled: false,
        custom_setting: 'keep-me'
      }
    )
    updateAccountMock.mockResolvedValue(account)

    const wrapper = mountModal(account)
    const toggle = wrapper.get('[data-testid="grok-client-tool-cache-toggle"]')
    expect(toggle.attributes('aria-checked')).toBe('false')

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await vi.waitFor(() => expect(updateAccountMock).toHaveBeenCalledTimes(1))

    const payload = updateAccountMock.mock.calls[0]?.[1]
    expect(payload?.extra).toMatchObject({
      grok_client_tool_cache_enabled: false,
      custom_setting: 'keep-me'
    })
  })

  it('shows malformed cache settings as disabled so the UI matches the fail-closed backend', () => {
    const account = buildGrokOAuthAccount(
      {},
      {
        grok_client_tool_cache_enabled: 'true',
        custom_setting: 'keep-me'
      }
    )

    const wrapper = mountModal(account)
    expect(
      wrapper.get('[data-testid="grok-client-tool-cache-toggle"]').attributes('aria-checked')
    ).toBe('false')
  })

  it('keeps an edited base URL when a saved sync finishes', async () => {
    const account = buildGrokOAuthAccount({ base_url: 'https://my-relay.example.com' })
    getByIdMock.mockResolvedValue({
      ...account,
      grok_model_diagnostics: diagnostics('saved-model', 'https://my-relay.example.com/v1/models')
    })
    const wrapper = mountModal(account)
    await flushPromises()

    const input = wrapper.get('[data-testid="grok-custom-base-url-input"]')
    await input.setValue('https://draft.example.com')
    expect(wrapper.text()).toContain('admin.accounts.grokModelDiagnostics.savedConfigOnly')
    await wrapper.get('[data-testid="emit-upstream-sync-finished"]').trigger('click')
    await flushPromises()

    expect((input.element as HTMLInputElement).value).toBe('https://draft.example.com')
    expect(account.credentials.base_url).toBe('https://my-relay.example.com')
    expect(account.grok_model_diagnostics).toBeUndefined()
    expect(wrapper.props('account').id).toBe(account.id)
    expect(wrapper.text()).toContain('saved-model')
    expect(getByIdMock).toHaveBeenCalledWith(account.id)
  })

  it('submits the draft mapping and does not append catalog ids after refresh or saved-config sync', async () => {
    const account = buildGrokOAuthAccount({
      base_url: 'https://my-relay.example.com',
      model_mapping: { friendly: 'grok-4.7' }
    })
    getByIdMock.mockResolvedValue({
      ...account,
      grok_model_diagnostics: diagnostics('catalog-only-model')
    })
    syncUpstreamModelsMock.mockResolvedValue({
      models: ['grok-composer-2.5-fast', 'extra-catalog-id'],
      discovery_source: 'upstream',
      warnings: [{ code: 'upstream_model_metadata_incomplete', message: 'incomplete' }]
    })
    updateAccountMock.mockResolvedValue(account)
    const wrapper = mountModal(account)
    await flushPromises()

    const target = wrapper.get('input[placeholder="admin.accounts.actualModel"]')
    await target.setValue('grok-4.5')
    await wrapper.get('[data-testid="grok-diagnostics-sync"]').trigger('click')
    await flushPromises()
    const whitelist = wrapper.findAll('button').find((button) => button.text() === 'admin.accounts.modelWhitelist')
    await whitelist!.trigger('click')
    await wrapper.get('[data-testid="emit-upstream-sync-finished"]').trigger('click')
    await flushPromises()
    await wrapper.findAll('button').find((button) => button.text() === 'admin.accounts.modelMapping')!.trigger('click')

    expect((wrapper.get('input[placeholder="admin.accounts.actualModel"]').element as HTMLInputElement).value).toBe('grok-4.5')
    expect(showSuccessMock).not.toHaveBeenCalled()
    expect(showWarningMock).toHaveBeenCalledWith('admin.accounts.syncUpstreamModelsMetadataIncomplete')
    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await vi.waitFor(() => expect(updateAccountMock).toHaveBeenCalledTimes(1))
    const payload = updateAccountMock.mock.calls[0]?.[1]
    expect(payload?.credentials?.model_mapping).toEqual({ friendly: 'grok-4.5' })
    expect(JSON.stringify(payload)).not.toContain('extra-catalog-id')
    expect(JSON.stringify(payload)).not.toContain('catalog-only-model')
  })

  it('keeps the diagnostics panel visible in both whitelist and mapping modes', async () => {
    const wrapper = mountModal(buildGrokOAuthAccount())
    await flushPromises()
    expect(wrapper.find('[data-testid="grok-model-diagnostics"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="grok-diagnostics-sync"]').exists()).toBe(false)

    await wrapper.findAll('button').find((button) => button.text() === 'admin.accounts.modelMapping')!.trigger('click')
    expect(wrapper.find('[data-testid="grok-model-diagnostics"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="grok-diagnostics-sync"]').exists()).toBe(true)

    await wrapper.findAll('button').find((button) => button.text() === 'admin.accounts.modelWhitelist')!.trigger('click')
    expect(wrapper.find('[data-testid="grok-model-diagnostics"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="grok-diagnostics-sync"]').exists()).toBe(false)
  })

  it('does not let a late account response overwrite the current diagnostics', async () => {
    const accountA = buildGrokOAuthAccount({ base_url: 'https://a.example.com' })
    const accountB = { ...buildGrokOAuthAccount({ base_url: 'https://b.example.com' }), id: 9, name: 'Grok B' }
    const first = deferred<any>()
    const second = deferred<any>()
    const calls: number[] = []
    getByIdMock.mockImplementation((id: number) => {
      calls.push(id)
      return calls.length === 1 ? first.promise : second.promise
    })
    const wrapper = mountModal(accountA)
    await wrapper.setProps({ account: accountB })
    first.resolve({ ...accountA, grok_model_diagnostics: diagnostics('late-a-model') })
    await flushPromises()
    expect(wrapper.text()).not.toContain('late-a-model')

    second.resolve({ ...accountB, grok_model_diagnostics: diagnostics('current-b-model') })
    await flushPromises()
    expect(wrapper.text()).toContain('current-b-model')
    expect(wrapper.text()).not.toContain('late-a-model')
    expect(wrapper.props('account').id).toBe(accountB.id)
    expect(accountA.grok_model_diagnostics).toBeUndefined()
    expect(accountB.grok_model_diagnostics).toBeUndefined()
  })

  it('drops a response that arrives after the same account is closed and reopened', async () => {
    const account = buildGrokOAuthAccount()
    const first = deferred<any>()
    getByIdMock.mockImplementation(() => first.promise)
    const wrapper = mountModal(account)
    await wrapper.setProps({ show: false })

    const reopened = deferred<any>()
    getByIdMock.mockImplementation(() => reopened.promise)
    await wrapper.setProps({ show: true })
    first.resolve({ ...account, grok_model_diagnostics: diagnostics('closed-model') })
    await flushPromises()
    expect(wrapper.text()).not.toContain('closed-model')

    reopened.resolve({ ...account, grok_model_diagnostics: diagnostics('reopened-model') })
    await flushPromises()
    expect(wrapper.text()).toContain('reopened-model')
    expect(wrapper.text()).not.toContain('closed-model')
  })

  it('does not apply a response after the modal unmounts', async () => {
    const account = buildGrokOAuthAccount()
    const pending = deferred<any>()
    getByIdMock.mockImplementation(() => pending.promise)
    const wrapper = mountModal(account)
    wrapper.unmount()
    pending.resolve({ ...account, grok_model_diagnostics: diagnostics('unmounted-model') })
    await flushPromises()

    getByIdMock.mockResolvedValue({ ...account, grok_model_diagnostics: diagnostics('mounted-model') })
    const again = mountModal(account)
    await flushPromises()
    expect(again.text()).toContain('mounted-model')
    expect(again.text()).not.toContain('unmounted-model')
  })

  it('keeps the previous diagnostics and shows a refresh failure', async () => {
    const account = buildGrokOAuthAccount()
    getByIdMock
      .mockResolvedValueOnce({ ...account, grok_model_diagnostics: diagnostics('kept-model') })
      .mockRejectedValueOnce(new Error('offline'))
    const wrapper = mountModal(account)
    await flushPromises()
    expect(wrapper.text()).toContain('kept-model')

    await wrapper.get('[data-testid="emit-upstream-sync-finished"]').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('kept-model')
    expect(wrapper.text()).toContain('admin.accounts.grokModelDiagnostics.refreshFailed')
  })

  it('shows diagnostics for a Grok API key and skips non-Grok accounts', async () => {
    const apiKey = {
      ...buildGrokOAuthAccount(),
      type: 'apikey',
      credentials: { api_key: 'xai-test', base_url: 'https://api.x.ai/v1' },
      credentials_status: { has_api_key: true }
    }
    getByIdMock.mockResolvedValue({
      ...apiKey,
      grok_model_diagnostics: diagnostics('apikey-model', 'https://api.x.ai/v1/models')
    })
    const wrapper = mountModal(apiKey)
    await flushPromises()
    expect(wrapper.find('[data-testid="grok-model-diagnostics"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('apikey-model')
    expect(getByIdMock).toHaveBeenCalledWith(apiKey.id)

    const baseUrl = wrapper.get('input[placeholder="https://api.x.ai/v1"]')
    await baseUrl.setValue('https://draft-api.example.com')
    expect(wrapper.text()).toContain('admin.accounts.grokModelDiagnostics.savedConfigOnly')
    expect((baseUrl.element as HTMLInputElement).value).toBe('https://draft-api.example.com')

    getByIdMock.mockClear()
    const openai = mountModal({ ...buildGrokOAuthAccount(), platform: 'openai' })
    await flushPromises()
    expect(openai.find('[data-testid="grok-model-diagnostics"]').exists()).toBe(false)
    expect(getByIdMock).not.toHaveBeenCalled()
  })

  it('sends one saved-config sync from mapping mode and does not toast a live success for fallback', async () => {
    const account = buildGrokOAuthAccount({ model_mapping: { friendly: 'grok-4.7' } })
    const pending = deferred<any>()
    syncUpstreamModelsMock.mockImplementation(() => pending.promise)
    const wrapper = mountModal(account)
    await flushPromises()

    const button = wrapper.get('[data-testid="grok-diagnostics-sync"]')
    await button.trigger('click')
    await button.trigger('click')
    expect(syncUpstreamModelsMock).toHaveBeenCalledTimes(1)
    expect(syncUpstreamModelsMock).toHaveBeenCalledWith(account.id)

    pending.resolve({
      models: ['composer-2.5'],
      discovery_source: 'configured_fallback',
      warnings: [{ code: 'grok_model_catalog_configured_fallback', message: 'fallback' }]
    })
    await flushPromises()
    expect(showWarningMock).toHaveBeenCalledWith('admin.accounts.grokModelDiagnostics.configuredFallback')
    expect(showSuccessMock).not.toHaveBeenCalled()
    expect((wrapper.get('input[placeholder="admin.accounts.actualModel"]').element as HTMLInputElement).value).toBe('grok-4.7')
    expect(getByIdMock.mock.calls.length).toBeGreaterThan(1)
  })

  it('does not toast or replace diagnostics when a saved-config sync resolves for a previous account', async () => {
    const accountA = buildGrokOAuthAccount({ model_mapping: { friendly: 'grok-4.7' } })
    const accountB = {
      ...buildGrokOAuthAccount({ model_mapping: { friendly: 'grok-4.5' } }),
      id: 9,
      name: 'Grok B'
    }
    const pending = deferred<any>()
    getByIdMock.mockImplementation(async (id: number) => ({
      grok_model_diagnostics: diagnostics(id === 9 ? 'account-b-model' : 'account-a-model')
    }))
    syncUpstreamModelsMock.mockImplementation(() => pending.promise)
    const wrapper = mountModal(accountA)
    await flushPromises()
    await wrapper.get('[data-testid="grok-diagnostics-sync"]').trigger('click')
    await wrapper.setProps({ account: accountB })
    await flushPromises()
    showWarningMock.mockClear()
    showErrorMock.mockClear()
    showSuccessMock.mockClear()

    pending.resolve({
      models: ['composer-2.5'],
      discovery_source: 'configured_fallback'
    })
    await flushPromises()
    expect(showWarningMock).not.toHaveBeenCalled()
    expect(showErrorMock).not.toHaveBeenCalled()
    expect(showSuccessMock).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('account-b-model')
    expect(wrapper.text()).not.toContain('account-a-model')
  })

  it('reports a sync failure without dropping the previous diagnostics', async () => {
    const account = buildGrokOAuthAccount({ model_mapping: { friendly: 'grok-4.7' } })
    getByIdMock.mockResolvedValue({
      ...account,
      grok_model_diagnostics: diagnostics('kept-after-sync-error')
    })
    syncUpstreamModelsMock.mockRejectedValue({ message: 'boom' })
    const wrapper = mountModal(account)
    await flushPromises()
    const callsBeforeSync = getByIdMock.mock.calls.length
    await wrapper.get('[data-testid="grok-diagnostics-sync"]').trigger('click')
    await flushPromises()

    expect(showErrorMock).toHaveBeenCalledWith('admin.accounts.syncUpstreamModelsError')
    expect(showSuccessMock).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('kept-after-sync-error')
    expect(getByIdMock.mock.calls.length).toBeGreaterThan(callsBeforeSync)
    expect((wrapper.get('input[placeholder="admin.accounts.actualModel"]').element as HTMLInputElement).value).toBe('grok-4.7')
  })
})
