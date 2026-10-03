<template>
<div class="card" data-testid="excel-bps-image-settings">
  <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
    <h2 id="settings-section-features-excel-bps-images" tabindex="-1" class="text-lg font-semibold text-gray-900 dark:text-white">
      {{ t('admin.settings.features.excelBpsImages.title') }}
    </h2>
    <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
      {{ t('admin.settings.features.excelBpsImages.description') }}
    </p>
  </div>
  <div class="space-y-5 p-6">
    <div class="flex items-center justify-between gap-4">
      <div>
        <label for="excel-bps-image-enabled" class="text-sm font-medium text-gray-700 dark:text-gray-300">
          {{ t('admin.settings.features.excelBpsImages.enabled') }}
        </label>
        <p class="mt-0.5 text-xs text-gray-500 dark:text-gray-400">
          {{ t('admin.settings.features.excelBpsImages.enabledHint') }}
        </p>
      </div>
      <Toggle id="excel-bps-image-enabled" v-model="form.excel_bps_image_relay_enabled" />
    </div>
    <div v-if="form.excel_bps_image_relay_enabled">
      <label for="excel-bps-image-mode" class="input-label">{{ t('admin.settings.features.excelBpsImages.mode') }}</label>
      <select id="excel-bps-image-mode" v-model="form.excel_bps_image_mode" class="input">
        <option value="relay">{{ t('admin.settings.features.excelBpsImages.modeRelay') }}</option>
        <option value="native">{{ t('admin.settings.features.excelBpsImages.modeNative') }}</option>
      </select>
      <p v-if="form.excel_bps_image_mode === 'native'" class="mt-1.5 text-xs text-gray-500 dark:text-gray-400">
        {{ t('admin.settings.features.excelBpsImages.nativeHint') }}
      </p>
      <div v-if="form.excel_bps_image_mode === 'relay'" class="mt-5">
        <label for="excel-bps-image-base-url" class="input-label">
          {{ t('admin.settings.features.excelBpsImages.baseUrl') }}
        </label>
        <input
          id="excel-bps-image-base-url"
          v-model.trim="form.excel_bps_image_base_url"
          type="url"
          class="input"
          placeholder="https://your-api.example.com"
          required
        />
        <p class="mt-1.5 text-xs text-gray-500 dark:text-gray-400">
          {{ t('admin.settings.features.excelBpsImages.baseUrlHint') }}
        </p>
      </div>
      <div class="mt-5 space-y-3" data-testid="bps-image-policy">
        <label class="input-label" for="bps-image-limit-policy">{{ t('admin.settings.features.excelBpsImages.policyTitle') }}</label>
        <select id="bps-image-limit-policy" v-model="form.excel_bps_image_limit_policy" class="input">
          <option value="off">{{ t('admin.settings.features.excelBpsImages.policyOff') }}</option>
          <option value="auto_compact">{{ t('admin.settings.features.excelBpsImages.policyAuto') }}</option>
          <option value="warn">{{ t('admin.settings.features.excelBpsImages.policyWarn') }}</option>
        </select>
        <p class="text-xs text-gray-500">{{ t('admin.settings.features.excelBpsImages.policyHint') }}</p>
        <div v-if="form.excel_bps_image_limit_policy === 'warn'" class="grid gap-4 sm:grid-cols-2">
          <label class="space-y-1"><span class="input-label">{{ t('admin.settings.features.excelBpsImages.policyWarning') }}</span><input id="bps-image-warning-remaining" v-model.number="form.excel_bps_image_warning_remaining" class="input" type="number" min="1" :max="excelBPSImageLimits.images" required /></label>
          <label class="space-y-1"><span class="input-label">{{ t('admin.settings.features.excelBpsImages.policyReserve') }}</span><input id="bps-image-compact-reserve" v-model.number="form.excel_bps_image_compact_reserve" class="input" type="number" min="1" :max="excelBPSImageLimits.images" required /></label>
          <p class="text-xs text-gray-500 sm:col-span-2">{{ t('admin.settings.features.excelBpsImages.policyMarginsHint') }}</p>
        </div>
      </div>
      <h4 class="mt-6 input-label">{{ t('admin.settings.features.excelBpsImages.requestLimitsTitle') }}</h4>
      <div class="mt-5 grid gap-4 sm:grid-cols-4">
        <div class="space-y-1">
          <label for="excel-bps-image-body-limit" class="input-label">{{ t('admin.settings.features.excelBpsImages.bodyLimit') }}</label>
          <input id="excel-bps-image-body-limit" v-model.number="form.excel_bps_image_body_limit_mib" class="input" type="number" min="1" :max="excelBPSImageLimits.bodyMiB" step="1" required />
        </div>
        <div class="space-y-1">
          <label for="excel-bps-image-budget" class="input-label">{{ t('admin.settings.features.excelBpsImages.budget') }}</label>
          <input id="excel-bps-image-budget" v-model.number="form.excel_bps_image_budget_mib" class="input" type="number" :min="excelBPSImageLimits.minBudgetMiB" :max="excelBPSImageLimits.budgetMiB" step="1" required />
        </div>
        <div class="space-y-1">
          <label for="excel-bps-image-max-requests" class="input-label">{{ t('admin.settings.features.excelBpsImages.maxRequests') }}</label>
          <input id="excel-bps-image-max-requests" v-model.number="form.excel_bps_image_max_requests" class="input" type="number" min="1" :max="excelBPSImageLimits.requests" step="1" required />
        </div>
        <div class="space-y-1">
          <label for="excel-bps-image-max-images" class="input-label">{{ t('admin.settings.features.excelBpsImages.maxImages') }}</label>
          <input id="excel-bps-image-max-images" v-model.number="form.excel_bps_image_max_images" class="input" type="number" min="1" :max="excelBPSImageLimits.images" step="1" required />
          <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.settings.features.excelBpsImages.limitRange', { max: excelBPSImageLimits.images }) }}</p>
        </div>
      </div>
      <p class="mt-1.5 text-xs text-gray-500 dark:text-gray-400">
        {{ t('admin.settings.features.excelBpsImages.budgetHint', excelBPSImageLimits) }}
      </p>
      <p v-if="form.excel_bps_image_mode === 'native'" class="mt-1.5 text-xs text-gray-500 dark:text-gray-400">
        {{ t('admin.settings.features.excelBpsImages.nativeRetentionHint', { maxImages: form.excel_bps_image_max_images }) }}
      </p>
      <template v-if="form.excel_bps_image_mode === 'relay'">
        <h4 class="mt-6 input-label">{{ t('admin.settings.features.excelBpsImages.imageLimitsTitle') }}</h4>
        <div class="mt-3 grid gap-4 sm:grid-cols-3">
          <div class="space-y-1">
            <label for="excel-bps-image-max-image-mib" class="input-label">{{ t('admin.settings.features.excelBpsImages.maxImageMiB') }}</label>
            <input id="excel-bps-image-max-image-mib" v-model.number="form.excel_bps_image_max_image_mib" class="input" type="number" min="1" :max="excelBPSImageLimits.imageMiB" step="1" required />
            <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.settings.features.excelBpsImages.limitRange', { max: excelBPSImageLimits.imageMiB }) }}</p>
          </div>
          <div class="space-y-1">
            <label for="excel-bps-image-max-total-mib" class="input-label">{{ t('admin.settings.features.excelBpsImages.maxTotalMiB') }}</label>
            <input id="excel-bps-image-max-total-mib" v-model.number="form.excel_bps_image_max_total_mib" class="input" type="number" min="1" :max="excelBPSImageLimits.totalMiB" step="1" required />
            <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.settings.features.excelBpsImages.limitRange', { max: excelBPSImageLimits.totalMiB }) }}</p>
          </div>
          <div class="space-y-1">
            <label for="excel-bps-image-storage-mib" class="input-label">{{ t('admin.settings.features.excelBpsImages.storageMiB') }}</label>
            <input id="excel-bps-image-storage-mib" v-model.number="form.excel_bps_image_storage_mib" class="input" type="number" min="1" :max="excelBPSImageLimits.storageMiB" step="1" required />
            <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.settings.features.excelBpsImages.limitRange', { max: excelBPSImageLimits.storageMiB }) }}</p>
          </div>
          <div class="space-y-1">
            <label for="excel-bps-image-storage-entries" class="input-label">{{ t('admin.settings.features.excelBpsImages.storageEntries') }}</label>
            <input id="excel-bps-image-storage-entries" v-model.number="form.excel_bps_image_storage_entries" class="input" type="number" min="1" :max="excelBPSImageLimits.storageEntries" step="1" required />
            <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.settings.features.excelBpsImages.limitRange', { max: excelBPSImageLimits.storageEntries }) }}</p>
          </div>
          <div class="space-y-1">
            <label for="excel-bps-image-ttl-minutes" class="input-label">{{ t('admin.settings.features.excelBpsImages.ttlMinutes') }}</label>
            <input id="excel-bps-image-ttl-minutes" v-model.number="form.excel_bps_image_ttl_minutes" class="input" type="number" min="1" :max="excelBPSImageLimits.ttlMinutes" step="1" required />
            <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.settings.features.excelBpsImages.limitRange', { max: excelBPSImageLimits.ttlMinutes }) }}</p>
          </div>
        </div>
        <p class="mt-1.5 text-xs text-gray-500 dark:text-gray-400">
          {{ t('admin.settings.features.excelBpsImages.retentionHint') }}
        </p>
      </template>
    </div>
  </div>
</div>
</template>
<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import Toggle from '@/components/common/Toggle.vue'
import { excelBPSImageLimits } from '@/utils/excelBPSImageLimits'
import type { ExcelBPSImageSettings } from '@/utils/excelBPSImageSettings'
const form = defineModel<ExcelBPSImageSettings>({ required: true })
const { t } = useI18n()
</script>
<style scoped>
h2[id^="settings-section-"] { scroll-margin-top: 6rem; }
h2[id^="settings-section-"]:target {
  outline: 2px solid var(--color-primary-500, #6366f1);
  outline-offset: 6px;
  border-radius: 2px;
}
</style>
