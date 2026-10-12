<template>
  <section class="card p-5" data-testid="harvest-policy-panel">
    <div class="mb-4 flex items-center justify-between gap-3">
      <h2 class="text-sm font-semibold">{{ localText('自动打票策略', 'Automatic harvest policy') }}</h2>
      <router-link to="/admin/proxies#harvest" class="text-sm text-primary-600">{{ localText('管理采票出口', 'Manage harvest exit') }}</router-link>
    </div>
    <p v-if="error" role="alert" class="mb-3 text-sm text-red-600">{{ error }}</p>
    <p v-if="loading" class="text-sm text-gray-500">{{ localText('加载中…', 'Loading…') }}</p>
    <button v-else-if="!form" type="button" class="btn btn-secondary" @click="load">{{ localText('重试', 'Retry') }}</button>
    <form v-else class="space-y-4" @submit.prevent="save">
      <fieldset class="space-y-4" :disabled="saving">
        <div class="flex items-center justify-between gap-4">
          <div class="min-w-0">
            <h3 class="text-base font-semibold text-gray-900 dark:text-white">
              {{ t("admin.settings.gatewayForwarding.codexTicketEnabled") }}
            </h3>
            <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
              {{ t("admin.settings.gatewayForwarding.codexTicketEnabledDesc") }}
            </p>
          </div>
          <Toggle
            id="codex-ticket-enabled"
            v-model="form.openai_codex_ticket_enabled"
          />
        </div>
        <div class="flex items-center justify-between gap-4 rounded-lg border border-gray-200 p-4 dark:border-dark-600">
          <div class="min-w-0">
            <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
              {{ t("admin.settings.gatewayForwarding.codexTicketFailClosed") }}
            </h3>
            <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
              {{ t("admin.settings.gatewayForwarding.codexTicketFailClosedDesc") }}
            </p>
          </div>
          <Toggle
            id="codex-ticket-fail-closed"
            v-model="form.openai_codex_ticket_fail_closed"
          />
        </div>
        <div class="rounded-lg border border-gray-200 p-4 dark:border-dark-600">
          <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
            {{ t("admin.settings.gatewayForwarding.codexTicketModels") }}
          </h3>
          <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
            {{ t("admin.settings.gatewayForwarding.codexTicketModelsDesc") }}
          </p>
          <fieldset class="mt-3 rounded-lg border border-gray-200 p-3 dark:border-dark-600">
            <legend class="px-1 text-sm font-semibold">{{ localText('自动打票范围', 'Automatic harvest scope') }}</legend>
            <select id="codex-ticket-harvest-scope" v-model="form.openai_codex_ticket_harvest_scope.mode" class="input">
              <option value="all">{{ localText('全部 OpenAI 账号', 'All OpenAI accounts') }}</option>
              <option value="selected">{{ localText('仅指定分组', 'Selected groups only') }}</option>
            </select>
            <label class="mt-3 block text-sm" for="codex-ticket-account-policy">{{ localText('账号采集策略', 'Account harvest policy') }}</label>
            <select id="codex-ticket-account-policy" v-model="form.openai_codex_ticket_harvest_scope.account_policy" class="input mt-2">
              <option value="schedulable_only">{{ localText('仅可调度账号（默认）', 'Schedulable accounts only (default)') }}</option>
              <option value="prioritize_schedulable">{{ localText('可调度优先，手动停调账号排队', 'Prioritize schedulable; queue manually disabled accounts') }}</option>
            </select>
            <div v-if="form.openai_codex_ticket_harvest_scope.mode === 'selected'" class="mt-3 space-y-2">
              <p v-if="codexHarvestGroupsLoadFailed" class="text-sm text-amber-600">{{ localText('分组加载失败，已选范围保留，请刷新后重试。', 'Could not load groups. Saved selection is preserved; refresh to retry.') }}</p>
              <label v-for="group in codexHarvestGroupChoices" :key="group.id" class="flex items-center gap-2 text-sm">
                <input :id="'codex-ticket-group-' + group.id" v-model="form.openai_codex_ticket_harvest_scope.group_ids" type="checkbox" :value="group.id" />
                <span>{{ group.name }} (#{{ group.id }})</span>
              </label>
              <p v-if="form.openai_codex_ticket_harvest_scope.group_ids.length === 0" class="text-sm text-amber-600">{{ localText('未选择分组：不会自动打票，不会退回全部账号。', 'No groups selected: automatic harvesting is paused, not broadened to all accounts.') }}</p>
            </div>
            <p class="mt-2 text-xs text-gray-500">{{ localText('控制后台采集。范围外账号不再自动补票；缺票拦截开启时，范围外遗留门票也不再参与门控选号。账号级「不打票」只停采集，不停调度。限流、过载、临时冷却、过期和配额耗尽账号始终跳过。兼容模式只会把手动关闭「参与调度」的账号放到后排。保存后下轮生效。', 'Controls background harvesting. Out-of-scope accounts are not probed, and fail-closed routing will not spend leftover tickets on them. Per-account skip-harvest only stops probing — the account stays schedulable. Rate-limited, overloaded, cooling-down, expired, and quota-exhausted accounts are always skipped. Compatibility mode only defers accounts whose scheduling switch was manually disabled. Changes apply next round.') }}</p>
          </fieldset>
          <label class="mt-3 block text-sm" for="codex-ticket-strategy">{{ localText('票据刷新策略', 'Ticket refresh strategy') }}</label>
          <select id="codex-ticket-strategy" v-model="form.openai_codex_ticket_strategy" class="input mt-2">
            <option value="standby">{{ localText('提前准备备用（默认）', 'Prepare standby (default)') }}</option>
            <option value="fixed">{{ localText('固定主用，失效后再采集', 'Keep primary until invalid') }}</option>
          </select>
          <label class="mt-3 flex items-center gap-2 text-sm"><input id="codex-ticket-strict" type="checkbox" v-model="form.openai_codex_ticket_strict_response" />{{ localText('严格拦截票据响应不匹配（默认关闭）', 'Reject mismatched ticket responses (off by default)') }}</label>
          <p class="mt-1 text-xs text-gray-500">{{ localText('仅在 HTTP 响应头明确不匹配时拦截正文；当前请求可能已计费，不会自动重放。关闭时只更新后续票据调度。', 'Rejects the body when HTTP response state headers mismatch. The upstream may have charged; the request is not replayed. When off, only future ticket scheduling changes.') }}</p>
          <p class="mt-1 text-xs text-gray-500">{{ localText('切换策略保留有效主备票据和冷却。固定主用可能在失效后短暂等待新票。', 'Switching preserves valid tickets and cooldowns. Fixed primary may briefly wait for a new ticket after expiry.') }}</p>
          <div class="mt-3 grid gap-3 sm:grid-cols-2">
            <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
              <input
                id="codex-ticket-model-astra"
                type="checkbox"
                :checked="form.openai_codex_ticket_models.includes('gpt-6-astra')"
                @change="toggleCodexTicketModel('gpt-6-astra', ($event.target as HTMLInputElement).checked)"
              />
              <span>Astra (gpt-6-astra)</span>
            </label>
            <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
              <input
                id="codex-ticket-model-sol"
                type="checkbox"
                :checked="form.openai_codex_ticket_models.includes('gpt-5.6-sol')"
                @change="toggleCodexTicketModel('gpt-5.6-sol', ($event.target as HTMLInputElement).checked)"
              />
              <span>Sol (gpt-5.6-sol)</span>
            </label>
          </div>
        </div>
      </fieldset>
      <button type="submit" data-testid="save-harvest-policy" class="btn btn-primary btn-sm" :disabled="saving">{{ localText('保存打票策略', 'Save harvest policy') }}</button>
    </form>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import Toggle from '@/components/common/Toggle.vue'
import { getSettings, updateSettings, type SystemSettings } from '@/api/admin/settings'
import { getAll } from '@/api/admin/groups'
import type { AdminGroup } from '@/types'
import { useAppStore } from '@/stores'

const emit = defineEmits<{ saved: [] }>()
const { t, locale } = useI18n()
const localText = (zh: string, en: string) => locale.value.startsWith('zh') ? zh : en
const appStore = useAppStore()
const saving = ref(false)
const loading = ref(true)
const error = ref('')
const codexHarvestGroups = ref<AdminGroup[]>([])
const codexHarvestGroupsLoadFailed = ref(false)
let disposed = false
function policyOf(settings: SystemSettings) {
  const scope = settings.openai_codex_ticket_harvest_scope
  return {
    openai_codex_ticket_enabled: settings.openai_codex_ticket_enabled ?? false,
    openai_codex_ticket_fail_closed: settings.openai_codex_ticket_fail_closed ?? false,
    openai_codex_ticket_models: [...(settings.openai_codex_ticket_models ?? ['gpt-6-astra', 'gpt-5.6-sol'])],
    openai_codex_ticket_strategy: settings.openai_codex_ticket_strategy || 'standby',
    openai_codex_ticket_strict_response: settings.openai_codex_ticket_strict_response ?? false,
    openai_codex_ticket_harvest_scope: {
      mode: scope?.mode || 'all', group_ids: [...(scope?.group_ids ?? [])],
      account_policy: scope?.account_policy || 'schedulable_only'
    }
  }
}
const form = ref<ReturnType<typeof policyOf> | null>(null)
const codexHarvestGroupChoices = computed(() => {
  const known = new Set(codexHarvestGroups.value.map(group => group.id))
  return [
    ...codexHarvestGroups.value.map(group => ({ id: group.id, name: group.name })),
    ...(form.value?.openai_codex_ticket_harvest_scope.group_ids ?? []).filter(id => !known.has(id)).map(id => ({
      id, name: localText('不可用或已删除的分组', 'Unavailable or deleted group') + ' #' + id
    }))
  ]
})
function toggleCodexTicketModel(model: string, checked: boolean) {
  if (!form.value) return
  const models = new Set(form.value.openai_codex_ticket_models)
  if (checked) models.add(model)
  else models.delete(model)
  form.value.openai_codex_ticket_models = [...models]
}
async function load() {
  loading.value = true
  error.value = ''
  const [settings, groups] = await Promise.allSettled([getSettings(), getAll()])
  if (disposed) return
  if (settings.status === 'fulfilled') form.value = policyOf(settings.value)
  else error.value = localText('无法加载打票策略，请重试。', 'Could not load harvest policy. Please retry.')
  codexHarvestGroupsLoadFailed.value = groups.status === 'rejected'
  codexHarvestGroups.value = groups.status === 'fulfilled' ? groups.value.filter(group => group.platform === 'openai') : []
  loading.value = false
}
async function save() {
  if (!form.value || saving.value || loading.value) return
  saving.value = true
  error.value = ''
  try {
    await updateSettings(structuredClone({ ...form.value, openai_codex_ticket_models: [...form.value.openai_codex_ticket_models], openai_codex_ticket_harvest_scope: { ...form.value.openai_codex_ticket_harvest_scope, group_ids: [...form.value.openai_codex_ticket_harvest_scope.group_ids] } }))
    if (disposed) return
    appStore.showSuccess(localText('打票策略已保存。', 'Harvest policy saved.'))
    emit('saved')
  } catch {
    if (!disposed) error.value = localText('打票策略保存失败，当前修改已保留。', 'Could not save harvest policy. Your changes are retained.')
  } finally { if (!disposed) saving.value = false }
}
onMounted(load)
onBeforeUnmount(() => { disposed = true })
</script>
