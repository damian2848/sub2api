<template>
  <div class="space-y-5" data-testid="channel-prism">
    <header class="flex flex-wrap items-start justify-between gap-3">
      <div><h2 class="text-xl font-semibold">{{ t('prismConfig.title') }}</h2><p class="mt-2 text-sm text-gray-500">{{ t('prismConfig.description') }}</p></div>
      <RouterLink class="btn btn-secondary" to="/admin/accounts">{{ t('prismConfig.accounts') }}</RouterLink>
    </header>
    <p v-if="!isAdmin" role="alert">{{ t('prismConfig.forbidden') }}</p>
    <template v-else>
      <p v-if="error" role="alert" class="rounded-xl bg-red-50 p-4 text-red-700 dark:bg-red-950/30 dark:text-red-300">{{ error }}</p>
      <p v-if="notice" role="status" class="text-emerald-600">{{ notice }}</p>
      <div class="flex items-center gap-3"><button type="button" class="btn btn-secondary" data-testid="prism-refresh" :disabled="busy" @click="load">{{ t('common.refresh') }}</button><span v-if="operation === 'load'" role="status">{{ t('common.loading') }}</span></div>
      <section v-if="settings" class="card space-y-4 p-5" data-testid="prism-gateway">
        <h3 class="text-lg font-semibold">{{ t('prismConfig.gateway') }}</h3>
        <dl class="grid gap-3 text-sm sm:grid-cols-3">
          <div><dt class="text-gray-500">{{ t('prismConfig.baseUrl') }}</dt><dd class="mt-1 break-all">{{ settings.gateway.base_url || t('prismConfig.missing') }}</dd></div>
          <div><dt class="text-gray-500">{{ t('prismConfig.routing') }}</dt><dd class="mt-1">{{ t(settings.gateway.enabled ? 'prismConfig.enabled' : 'prismConfig.disabled') }}</dd></div>
          <div><dt class="text-gray-500">{{ t('prismConfig.managementKey') }}</dt><dd class="mt-1">{{ t(settings.gateway.management_key_configured ? 'prismConfig.configured' : 'prismConfig.missing') }}</dd></div>
        </dl>
        <p class="text-xs leading-5 text-gray-500">{{ t('prismConfig.connectionHint') }}</p>
        <p v-if="settings.availability !== 'ready'" role="alert" class="rounded-xl bg-amber-50 p-3 text-amber-800 dark:bg-amber-950/30 dark:text-amber-200">{{ t('prismConfig.availability.' + settings.availability) }}</p>
      </section>
      <form v-if="draft && settings?.configuration && settings.availability === 'ready'" class="card space-y-5 p-5" data-testid="prism-form" @submit.prevent="save">
        <div class="rounded-xl bg-blue-50 p-4 text-sm leading-6 text-blue-800 dark:bg-blue-950/30 dark:text-blue-200">{{ t('prismConfig.restartHint') }}</div>
        <p v-if="settings.configuration.restart_required" role="status" data-testid="prism-restart-required" class="rounded-xl bg-amber-50 p-3 text-amber-800 dark:bg-amber-950/30 dark:text-amber-200">{{ t('prismConfig.pending') }}</p>
        <p class="text-sm text-gray-500">{{ t('prismConfig.source') }}: {{ t('prismConfig.' + settings.configuration.source) }}</p>
        <h3 class="text-lg font-semibold">{{ t('prismConfig.desired') }}</h3>
        <fieldset :disabled="busy" class="min-w-0 space-y-4">
          <div v-for="key in prismBooleanFields" :key="key" class="rounded-xl border border-gray-200 p-4 dark:border-dark-700">
            <label class="flex items-center gap-3 text-sm font-medium"><input v-model="draft[key]" :data-testid="'prism-' + key" type="checkbox" role="switch" class="h-4 w-4 rounded text-primary-600" />{{ t('prismConfig.fields.' + key + '.label') }}</label>
            <p class="mt-2 text-xs leading-5 text-gray-500">{{ t('prismConfig.fields.' + key + '.hint') }}</p>
            <p class="mt-1 text-xs text-gray-500" :data-testid="'prism-effective-' + key">{{ t('prismConfig.current') }}: {{ t(settings.configuration.effective[key] ? 'prismConfig.enabled' : 'prismConfig.disabled') }}</p>
          </div>
          <div class="grid gap-4 sm:grid-cols-2">
            <div v-for="key in prismMemoryFields" :key="key">
              <label class="block"><span class="mb-2 block text-sm font-medium">{{ t('prismConfig.fields.' + key + '.label') }}</span><input v-model.number="draft[key]" :data-testid="'prism-' + key" type="number" min="0" :max="prismMemoryMaximum" step="1" required class="input w-full" /></label>
              <p class="mt-2 text-xs leading-5 text-gray-500">{{ t('prismConfig.fields.' + key + '.hint') }}</p>
              <p class="mt-1 text-xs text-gray-500" :data-testid="'prism-effective-' + key">{{ t('prismConfig.current') }}: {{ settings.configuration.effective[key] }} MiB</p>
            </div>
          </div>
          <div class="flex flex-wrap gap-3"><button type="submit" class="btn btn-primary" data-testid="prism-save" :disabled="busy || !dirty">{{ t(operation === 'save' ? 'prismConfig.saving' : 'prismConfig.save') }}</button><button type="button" class="btn btn-secondary" data-testid="prism-reset" :disabled="busy || settings.configuration.source !== 'saved'" @click="showReset = true">{{ t('prismConfig.reset') }}</button></div>
        </fieldset>
      </form>
      <ConfirmDialog :show="showReset" :title="t('prismConfig.resetTitle')" :message="t('prismConfig.resetMessage')" @cancel="showReset = false" @confirm="reset" />
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useAuthStore } from '@/stores/auth'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import { getPrismSettings, savePrismSettings, resetPrismSettings, validPrismStartupConfig, prismBooleanFields, prismMemoryFields, prismMemoryMaximum, type PrismSettings, type PrismStartupConfig } from '@/api/admin/prismConfig'

const { t } = useI18n()
const auth = useAuthStore()
const isAdmin = computed(() => auth.user?.role === 'admin')
const settings = ref<PrismSettings | null>(null)
const draft = ref<PrismStartupConfig | null>(null)
const operation = ref<'load' | 'save' | 'reset' | null>(null)
const busy = computed(() => operation.value !== null)
const error = ref(''), notice = ref(''), showReset = ref(false)
const dirty = computed(() => JSON.stringify(draft.value) !== JSON.stringify(settings.value?.configuration?.desired))
let generation = 0
let controller: AbortController | null = null

function stop() { generation++; controller?.abort(); controller = null; operation.value = null; showReset.value = false }
async function request(kind: 'load' | 'save' | 'reset') {
  if (!isAdmin.value || busy.value) return
  if (kind === 'save' && (!draft.value || !validPrismStartupConfig(draft.value))) { error.value = t('prismConfig.invalid'); return }
  if (kind !== 'load' && settings.value?.availability !== 'ready') return
  const id = ++generation
  controller = new AbortController()
  const signal = controller.signal
  operation.value = kind; error.value = ''; notice.value = ''
  try {
    const result = kind === 'load' ? await getPrismSettings(signal) : kind === 'save' ? await savePrismSettings({ ...draft.value! }, signal) : await resetPrismSettings(signal)
    if (generation !== id || !isAdmin.value) return
    settings.value = result
    draft.value = result.configuration ? { ...result.configuration.desired } : null
    if (kind !== 'load') notice.value = t(kind === 'save' ? 'prismConfig.savedNotice' : 'prismConfig.resetNotice')
  } catch {
    if (generation === id && !signal.aborted) {
      error.value = t('prismConfig.' + (kind === 'load' ? 'loadFailed' : kind === 'save' ? 'saveFailed' : 'resetFailed'))
      if (kind === 'load') { settings.value = null; draft.value = null }
    }
  } finally { if (generation === id) { operation.value = null; controller = null } }
}
function load() { void request('load') }
function save() { void request('save') }
function reset() { showReset.value = false; void request('reset') }
watch(() => [auth.user?.id, auth.user?.role], () => {
  stop(); settings.value = null; draft.value = null; error.value = ''; notice.value = ''
  if (isAdmin.value) load()
}, { immediate: true })
onBeforeUnmount(stop)
</script>
