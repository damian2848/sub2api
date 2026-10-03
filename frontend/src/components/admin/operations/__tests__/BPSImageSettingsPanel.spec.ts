import { beforeEach, describe, expect, it, vi } from 'vitest'
import { reactive, nextTick } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import BPSImageSettingsPanel from '../BPSImageSettingsPanel.vue'
import { getSettings, updateSettings, type SystemSettings } from '@/api/admin/settings'
import { excelBPSImageSettingKeys, type ExcelBPSImageSettings } from '@/utils/excelBPSImageSettings'
const state = vi.hoisted(() => ({ auth: null as any }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => state.auth }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@/api/admin/settings', () => ({ getSettings: vi.fn(), updateSettings: vi.fn() }))
const image: ExcelBPSImageSettings = { excel_bps_image_mode: 'relay', excel_bps_image_relay_enabled: true, excel_bps_image_base_url: 'https://images.example.com', excel_bps_image_body_limit_mib: 32, excel_bps_image_budget_mib: 512, excel_bps_image_max_requests: 32, excel_bps_image_max_image_mib: 16, excel_bps_image_limit_policy: 'off', excel_bps_image_warning_remaining: 10, excel_bps_image_compact_reserve: 5, excel_bps_image_max_images: 100, excel_bps_image_max_total_mib: 64, excel_bps_image_storage_mib: 1024, excel_bps_image_storage_entries: 512, excel_bps_image_ttl_minutes: 60 }
const settings = { ...image, site_name: 'Unrelated custom name', registration_enabled: false } as SystemSettings
beforeEach(() => {
  vi.resetAllMocks(); state.auth = reactive({ user: { id: 1, role: 'admin' } })
  vi.mocked(getSettings).mockResolvedValue({ ...settings })
  vi.mocked(updateSettings).mockImplementation(async payload => ({ ...settings, ...payload } as SystemSettings))
})
describe('BPSImageSettingsPanel', () => {
  it('reuses the complete settings card and submits only the BPS image slice', async () => {
    const w = mount(BPSImageSettingsPanel); await flushPromises()
    expect(w.find('[data-testid="excel-bps-image-settings"]').exists()).toBe(true)
    expect(w.find('#excel-bps-image-storage-entries').exists()).toBe(true)
    await w.get('#excel-bps-image-base-url').setValue(' https://new.example.com/ ')
    await w.get('#excel-bps-image-budget').setValue(1024)
    await w.get('form').trigger('submit'); await flushPromises()
    expect(updateSettings).toHaveBeenCalledWith({ ...image, excel_bps_image_base_url: 'https://new.example.com', excel_bps_image_budget_mib: 1024 }, expect.any(AbortSignal))
    expect(Object.keys(vi.mocked(updateSettings).mock.calls[0]![0]).sort()).toEqual([...excelBPSImageSettingKeys].sort())
    expect(w.text()).toContain('channelOperations.bpsImageSaved'); w.unmount()
  })
  it('preserves hidden native/relay fields while allowing native mode without a relay origin', async () => {
    const w = mount(BPSImageSettingsPanel); await flushPromises()
    await w.get('#excel-bps-image-base-url').setValue('')
    await w.get('#excel-bps-image-mode').setValue('native')
    expect(w.find('#excel-bps-image-base-url').exists()).toBe(false)
    expect(w.find('#excel-bps-image-storage-entries').exists()).toBe(false)
    await w.get('form').trigger('submit'); await flushPromises()
    expect(updateSettings).toHaveBeenCalledWith({ ...image, excel_bps_image_mode: 'native', excel_bps_image_base_url: '' }, expect.any(AbortSignal)); w.unmount()
  })
  it('validates the same image limits and HTTPS origin as system settings', async () => {
    const w = mount(BPSImageSettingsPanel); await flushPromises()
    await w.get('#excel-bps-image-base-url').setValue('http://invalid.example'); await w.get('form').trigger('submit')
    expect(w.get('[role="alert"]').text()).toContain('invalidBaseUrl'); expect(updateSettings).not.toHaveBeenCalled()
    await w.get('#excel-bps-image-base-url').setValue(image.excel_bps_image_base_url)
    await w.get('#excel-bps-image-budget').setValue(1); await w.get('form').trigger('submit')
    expect(w.get('[role="alert"]').text()).toContain('invalidCapacity'); expect(updateSettings).not.toHaveBeenCalled()
    await w.get('#excel-bps-image-budget').setValue(512)
    await w.get('#bps-image-limit-policy').setValue('warn'); await w.get('#bps-image-compact-reserve').setValue(11)
    await w.get('form').trigger('submit'); expect(w.get('[role="alert"]').text()).toContain('invalidLimits'); w.unmount()
  })
  it('never loads as observer, aborts on role loss and discards late responses', async () => {
    let resolve!: (value: SystemSettings) => void
    vi.mocked(getSettings).mockImplementation(() => new Promise(done => { resolve = done }))
    const w = mount(BPSImageSettingsPanel); await nextTick(); const signal = vi.mocked(getSettings).mock.calls[0]![0]!
    state.auth.user.role = 'observer'; await nextTick(); expect(signal.aborted).toBe(true)
    resolve(settings); await flushPromises(); expect(w.find('form').exists()).toBe(false); w.unmount()
    vi.mocked(getSettings).mockClear(); const observer = mount(BPSImageSettingsPanel); await flushPromises(); expect(getSettings).not.toHaveBeenCalled(); observer.unmount()
  })
  it('does not write default values when loading fails and allows retry', async () => {
    vi.mocked(getSettings).mockRejectedValueOnce(new Error('secret'))
    const w = mount(BPSImageSettingsPanel); await flushPromises(); expect(w.find('form').exists()).toBe(false)
    expect(w.text()).toContain('channelOperations.bpsImageLoadFailed'); expect(w.text()).not.toContain('secret')
    await w.get('button').trigger('click'); await flushPromises(); expect(w.find('form').exists()).toBe(true); w.unmount()
  })
  it('guards duplicate saves, aborts on unmount and uses safe errors', async () => {
    const w = mount(BPSImageSettingsPanel); await flushPromises()
    let reject!: (error: Error) => void
    vi.mocked(updateSettings).mockImplementation(() => new Promise((_done, fail) => { reject = fail }))
    await w.get('form').trigger('submit'); await w.get('form').trigger('submit'); expect(updateSettings).toHaveBeenCalledTimes(1)
    reject(new Error('secret')); await flushPromises(); expect(w.text()).not.toContain('secret'); expect(w.text()).toContain('channelOperations.bpsImageSaveFailed')
    expect(w.find('[role="status"]').exists()).toBe(false)
    vi.mocked(updateSettings).mockImplementation(() => new Promise(() => {}))
    await w.get('form').trigger('submit'); const signal = vi.mocked(updateSettings).mock.calls[1]![1]!; w.unmount(); expect(signal.aborted).toBe(true)
  })
})
