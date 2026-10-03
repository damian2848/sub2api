<template>
  <section class="space-y-4" data-testid="bps-image-panel">
    <p v-if="error" role="alert" class="rounded-xl bg-red-50 p-4 text-red-700 dark:bg-red-950/30 dark:text-red-300">{{ error }}</p>
    <p v-if="notice" role="status" class="text-emerald-600">{{ notice }}</p>
    <p v-if="loading" role="status">{{ t('common.loading') }}</p>
    <button v-if="!draft && !loading && isAdmin" type="button" class="btn btn-secondary" @click="load">{{ t('autoConfig.retry') }}</button>
    <form v-if="draft && isAdmin" class="space-y-4" @submit.prevent="save">
      <fieldset :disabled="saving" class="min-w-0"><BPSImageSettingsCard v-model="draft" /></fieldset>
      <div class="flex flex-wrap items-center gap-3"><button type="submit" data-testid="bps-image-save" class="btn btn-primary" :disabled="saving">{{ t(saving ? 'autoConfig.saving' : 'common.save') }}</button><p class="text-xs text-gray-500">{{ t('channelOperations.bpsImageSaveHint') }}</p></div>
    </form>
  </section>
</template>
<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAuthStore } from '@/stores/auth'
import { getSettings, updateSettings } from '@/api/admin/settings'
import BPSImageSettingsCard from './BPSImageSettingsCard.vue'
import { excelBPSImageLimits } from '@/utils/excelBPSImageLimits'
import { pickExcelBPSImageSettings, excelBPSImageSettingsError, excelBPSImageSettingsPayload, type ExcelBPSImageSettings } from '@/utils/excelBPSImageSettings'
const { t } = useI18n(), auth = useAuthStore()
const isAdmin = computed(() => auth.user?.role === 'admin')
const draft = ref<ExcelBPSImageSettings | null>(null)
const loading = ref(false), saving = ref(false), error = ref(''), notice = ref('')
let generation = 0, controller: AbortController | null = null
function stop() { generation++; controller?.abort(); controller = null; loading.value = false; saving.value = false }
async function load() {
  if (!isAdmin.value || loading.value || saving.value) return
  const id = ++generation; controller = new AbortController(); const signal = controller.signal
  loading.value = true; error.value = ''
  try { const value = await getSettings(signal); if (id === generation && isAdmin.value) draft.value = pickExcelBPSImageSettings(value) }
  catch { if (id === generation && !signal.aborted) error.value = t('channelOperations.bpsImageLoadFailed') }
  finally { if (id === generation) { loading.value = false; controller = null } }
}
async function save() {
  if (!draft.value || !isAdmin.value || saving.value || loading.value) return
  error.value = ''; notice.value = ''
  const invalid = excelBPSImageSettingsError(draft.value)
  if (invalid) { error.value = t(invalid, excelBPSImageLimits); return }
  const id = ++generation; controller = new AbortController(); const signal = controller.signal
  saving.value = true
  try { const value = await updateSettings(excelBPSImageSettingsPayload(draft.value), signal); if (id === generation && isAdmin.value) { draft.value = pickExcelBPSImageSettings(value); notice.value = t('channelOperations.bpsImageSaved') } }
  catch { if (id === generation && !signal.aborted) error.value = t('channelOperations.bpsImageSaveFailed') }
  finally { if (id === generation) { saving.value = false; controller = null } }
}
watch(() => [auth.user?.id, auth.user?.role], () => { stop(); draft.value = null; error.value = ''; notice.value = ''; if (isAdmin.value) void load() }, { immediate: true })
onBeforeUnmount(stop)
</script>
