<template>
  <section
    data-testid="grok-model-diagnostics"
    class="min-w-0 max-w-full overflow-hidden rounded-lg border border-gray-200 bg-gray-50 p-3 text-sm text-gray-700 dark:border-dark-600 dark:bg-dark-800 dark:text-gray-200"
  >
    <div class="flex min-w-0 flex-wrap items-start justify-between gap-2">
      <div class="min-w-0">
        <h3 class="font-medium text-gray-900 dark:text-gray-100">
          {{ t('admin.accounts.grokModelDiagnostics.title') }}
        </h3>
        <p data-testid="grok-diagnostics-state">{{ stateLabel }}</p>
      </div>
      <button
        v-if="canSync"
        type="button"
        data-testid="grok-diagnostics-sync"
        class="rounded-lg border border-emerald-200 px-3 py-1.5 text-sm text-emerald-700 hover:bg-emerald-50 disabled:cursor-not-allowed disabled:opacity-60 dark:border-emerald-800 dark:text-emerald-300 dark:hover:bg-emerald-900/30"
        :disabled="syncing || loading"
        @click="requestSync"
      >
        {{ syncing ? t('common.loading') : t('admin.accounts.grokModelDiagnostics.syncSavedConfig') }}
      </button>
    </div>

    <p v-if="loading" class="mt-2 text-xs text-gray-500 dark:text-gray-400">
      {{ t('common.loading') }}
    </p>
    <p
      v-if="dirty"
      data-testid="grok-diagnostics-dirty"
      class="mt-2 text-xs text-amber-700 dark:text-amber-300"
    >
      {{ t('admin.accounts.grokModelDiagnostics.savedConfigOnly') }}
    </p>
    <p
      v-if="refreshFailed"
      data-testid="grok-diagnostics-refresh-failed"
      class="mt-2 text-xs text-red-600 dark:text-red-400"
    >
      {{ t('admin.accounts.grokModelDiagnostics.refreshFailed') }}
    </p>

    <p data-testid="grok-diagnostics-endpoint" class="mt-2 min-w-0 break-all">
      {{ diagnostics?.endpoint || t('admin.accounts.grokModelDiagnostics.unknown') }}
    </p>
    <p data-testid="grok-diagnostics-last-success" class="mt-1">
      {{ t('admin.accounts.grokModelDiagnostics.lastSuccess') }}: {{ observedTime(diagnostics?.fetched_at) }}
    </p>
    <p data-testid="grok-diagnostics-last-attempt" class="mt-1 min-w-0 break-all">
      {{ t('admin.accounts.grokModelDiagnostics.lastAttempt') }}:
      <template v-if="diagnostics?.last_attempt">
        {{ diagnostics.last_attempt.status }}
        <span v-if="diagnostics.last_attempt.error_code">{{ diagnostics.last_attempt.error_code }}</span>
        {{ observedTime(diagnostics.last_attempt.finished_at) }}
      </template>
      <template v-else>{{ t('admin.accounts.grokModelDiagnostics.unknown') }}</template>
    </p>

    <details open class="mt-3 min-w-0">
      <summary class="cursor-pointer font-medium text-gray-800 dark:text-gray-100">
        {{ t('admin.accounts.grokModelDiagnostics.upstreamModels') }}
      </summary>
      <ul class="mt-1 space-y-1">
        <li
          v-for="model in diagnostics?.models ?? []"
          :key="model"
          data-testid="grok-diagnostics-model"
          class="min-w-0 break-all"
        >
          {{ model }}
        </li>
      </ul>
    </details>

    <details open class="mt-3 min-w-0">
      <summary class="cursor-pointer font-medium text-gray-800 dark:text-gray-100">
        {{ t('admin.accounts.grokModelDiagnostics.configuredModels') }}
      </summary>
      <ul class="mt-1 space-y-1">
        <li
          v-for="item in diagnostics?.mappings ?? []"
          :key="item.requested"
          class="min-w-0 break-all"
        >
          <span>{{ item.requested }}</span>
          <span v-if="item.upstream"> → {{ item.upstream }}</span>
          <span v-if="mappingStatus(item)"> {{ mappingStatus(item) }}</span>
        </li>
      </ul>
    </details>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import type { GrokModelDiagnosticItem, GrokModelDiagnostics } from '@/types'
import { formatDateTime } from '@/utils/format'

const props = withDefaults(defineProps<{
  diagnostics: GrokModelDiagnostics | null
  loading?: boolean
  dirty?: boolean
  refreshFailed?: boolean
  canSync?: boolean
  syncing?: boolean
}>(), {
  diagnostics: null,
  loading: false,
  dirty: false,
  refreshFailed: false,
  canSync: false,
  syncing: false
})

const emit = defineEmits<{
  sync: []
}>()

const { t } = useI18n()
const nowMs = ref(Date.now())
const RFC3339_RE = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/
let timer: ReturnType<typeof setInterval> | undefined

onMounted(() => {
  timer = setInterval(() => {
    nowMs.value = Date.now()
  }, 1000)
})

onBeforeUnmount(() => {
  if (timer !== undefined) clearInterval(timer)
})

function parseCatalogTime(value: string | undefined): number | null {
  if (!value) return null
  const trimmed = value.trim()
  if (!RFC3339_RE.test(trimmed)) return null
  const parsed = Date.parse(trimmed)
  return Number.isFinite(parsed) ? parsed : null
}

const fetchedMs = computed(() => parseCatalogTime(props.diagnostics?.fetched_at))
const expiresMs = computed(() => parseCatalogTime(props.diagnostics?.expires_at))
const fetchedUsable = computed(() => fetchedMs.value !== null && fetchedMs.value <= nowMs.value)

const effectiveState = computed(() => {
  const diagnostics = props.diagnostics
  if (!diagnostics) return 'unknown'
  if (diagnostics.state === 'fresh') {
    if (!fetchedUsable.value || expiresMs.value === null) return 'unknown'
    if (nowMs.value >= expiresMs.value) return 'stale'
    return 'fresh'
  }
  if (diagnostics.state === 'stale' || diagnostics.state === 'source_changed') {
    if (!fetchedUsable.value) return 'unknown'
    return diagnostics.state
  }
  return 'unknown'
})

const stateLabel = computed(() => {
  switch (effectiveState.value) {
    case 'fresh':
      return t('admin.accounts.grokModelDiagnostics.fresh')
    case 'stale':
      return t('admin.accounts.grokModelDiagnostics.stale')
    case 'source_changed':
      return t('admin.accounts.grokModelDiagnostics.sourceChanged')
    default:
      return t('admin.accounts.grokModelDiagnostics.unknown')
  }
})

function observedTime(value: string | undefined): string {
  const parsed = parseCatalogTime(value)
  if (parsed === null || parsed > nowMs.value) return t('admin.accounts.grokModelDiagnostics.unknown')
  return formatDateTime(value)
}

function mappingStatus(item: GrokModelDiagnosticItem): string {
  if (item.status === 'out_of_scope') return t('admin.accounts.grokModelDiagnostics.outOfScope')
  if (item.status === 'unverifiable' || (item.status === 'not_listed' && effectiveState.value !== 'fresh')) {
    return t('admin.accounts.grokModelDiagnostics.unverifiable')
  }
  if (item.status === 'not_listed') {
    return `${t('admin.accounts.grokModelDiagnostics.notListed')} ${t('admin.accounts.grokModelDiagnostics.compatibilityNotice')}`
  }
  return ''
}

function requestSync() {
  if (props.syncing || props.loading) return
  emit('sync')
}
</script>
