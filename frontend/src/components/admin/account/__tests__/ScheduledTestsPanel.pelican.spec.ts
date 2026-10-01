import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ScheduledTestsPanel from '../ScheduledTestsPanel.vue'
import { adminAPI } from '@/api/admin'
const { showError } = vi.hoisted(() => ({ showError: vi.fn() }))
vi.mock('vue-i18n', async () => ({ ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'), useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError, showSuccess: vi.fn() }) }))
vi.mock('@/api/admin', () => ({ adminAPI: { scheduledTests: { listByAccount: vi.fn(), listResults: vi.fn(), getResult: vi.fn(), create: vi.fn(), update: vi.fn(), delete: vi.fn() } } }))
const config = { prompt: 'draw a pelican', reasoning_effort: 'medium', parallel_count: 2 }
const plan = { id: 4, account_id: 42, model_id: 'gpt-6-astra', cron_expression: '*/30 * * * *', enabled: true, max_results: 100, auto_recover: true, pelican_config: config }
function mountPanel(pelican = true, overrides: Record<string, unknown> = {}) {
  return mount(ScheduledTestsPanel, { props: { show: true, embedded: pelican, accountId: 42, modelOptions: [], defaultModel: 'gpt-6-astra', ...(pelican ? { pelicanConfig: config } : {}), ...overrides }, global: { stubs: { BaseDialog: { template: '<div><slot /></div>' }, ConfirmDialog: true, Select: true, Input: true, Toggle: true, Icon: true, HelpTooltip: true, PelicanTestFields: true } } })
}
describe('shared scheduled test plans for Pelican', () => {
  beforeEach(() => { vi.useFakeTimers(); vi.mocked(adminAPI.scheduledTests.listByAccount).mockResolvedValue([]); vi.mocked(adminAPI.scheduledTests.listResults).mockResolvedValue([]) })
  afterEach(() => { vi.clearAllMocks(); vi.useRealTimers() })
  it('uses existing cron, retention, enabled and auto-recovery controls', async () => {
    vi.mocked(adminAPI.scheduledTests.create).mockResolvedValue(plan as any)
    const wrapper = mountPanel(); await flushPromises()
    const vm = wrapper.vm as any
    vm.newPlan.cron_expression = '0 */2 * * *'
    vm.newPlan.auto_recover = true
    vm.newPlan.max_results = '100'
    await vm.handleCreate()
    expect(adminAPI.scheduledTests.create).toHaveBeenCalledWith({ account_id: 42, model_id: 'gpt-6-astra', cron_expression: '0 */2 * * *', enabled: true, auto_recover: true, max_results: 100, pelican_config: config })
    wrapper.unmount()
  })
  it('edits saved Pelican settings, pauses, previews and deletes using the existing plan APIs', async () => {
    vi.mocked(adminAPI.scheduledTests.listByAccount).mockResolvedValue([plan, { id: 1 }] as any)
    vi.mocked(adminAPI.scheduledTests.update).mockResolvedValue({ ...plan, enabled: false } as any)
    const result = { id: 9, plan_id: 4, response_text: '<html></html>' }
    vi.mocked(adminAPI.scheduledTests.getResult).mockResolvedValue(result as any)
    const wrapper = mountPanel(); await flushPromises()
    const vm = wrapper.vm as any
    expect(vm.plans).toHaveLength(1)
    vm.startEdit(plan); vm.editPelican.prompt = 'updated question'
    await vm.handleEdit()
    expect(adminAPI.scheduledTests.update).toHaveBeenCalledWith(4, expect.objectContaining({ cron_expression: '*/30 * * * *', max_results: 100, auto_recover: true, pelican_config: { ...config, prompt: 'updated question' } }))
    await vm.handleToggleEnabled(plan, false)
    expect(adminAPI.scheduledTests.update).toHaveBeenLastCalledWith(4, { enabled: false })
    await vm.toggleExpand(4)
    expect(adminAPI.scheduledTests.listResults).toHaveBeenCalledWith(4, 20, false)
    await vm.previewResult(result)
    expect(wrapper.emitted('preview')?.[0]).toEqual([result])
    vm.confirmDeletePlan(plan); await vm.handleDelete()
    expect(adminAPI.scheduledTests.delete).toHaveBeenCalledWith(4)
    wrapper.unmount()
    await vi.advanceTimersByTimeAsync(30000)
    expect(adminAPI.scheduledTests.listByAccount).toHaveBeenCalledTimes(1)
  })
  it('persists candy question kind in new and edited plans', async () => {
    vi.mocked(adminAPI.scheduledTests.create).mockResolvedValue(plan as any)
    vi.mocked(adminAPI.scheduledTests.update).mockResolvedValue(plan as any)
    const wrapper = mountPanel(); await flushPromises()
    const vm = wrapper.vm as any
    vm.newPelican = { ...config, question_kind: 'candy', prompt: 'candy question' }
    await vm.handleCreate()
    expect(adminAPI.scheduledTests.create).toHaveBeenLastCalledWith(expect.objectContaining({ pelican_config: { ...config, question_kind: 'candy', prompt: 'candy question' } }))
    vm.startEdit({ ...plan, pelican_config: { ...config, question_kind: 'candy' } })
    await vm.handleEdit()
    expect(adminAPI.scheduledTests.update).toHaveBeenLastCalledWith(4, expect.objectContaining({ pelican_config: { ...config, question_kind: 'candy' } }))
    wrapper.unmount()
  })
  it('labels state probe plans and their results by verdict', async () => {
    const probe = { prompt: '', reasoning_effort: 'medium', parallel_count: 1, question_kind: 'state_probe' }
    vi.mocked(adminAPI.scheduledTests.listByAccount).mockResolvedValue([{ ...plan, auto_recover: false, pelican_config: probe }] as any)
    vi.mocked(adminAPI.scheduledTests.listResults).mockResolvedValue([
      { id: 3, plan_id: 4, status: 'failed', error_message: 'state_degraded', response_text: '', pelican_config: probe },
      { id: 2, plan_id: 4, status: 'failed', error_message: 'state_probe_inconclusive: network', response_text: '', pelican_config: probe },
      { id: 1, plan_id: 4, status: 'success', error_message: '', response_text: '', pelican_config: probe }
    ] as any)
    const wrapper = mountPanel(); await flushPromises()
    expect(wrapper.find('[data-testid="state-probe-plan-badge"]').exists()).toBe(true)
    expect(wrapper.findAll('[data-testid="result-status"]').map(badge => badge.text())).toEqual([
      'admin.accounts.pelicanTest.probe.verdictDegraded',
      'admin.accounts.pelicanTest.probe.verdictInconclusive',
      'admin.accounts.pelicanTest.probe.verdictHealthy'
    ])
    // 只有「无法判断」保留错误详情（失败分类）；降智不再把内部标记当错误展示。
    expect(wrapper.text().split('admin.scheduledTests.errorMessage')).toHaveLength(2)
    wrapper.unmount()
  })
  it('keeps ordinary connection tests free of Pelican options', async () => {
    const wrapper = mountPanel(false); await flushPromises()
    const vm = wrapper.vm as any
    await vm.handleCreate()
    expect(adminAPI.scheduledTests.create).toHaveBeenCalledWith(expect.not.objectContaining({ pelican_config: expect.anything() }))
    expect(vm.newPlan.max_results).toBe('100')
    wrapper.unmount()
  })

  it('uses a restricted catalog selector for Prism Pelican plans', async () => {
    const options = [{ value: 'gpt-5.6-sol', label: 'gpt-5.6-sol' }]
    const wrapper = mountPanel(true, { restrictModels: true, modelOptions: options, defaultModel: 'gpt-5.6-sol' })
    await flushPromises()
    ;(wrapper.vm as any).showAddForm = true
    await flushPromises()
    const selector = wrapper.findComponent({ name: 'Select' })
    expect(selector.exists()).toBe(true)
    expect(selector.props('options')).toEqual(options)
    expect((wrapper.vm as any).newPlan.model_id).toBe('gpt-5.6-sol')
    wrapper.unmount()
  })

  it('refuses to create or save a model outside the restricted catalog', async () => {
    const wrapper = mountPanel(true, { restrictModels: true, modelOptions: [{ value: 'gpt-5.6-sol', label: 'Sol' }] })
    await flushPromises()
    const vm = wrapper.vm as any
    vm.newPlan.model_id = 'gpt-6-astra'
    await vm.handleCreate()
    vm.startEdit(plan)
    await vm.handleEdit()
    expect(adminAPI.scheduledTests.create).not.toHaveBeenCalled()
    expect(adminAPI.scheduledTests.update).not.toHaveBeenCalled()
    expect(showError).toHaveBeenCalledWith('admin.accounts.prism.modelUnavailable')
    wrapper.unmount()
  })

  it('uses updated catalog options for new plans and requires an explicit replacement for stale edits', async () => {
    const wrapper = mountPanel(true, { restrictModels: true, modelOptions: [{ value: 'gpt-5.6-sol', label: 'Sol' }], defaultModel: 'gpt-5.6-sol' })
    await flushPromises()
    const vm = wrapper.vm as any
    vm.startEdit({ ...plan, model_id: 'gpt-5.6-sol' })
    await wrapper.setProps({ modelOptions: [{ value: 'gpt-6-luna', label: 'Luna' }], defaultModel: 'gpt-6-luna' })
    expect(vm.newPlan.model_id).toBe('gpt-6-luna')
    await vm.handleEdit()
    expect(adminAPI.scheduledTests.update).not.toHaveBeenCalled()
    vm.editForm.model_id = 'gpt-6-luna'
    vi.mocked(adminAPI.scheduledTests.update).mockResolvedValue({ ...plan, model_id: 'gpt-6-luna' } as any)
    await vm.handleEdit()
    expect(adminAPI.scheduledTests.update).toHaveBeenCalledWith(4, expect.objectContaining({ model_id: 'gpt-6-luna' }))
    wrapper.unmount()
  })

  it('allows pausing an unsupported plan but requires a catalog model before enabling it', async () => {
    const wrapper = mountPanel(true, { restrictModels: true, modelOptions: [{ value: 'gpt-5.6-sol', label: 'Sol' }] })
    await flushPromises()
    await (wrapper.vm as any).handleToggleEnabled(plan, true)
    expect(adminAPI.scheduledTests.update).not.toHaveBeenCalled()
    await (wrapper.vm as any).handleToggleEnabled(plan, false)
    expect(adminAPI.scheduledTests.update).toHaveBeenCalledWith(4, { enabled: false })
    wrapper.unmount()
  })

  it('continues to permit a custom model when restriction is omitted', async () => {
    const wrapper = mountPanel()
    await flushPromises()
    ;(wrapper.vm as any).newPlan.model_id = 'custom-model'
    await (wrapper.vm as any).handleCreate()
    expect(adminAPI.scheduledTests.create).toHaveBeenCalledWith(expect.objectContaining({ model_id: 'custom-model' }))
    wrapper.unmount()
  })
})
