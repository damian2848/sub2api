import { describe, expect, it } from 'vitest'
import { excelBPSImageSettingsError, excelBPSImageSettingsPayload, type ExcelBPSImageSettings } from '../excelBPSImageSettings'
const valid: ExcelBPSImageSettings = { excel_bps_image_mode: 'relay', excel_bps_image_relay_enabled: true, excel_bps_image_base_url: 'https://images.example', excel_bps_image_body_limit_mib: 32, excel_bps_image_budget_mib: 512, excel_bps_image_max_requests: 32, excel_bps_image_max_image_mib: 16, excel_bps_image_limit_policy: 'warn', excel_bps_image_warning_remaining: 10, excel_bps_image_compact_reserve: 5, excel_bps_image_max_images: 100, excel_bps_image_max_total_mib: 64, excel_bps_image_storage_mib: 1024, excel_bps_image_storage_entries: 512, excel_bps_image_ttl_minutes: 60 }
describe('BPS image settings validation', () => {
  it('keeps origin-only normalization and native mode semantics', () => {
    expect(excelBPSImageSettingsError(valid)).toBeNull()
    expect(excelBPSImageSettingsPayload({ ...valid, excel_bps_image_base_url: ' https://images.example/ ' }).excel_bps_image_base_url).toBe('https://images.example')
    expect(excelBPSImageSettingsError({ ...valid, excel_bps_image_mode: 'native', excel_bps_image_base_url: '' })).toBeNull()
  })
  it.each(['http://images.example', 'https://user:pass@images.example', 'https://images.example/v1', 'https://images.example/?', 'https://images.example/#'])('rejects unsafe/non-origin URLs %s', url => {
    expect(excelBPSImageSettingsError({ ...valid, excel_bps_image_base_url: url })).toContain('invalidBaseUrl')
  })
  it.each([
    { excel_bps_image_body_limit_mib: 65 }, { excel_bps_image_budget_mib: 0 },
    { excel_bps_image_max_requests: 4097 }, { excel_bps_image_body_limit_mib: 1.5 }
  ])('rejects capacity inconsistency %j', fields => {
    expect(excelBPSImageSettingsError({ ...valid, ...fields })).toContain('invalidCapacity')
  })
  it.each([
    { excel_bps_image_compact_reserve: 10 }, { excel_bps_image_warning_remaining: 100 },
    { excel_bps_image_max_total_mib: 15 }, { excel_bps_image_storage_mib: 63 },
    { excel_bps_image_storage_entries: 99 }, { excel_bps_image_ttl_minutes: 10081 },
    { excel_bps_image_max_images: 65537 }, { excel_bps_image_storage_mib: 262145 }
  ])('rejects count/storage/policy limits %j', fields => {
    expect(excelBPSImageSettingsError({ ...valid, ...fields })).toContain('invalidLimits')
  })
})
