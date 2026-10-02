<template>
  <section class="min-w-0 border-t border-gray-200 pt-5 dark:border-dark-700" data-testid="monitor-observations">
    <div class="mb-4 flex flex-wrap items-center justify-between gap-2">
      <h2 class="text-base font-semibold text-gray-900 dark:text-white">{{ t('channelMonitorV2.observations.title') }}</h2>
      <span class="text-xs text-gray-500">{{ data ? time(data.computed_at) : '' }}</span>
    </div>
    <div v-if="data" class="mb-5 grid gap-4 border-y border-gray-200 py-4 dark:border-dark-700 sm:grid-cols-3">
      <div>
        <p class="text-xs text-gray-500">{{ t('channelMonitorV2.observations.businessUsage') }}</p>
        <p class="mt-1 text-sm font-medium tabular-nums">{{ t('channelMonitorV2.observations.requestsTokens', { requests: exactBusiness ? count(data.business_usage.request_count) : '-', tokens: exactBusiness ? count(data.business_usage.token_count) : '-' }) }}</p>
      </div>
      <div>
        <p class="text-xs text-gray-500">{{ t('channelMonitorV2.observations.probeUsage') }}</p>
        <p class="mt-1 text-sm font-medium tabular-nums">{{ t('channelMonitorV2.observations.requestsTokens', { requests: showUsage ? count(data.summary.usage.request_count) : '-', tokens: showUsage ? count(probeTokens(data.summary.usage)) : '-' }) }}</p>
        <p class="mt-1 text-xs text-gray-500">{{ t('channelMonitorV2.observations.cost') }} {{ showUsage ? money(data.summary.usage.cost_usd) : '-' }} <span v-if="showUsage && (data.summary.usage.cost_incomplete || data.summary.usage.usage_incomplete)">· {{ t('channelMonitorV2.observations.partial') }}</span></p>
      </div>
      <div>
        <p class="text-xs text-gray-500">{{ t('channelMonitorV2.observations.totalTokens') }}</p>
        <p class="mt-1 text-sm font-medium tabular-nums">{{ exactBusiness && showUsage ? count(data.total_tokens) : '-' }}</p>
        <p class="mt-1 text-xs text-gray-500">{{ t('channelMonitorV2.observations.summary', { passed: count(data.summary.passed_count), failed: count(data.summary.failed_count), inconclusive: count(data.summary.inconclusive_count) }) }}</p>
      </div>
    </div>
    <p v-if="loading && !data" class="py-6 text-sm text-gray-500">{{ t('common.loading') }}</p>
    <p v-else-if="!data?.items.length" class="py-6 text-sm text-gray-500">{{ t('channelMonitorV2.observations.empty') }}</p>
    <div v-else class="overflow-x-auto">
      <table class="w-full min-w-[760px] text-left text-sm">
        <thead class="border-b border-gray-200 text-xs text-gray-500 dark:border-dark-700">
          <tr><th class="py-3 pr-4">{{ t('channelMonitorV2.observations.target') }}</th><th class="px-3 py-3">{{ t('channelMonitorV2.observations.kind') }}</th><th class="px-3 py-3">{{ t('channelMonitorV2.observations.status') }}</th><th class="px-3 py-3">{{ t('channelMonitorV2.observations.lastChecked') }}</th><th class="px-3 py-3">{{ t('channelMonitorV2.observations.usage') }}</th><th class="px-3 py-3">{{ t('channelMonitorV2.observations.history') }}</th></tr>
        </thead>
        <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
          <tr v-for="row in data.items" :key="row.id" data-testid="monitor-observation-row">
            <td class="py-3 pr-4"><p class="font-medium text-gray-900 dark:text-gray-100">{{ row.group_name || (row.name === row.type ? t(`channelMonitorV2.observations.kinds.${row.type}`) : row.name) }}</p><p class="mt-1 break-words text-xs text-gray-500">{{ row.platform }}<span v-if="row.model"> · {{ row.model }}</span></p></td>
            <td class="px-3 py-3 align-top"><span>{{ t(`channelMonitorV2.observations.kinds.${row.type}`) }}</span><p class="mt-1 text-xs text-gray-500">{{ !row.enabled ? t('channelMonitorV2.observations.paused') : row.schedule || (row.interval_seconds > 0 ? t('channelMonitorV2.observations.cadence', { seconds: row.interval_seconds }) : '') }}</p></td>
            <td class="px-3 py-3 align-top"><span class="inline-flex items-center gap-1.5 whitespace-nowrap"><i class="h-2 w-2 rounded-full" :class="statusColor(row.verdict)" />{{ verdict(row.type, row.verdict) }}</span><p v-if="row.latency_ms != null" class="mt-1 text-xs text-gray-500">{{ formatMonitorMs(row.latency_ms) }}</p></td>
            <td class="whitespace-nowrap px-3 py-3 align-top text-xs text-gray-500">{{ row.checked_at ? time(row.checked_at) : t('channelMonitorV2.candy.notChecked') }}</td>
            <td class="px-3 py-3 align-top text-xs tabular-nums"><p>{{ t('channelMonitorV2.observations.requestsTokens', { requests: showUsage ? count(row.usage.request_count) : '-', tokens: showUsage ? count(probeTokens(row.usage)) : '-' }) }}</p><p class="mt-1 text-gray-500">{{ showUsage ? money(row.usage.cost_usd) : '-' }}<span v-if="showUsage && (row.usage.cost_incomplete || row.usage.usage_incomplete)"> · {{ t('channelMonitorV2.observations.partial') }}</span></p></td>
            <td class="px-3 py-3 align-top">
              <details v-if="row.history.length" class="max-w-sm text-xs"><summary class="cursor-pointer text-gray-600 dark:text-gray-300">{{ t('channelMonitorV2.observations.samples', { count: row.sample_count }) }}</summary><ol class="mt-3 space-y-2"><li v-for="(point, index) in recentHistory(row)" :key="`${point.checked_at}-${index}`"><p>{{ time(point.checked_at) }} · {{ verdict(row.type, point.verdict) }}</p><p v-if="point.message && exactBusiness" class="mt-1 break-words text-gray-500">{{ point.message }}</p></li></ol></details>
              <span v-else class="text-xs text-gray-400">{{ t('channelMonitorV2.candy.notChecked') }}</span>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </section>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { MonitorObservationRow, MonitorObservations, ProbeUsage } from '@/api/channelMonitorV2'
import { formatMonitorMs } from './monitorFormat'

defineProps<{ data: MonitorObservations | null; loading: boolean; exactBusiness: boolean; showUsage: boolean }>()
const { t, te, locale } = useI18n()
const count = (value?: number) => new Intl.NumberFormat(locale.value).format(value || 0)
const money = (value?: number | null) => value == null ? '-' : new Intl.NumberFormat(locale.value, { style: 'currency', currency: 'USD', maximumFractionDigits: 4 }).format(value)
const time = (value: string) => new Date(value).toLocaleString(locale.value, { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false })
const probeTokens = (usage: ProbeUsage) => usage.input_tokens + usage.output_tokens + usage.cache_read_tokens + usage.cache_creation_tokens
const recentHistory = (row: MonitorObservationRow) => [...row.history].sort((a, b) => Date.parse(b.checked_at) - Date.parse(a.checked_at)).slice(0, 20)
function verdict(type: MonitorObservationRow['type'], value: string) {
  if (type === 'quota' && ['failed', 'error', 'degraded'].includes(value)) return t('channelMonitorV2.observations.quotaFailed')
  if (type === 'quota' && value === 'operational') return t('channelMonitorV2.observations.quotaAvailable')
  if (type === 'connectivity' && value === 'degraded') return t('channelMonitorV2.observations.connectedSlow')
  const key = `channelMonitorV2.observations.verdicts.${value}`
  if (value === 'incorrect' && type === 'candy') return t('channelMonitorV2.candy.states.incorrect')
  return te(key) ? t(key) : t('channelMonitorV2.observations.verdicts.unknown')
}
function statusColor(value: string) {
  if (['healthy', 'correct', 'operational', 'passed'].includes(value)) return 'bg-emerald-500'
  if (['incorrect', 'degraded', 'warning'].includes(value)) return 'bg-amber-500'
  if (['error', 'failed', 'critical'].includes(value)) return 'bg-red-500'
  return 'bg-gray-400'
}
</script>
