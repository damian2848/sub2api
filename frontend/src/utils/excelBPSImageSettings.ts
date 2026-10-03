import type { SystemSettings } from '@/api/admin/settings'
import { excelBPSImageLimits as limits } from './excelBPSImageLimits'

export const excelBPSImageSettingKeys = [
  'excel_bps_image_mode', 'excel_bps_image_relay_enabled', 'excel_bps_image_base_url',
  'excel_bps_image_body_limit_mib', 'excel_bps_image_budget_mib', 'excel_bps_image_max_requests',
  'excel_bps_image_max_image_mib', 'excel_bps_image_limit_policy', 'excel_bps_image_warning_remaining',
  'excel_bps_image_compact_reserve', 'excel_bps_image_max_images', 'excel_bps_image_max_total_mib',
  'excel_bps_image_storage_mib', 'excel_bps_image_storage_entries', 'excel_bps_image_ttl_minutes'
] as const
export type ExcelBPSImageSettings = Pick<SystemSettings, typeof excelBPSImageSettingKeys[number]>

// Only this slice is submitted, so a BPS save cannot overwrite unrelated system settings.
export function pickExcelBPSImageSettings(settings: SystemSettings): ExcelBPSImageSettings {
  return Object.fromEntries(excelBPSImageSettingKeys.map(key => [key, settings[key]])) as ExcelBPSImageSettings
}

export function excelBPSImageSettingsError(form: ExcelBPSImageSettings): string | null {
  if (!['relay', 'native'].includes(form.excel_bps_image_mode) || typeof form.excel_bps_image_relay_enabled !== 'boolean') return 'admin.settings.features.excelBpsImages.invalidLimits'
  const baseUrl = form.excel_bps_image_base_url?.trim()
  if ((form.excel_bps_image_relay_enabled && form.excel_bps_image_mode === 'relay') || baseUrl) {
    try {
      const url = new URL(baseUrl)
      if (url.protocol !== 'https:' || !url.hostname || url.username || url.password ||
          (url.pathname !== '/' && url.pathname !== '') || baseUrl.includes('?') || baseUrl.includes('#')) throw new Error('invalid')
    } catch { return 'admin.settings.features.excelBpsImages.invalidBaseUrl' }
  }
  const integer = (value: number, min: number, max: number) => Number.isInteger(value) && value >= min && value <= max
  if (!integer(form.excel_bps_image_body_limit_mib, 1, limits.bodyMiB) ||
      !integer(form.excel_bps_image_budget_mib, limits.minBudgetMiB, limits.budgetMiB) ||
      form.excel_bps_image_budget_mib < form.excel_bps_image_body_limit_mib * 8 ||
      !integer(form.excel_bps_image_max_requests, 1, limits.requests)) return 'admin.settings.features.excelBpsImages.invalidCapacity'
  if (!['off', 'auto_compact', 'warn'].includes(form.excel_bps_image_limit_policy) ||
      !integer(form.excel_bps_image_max_image_mib, 1, limits.imageMiB) ||
      !integer(form.excel_bps_image_warning_remaining, 1, limits.images) ||
      !integer(form.excel_bps_image_compact_reserve, 1, limits.images) ||
      !integer(form.excel_bps_image_max_images, 1, limits.images) ||
      !integer(form.excel_bps_image_max_total_mib, 1, limits.totalMiB) ||
      !integer(form.excel_bps_image_storage_mib, 1, limits.storageMiB) ||
      !integer(form.excel_bps_image_storage_entries, 1, limits.storageEntries) ||
      !integer(form.excel_bps_image_ttl_minutes, 1, limits.ttlMinutes) ||
      form.excel_bps_image_max_total_mib < form.excel_bps_image_max_image_mib ||
      form.excel_bps_image_storage_mib < form.excel_bps_image_max_total_mib ||
      form.excel_bps_image_storage_entries < form.excel_bps_image_max_images ||
      (form.excel_bps_image_limit_policy === 'warn' && (form.excel_bps_image_compact_reserve >= form.excel_bps_image_warning_remaining || form.excel_bps_image_warning_remaining >= form.excel_bps_image_max_images))) return 'admin.settings.features.excelBpsImages.invalidLimits'
  return null
}

export function excelBPSImageSettingsPayload(form: ExcelBPSImageSettings): ExcelBPSImageSettings {
  const payload = { ...form, excel_bps_image_base_url: form.excel_bps_image_base_url.trim() }
  if (payload.excel_bps_image_base_url) payload.excel_bps_image_base_url = new URL(payload.excel_bps_image_base_url).origin
  return payload
}
