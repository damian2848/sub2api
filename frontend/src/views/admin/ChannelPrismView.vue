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
        <div class="space-y-2 border-t border-gray-200 pt-4 dark:border-dark-700">
          <button type="button" class="btn btn-secondary" data-testid="prism-restart" :disabled="restartDisabled" @click="prepareRestart">{{ t('prismConfig.restart') }}</button>
          <p v-if="restartExplanation" class="text-sm text-gray-500" data-testid="prism-restart-explanation">{{ restartExplanation }}</p>
          <p v-if="restartPhase" role="status" data-testid="prism-restart-progress" class="text-sm text-blue-600">{{ t('prismConfig.' + restartPhase) }}</p>
        </div>
      </form>
      <ConfirmDialog :show="showReset" :title="t('prismConfig.resetTitle')" :message="t('prismConfig.resetMessage')" @cancel="showReset = false" @confirm="reset" />
      <ConfirmDialog :show="showRestart" :title="t('prismConfig.restartTitle')" :message="t('prismConfig.restartMessage')" :confirm-text="t('prismConfig.restart')" danger @cancel="cancelRestart" @confirm="restart" />
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useAuthStore } from '@/stores/auth'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import {
  getPrismSettings, savePrismSettings, resetPrismSettings, getPrismRestartStatus, restartPrismSettings,
  validPrismStartupConfig, prismBooleanFields, prismMemoryFields, prismMemoryMaximum,
  type PrismSettings, type PrismStartupConfig, type PrismRestartStatus, type PrismRestartRequest
} from '@/api/admin/prismConfig'

const { t } = useI18n()
const auth = useAuthStore()
const isAdmin = computed(() => auth.user?.role === 'admin')
const settings = ref<PrismSettings | null>(null)
const draft = ref<PrismStartupConfig | null>(null)
const restartStatus = ref<PrismRestartStatus | null>(null)
const operation = ref<'load' | 'save' | 'reset' | 'restart' | null>(null)
const restartPhase = ref<'restarting' | 'restartWaiting' | 'restartVerifying' | null>(null)
const busy = computed(() => operation.value !== null)
const error = ref(''), notice = ref(''), showReset = ref(false), showRestart = ref(false)
const dirty = computed(() => !sameConfig(draft.value, settings.value?.configuration?.desired))
const restartDisabled = computed(() => busy.value || dirty.value || settings.value?.availability !== 'ready' ||
  restartStatus.value?.availability !== 'ready' || restartStatus.value.runtime?.state !== 'ready')
const restartExplanation = computed(() => {
  if (dirty.value) return t('prismConfig.restartSaveFirst')
  if (restartStatus.value?.availability === 'unsupported') return t('prismConfig.restartUnsupported')
  if (restartStatus.value?.availability === 'not_configured') return t('prismConfig.restartNotConfigured')
  if (restartStatus.value?.availability !== 'ready') return t('prismConfig.restartUnavailable')
  if (restartStatus.value.runtime?.state === 'restarting' && operation.value !== 'restart') return t('prismConfig.restartAlreadyRunning')
  return ''
})
let restartSnapshot: PrismRestartRequest | null = null
let generation = 0
let controller: AbortController | null = null

function sameConfig(a: PrismStartupConfig | null | undefined, b: PrismStartupConfig | null | undefined) {
  return !!a && !!b && [...prismBooleanFields, ...prismMemoryFields].every(key => a[key] === b[key])
}
function cancelRestart() { showRestart.value = false; restartSnapshot = null }
function stop() {
  generation++; controller?.abort(); controller = null; operation.value = null
  restartPhase.value = null; showReset.value = false; cancelRestart()
}
async function probeRestart(signal: AbortSignal) {
  try { return await getPrismRestartStatus(signal) } catch { return null }
}
async function request(kind: 'load' | 'save' | 'reset') {
  if (!isAdmin.value || busy.value) return
  if (kind === 'save' && (!draft.value || !validPrismStartupConfig(draft.value))) { error.value = t('prismConfig.invalid'); return }
  if (kind !== 'load' && settings.value?.availability !== 'ready') return
  const id = ++generation
  controller = new AbortController()
  const signal = controller.signal
  operation.value = kind; error.value = ''; notice.value = ''
  // Capability failures must not turn a legacy sidecar's saved configuration into an empty form.
  const capability = probeRestart(signal)
  try {
    const result = kind === 'load' ? await getPrismSettings(signal) : kind === 'save' ? await savePrismSettings({ ...draft.value! }, signal) : await resetPrismSettings(signal)
    const status = await capability
    if (generation !== id || !isAdmin.value) return
    restartStatus.value = status
    if (kind === 'load' && result.availability === 'unavailable' && settings.value?.configuration) {
      error.value = t('prismConfig.loadFailed')
      return
    }
    settings.value = result
    draft.value = result.configuration ? { ...result.configuration.desired } : null
    if (kind !== 'load') notice.value = t(kind === 'save' ? 'prismConfig.savedNotice' : 'prismConfig.resetNotice')
  } catch {
    const status = await capability
    if (generation === id && !signal.aborted) {
      restartStatus.value = status
      error.value = t('prismConfig.' + (kind === 'load' ? 'loadFailed' : kind === 'save' ? 'saveFailed' : 'resetFailed'))
    }
  } finally { if (generation === id) { operation.value = null; controller = null } }
}
function load() { void request('load') }
function save() { void request('save') }
function reset() { showReset.value = false; void request('reset') }
function snapshotCurrent(snapshot: PrismRestartRequest) {
  return isAdmin.value && !dirty.value && settings.value?.availability === 'ready' &&
    sameConfig(settings.value.configuration?.desired, snapshot.expected_configuration) &&
    restartStatus.value?.availability === 'ready' && restartStatus.value.runtime?.state === 'ready' &&
    restartStatus.value.runtime.runtime_id === snapshot.expected_runtime_id
}
function prepareRestart() {
  if (restartDisabled.value || !settings.value?.configuration || !restartStatus.value?.runtime) return
  error.value = ''; notice.value = ''
  restartSnapshot = {
    expected_runtime_id: restartStatus.value.runtime.runtime_id,
    expected_configuration: { ...settings.value.configuration.desired }
  }
  showRestart.value = true
}
function responseStatus(value: unknown): number | undefined {
  if (!value || typeof value !== 'object') return undefined
  const failure = value as { status?: number; response?: { status?: number } }
  return failure.status ?? failure.response?.status
}
function refusalMessage(status: number) {
  return status === 409 ? 'restartConflict' : status === 401 || status === 403 ? 'restartDenied' : 'restartRejected'
}
// Abort the wait as well as the HTTP request, even if a transport has not settled yet.
function abortable<T>(promise: Promise<T>, signal: AbortSignal): Promise<T> {
  return new Promise((resolve, reject) => {
    const aborted = () => { cleanup(); reject(new Error('Aborted')) }
    const cleanup = () => signal.removeEventListener('abort', aborted)
    if (signal.aborted) { aborted(); return }
    signal.addEventListener('abort', aborted, { once: true })
    promise.then(value => { cleanup(); resolve(value) }, failure => { cleanup(); reject(failure) })
  })
}
function pause(signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const aborted = () => { clearTimeout(timer); signal.removeEventListener('abort', aborted); reject(new Error('Aborted')) }
    const timer = setTimeout(() => { signal.removeEventListener('abort', aborted); resolve() }, 1000)
    if (signal.aborted) { aborted(); return }
    signal.addEventListener('abort', aborted, { once: true })
  })
}
async function restart() {
  const snapshot = restartSnapshot
  if (!showRestart.value || !snapshot || busy.value) return
  if (!snapshotCurrent(snapshot)) {
    cancelRestart(); error.value = t('prismConfig.restartChanged'); return
  }
  cancelRestart()
  const id = ++generation
  const activeController = new AbortController()
  controller = activeController
  const signal = activeController.signal
  operation.value = 'restart'; restartPhase.value = 'restarting'; error.value = ''; notice.value = ''
  let timedOut = false
  const deadline = setTimeout(() => { timedOut = true; activeController.abort() }, 120000)
  try {
    try {
      const accepted = await abortable(restartPrismSettings(snapshot, signal), signal)
      if (generation !== id || !isAdmin.value) return
      restartStatus.value = accepted
    } catch (failure) {
      if (signal.aborted) throw failure
      const status = responseStatus(failure)
      if (status && status >= 400 && status < 500 && status !== 408 && status !== 429) {
        error.value = t('prismConfig.' + refusalMessage(status)); return
      }
      // Network / 503 acknowledgement loss is ambiguous. Only GETs may follow; never resubmit POST.
    }
    restartPhase.value = 'restartWaiting'
    while (!signal.aborted && generation === id && isAdmin.value) {
      await pause(signal)
      try {
        const status = await abortable(getPrismRestartStatus(signal), signal)
        if (generation !== id || !isAdmin.value) return
        restartStatus.value = status
        if (status.availability !== 'ready' || status.runtime?.state !== 'ready' || status.runtime.runtime_id === snapshot.expected_runtime_id) {
          restartPhase.value = 'restartWaiting'; continue
        }
        restartPhase.value = 'restartVerifying'
        const result = await abortable(getPrismSettings(signal), signal)
        if (generation !== id || !isAdmin.value) return
        if (result.availability !== 'ready' || !result.configuration) { restartPhase.value = 'restartWaiting'; continue }
        const config = result.configuration
        if (!sameConfig(config.effective, snapshot.expected_configuration) || !sameConfig(config.desired, snapshot.expected_configuration) || config.restart_required) {
          error.value = t('prismConfig.restartMismatch'); return
        }
        // Configuration and runtime are separate reads. Do not announce recovery if another
        // restart began while configuration was being verified.
        const settled = await abortable(getPrismRestartStatus(signal), signal)
        if (generation !== id || !isAdmin.value) return
        restartStatus.value = settled
        if (settled.availability !== 'ready' || settled.runtime?.state !== 'ready' || settled.runtime.runtime_id !== status.runtime.runtime_id) {
          restartPhase.value = 'restartWaiting'; continue
        }
        settings.value = result
        draft.value = { ...config.desired }
        notice.value = t('prismConfig.restartSucceeded')
        return
      } catch (failure) {
        if (signal.aborted) throw failure
        const status = responseStatus(failure)
        if (status === 401 || status === 403) { error.value = t('prismConfig.restartDenied'); return }
        restartPhase.value = 'restartWaiting'
      }
    }
  } catch {
    if (generation === id && isAdmin.value && (timedOut || !signal.aborted)) error.value = t('prismConfig.restartUnconfirmed')
  } finally {
    clearTimeout(deadline)
    if (generation === id) { operation.value = null; restartPhase.value = null; controller = null }
  }
}
watch(() => [dirty.value, settings.value?.configuration?.desired, restartStatus.value?.runtime?.runtime_id, restartStatus.value?.runtime?.state], () => {
  if (showRestart.value && restartSnapshot && !snapshotCurrent(restartSnapshot)) {
    cancelRestart(); error.value = t('prismConfig.restartChanged')
  }
}, { deep: true })
watch(() => [auth.user?.id, auth.user?.role], () => {
  stop(); settings.value = null; draft.value = null; restartStatus.value = null; error.value = ''; notice.value = ''
  if (isAdmin.value) load()
}, { immediate: true })
onBeforeUnmount(stop)
</script>
