<template>
  <BaseDialog :show="show" :title="isCreating ? t('admin.accounts.prism.create') : t('admin.accounts.prism.connectionStatus')" width="narrow" @close="emit('close')">
    <div class="space-y-4">
      <template v-if="isCreating">
        <p class="break-words text-sm text-gray-600 dark:text-gray-400">{{ t('admin.accounts.prism.createConfirm', { name: account?.name }) }}</p>
        <div>
          <label for="prism-account-name" class="input-label">{{ t('admin.accounts.accountName') }}</label>
          <input id="prism-account-name" v-model="name" type="text" maxlength="100" class="input" :disabled="busy" />
        </div>
        <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
          <input v-model="inheritGroups" type="checkbox" class="h-4 w-4 rounded border-gray-300 text-primary-600" :disabled="busy" />
          {{ t('admin.accounts.prism.inheritGroups') }}
        </label>
      </template>
      <template v-else-if="status">
        <dl class="space-y-3 text-sm">
          <div class="flex items-start justify-between gap-4">
            <dt class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.prism.phase') }}</dt>
            <dd class="text-right font-medium" :class="status.ready ? 'text-emerald-600 dark:text-emerald-400' : 'text-gray-800 dark:text-gray-200'" data-testid="prism-phase">{{ phaseLabel }}</dd>
          </div>
          <div class="flex justify-between gap-4">
            <dt class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.prism.sourceAccount') }}</dt>
            <dd class="font-mono text-gray-800 dark:text-gray-200">#{{ status.source_account_id }}</dd>
          </div>
          <div class="flex justify-between gap-4">
            <dt class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.prism.account') }}</dt>
            <dd class="font-mono text-gray-800 dark:text-gray-200">#{{ status.account_id }}</dd>
          </div>
          <div class="space-y-2">
            <dt class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.prism.models') }}</dt>
            <dd class="flex flex-wrap gap-2" data-testid="prism-models">
              <span v-for="model in status.models" :key="model" class="break-all rounded bg-gray-100 px-2 py-1 font-mono text-xs text-gray-800 dark:bg-dark-700 dark:text-gray-200">{{ model }}</span>
              <span v-if="!status.models.length" class="text-gray-400">{{ t('admin.accounts.prism.modelsPending') }}</span>
            </dd>
          </div>
          <div v-if="status.last_heartbeat_at" class="space-y-1">
            <dt class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.prism.lastHeartbeat') }}</dt>
            <dd class="break-all text-gray-800 dark:text-gray-200">{{ heartbeatLabel }}</dd>
          </div>
        </dl>
      </template>
      <p v-if="busy && !status" class="flex items-center gap-2 text-sm text-gray-500" role="status">
        <Icon name="refresh" size="sm" class="animate-spin" />{{ t('common.loading') }}
      </p>
      <p v-if="status && !status.enabled" class="text-sm text-amber-700 dark:text-amber-300" role="alert">{{ t('admin.accounts.prism.notConfigured') }}</p>
      <p v-else-if="displayError" class="break-words text-sm text-red-600 dark:text-red-400" role="alert">{{ displayError }}</p>
    </div>
    <template #footer>
      <div class="flex flex-wrap justify-end gap-2">
        <button type="button" class="btn btn-secondary" @click="emit('close')">{{ t('common.close') }}</button>
        <button v-if="isCreating" type="button" class="btn btn-primary" :disabled="busy || !name.trim()" data-testid="prism-create" @click="create">
          <Icon :name="busy ? 'refresh' : 'sparkles'" size="sm" :class="{ 'animate-spin': busy }" />{{ t('admin.accounts.prism.create') }}
        </button>
        <template v-else>
          <button type="button" class="btn btn-secondary" :disabled="busy" data-testid="prism-refresh" @click="refreshStatus">
            <Icon name="refresh" size="sm" :class="{ 'animate-spin': busy }" />{{ t('common.refresh') }}
          </button>
          <button type="button" class="btn btn-primary" :disabled="!canReconnect" data-testid="prism-reconnect" @click="reconnect">
            <Icon name="link" size="sm" />{{ t('admin.accounts.prism.reconnect') }}
          </button>
        </template>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { PrismStatus } from '@/api/admin/accounts'
import type { Account } from '@/types'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'

const props = withDefaults(defineProps<{
  show: boolean
  account: Account | null
  mode?: 'create' | 'status' | 'reconnect'
}>(), { mode: 'status' })
const emit = defineEmits<{ (e: 'close'): void; (e: 'updated'): void }>()
const { t } = useI18n()
const name = ref('')
const inheritGroups = ref(true)
const busy = ref(false)
const status = ref<PrismStatus | null>(null)
const accountId = ref<number | null>(null)
const requestError = ref('')
let generation = 0
let controller: AbortController | null = null
let pollTimer: ReturnType<typeof setTimeout> | undefined

const isCreating = computed(() => props.mode === 'create' && accountId.value === null)
const transientPhases = new Set(['provisioning', 'session_ready', 'sandbox_syncing'])
const phases = new Set([...transientPhases, 'ready', 'error', 'disabled'])
const phaseLabel = computed(() => {
  const phase = status.value?.phase || 'provisioning'
  return t(`admin.accounts.prism.phases.${phases.has(phase) ? phase : 'provisioning'}`)
})
const heartbeatLabel = computed(() => {
  const value = status.value?.last_heartbeat_at || ''
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? '' : date.toLocaleString()
})
const canReconnect = computed(() => !busy.value && status.value?.enabled === true && !transientPhases.has(status.value.phase))
const displayError = computed(() => requestError.value || (status.value?.error_code ? errorLabel(status.value.error_code) : ''))

function errorLabel(code: string): string {
  const normalized = code.toLowerCase()
  if (normalized === 'prism_not_configured' || normalized === 'prism_disabled') return t('admin.accounts.prism.notConfigured')
  if (normalized.includes('source') || normalized.includes('oauth')) return t('admin.accounts.prism.sourceUnavailable')
  if (normalized.includes('sentinel') || normalized.includes('browser')) return t('admin.accounts.prism.browserFailed')
  return t('admin.accounts.prism.connectionFailed')
}

function stopRequests(): void {
  generation++
  if (pollTimer) clearTimeout(pollTimer)
  pollTimer = undefined
  controller?.abort()
  controller = null
  busy.value = false
}

function schedulePoll(): void {
  if (props.show && accountId.value && status.value?.enabled && status.value.phase !== 'error') {
    pollTimer = setTimeout(() => { void refreshStatus() }, 5000)
  }
}

async function request(operation: (signal: AbortSignal) => Promise<PrismStatus>, updated = false): Promise<void> {
  if (busy.value || !props.show) return
  if (pollTimer) clearTimeout(pollTimer)
  pollTimer = undefined
  const current = generation
  const activeController = new AbortController()
  controller = activeController
  busy.value = true
  requestError.value = ''
  try {
    const next = await operation(activeController.signal)
    if (current !== generation || !props.show) return
    const changed = status.value && (next.ready !== status.value.ready || next.phase !== status.value.phase)
    status.value = next
    if (updated || changed) emit('updated')
    schedulePoll()
  } catch (error: unknown) {
    if (current !== generation || activeController.signal.aborted) return
    const failure = error as { reason?: string; code?: string; response?: { data?: { reason?: string; code?: string } } }
    const code = failure.reason || failure.response?.data?.reason || failure.code || failure.response?.data?.code
    requestError.value = code === 'ECONNABORTED' || code === 'ETIMEDOUT'
      ? t('admin.accounts.prism.requestTimeout')
      : errorLabel(typeof code === 'string' ? code : '')
  } finally {
    if (current === generation) {
      busy.value = false
      controller = null
    }
  }
}

async function create(): Promise<void> {
  if (!props.account || !name.value.trim() || !isCreating.value) return
  const source = props.account
  const payload = { name: name.value.trim(), group_ids: inheritGroups.value ? source.group_ids ?? source.groups?.map(group => group.id) ?? [] : [] }
  await request(async (signal) => {
    const result = await adminAPI.accounts.createPrism(source.id, payload, signal)
    if (!signal.aborted) {
      accountId.value = result.account.id
    }
    return result.status
  }, true)
}

async function refreshStatus(): Promise<void> {
  if (!accountId.value) return
  const id = accountId.value
  await request(signal => adminAPI.accounts.getPrismStatus(id, signal))
}

async function reconnect(): Promise<void> {
  if (!accountId.value) return
  const id = accountId.value
  await request(signal => adminAPI.accounts.reconnectPrism(id, signal), true)
}

watch(() => [props.show, props.account?.id, props.mode] as const, () => {
  stopRequests()
  status.value = null
  requestError.value = ''
  accountId.value = props.mode === 'create' ? null : props.account?.id ?? null
  name.value = `${Array.from(props.account?.name || '').slice(0, 92).join('')} (Prism)`
  inheritGroups.value = true
  if (props.show && accountId.value) {
    if (props.mode === 'reconnect') void reconnect()
    else void refreshStatus()
  }
}, { immediate: true })

onUnmounted(stopRequests)
</script>
