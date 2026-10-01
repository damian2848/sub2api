<template>
  <AppLayout>
    <div class="space-y-6 pb-6">
      <!-- Toolbar: group filter + the gallery rules + refresh -->
      <section class="flex flex-col gap-3 pt-3 md:flex-row md:items-center md:justify-between md:pt-4">
        <div
          v-if="groups.length > 1"
          role="tablist"
          :aria-label="t('pelicanShowcase.title')"
          class="flex flex-wrap gap-1 self-start rounded-xl border border-gray-200/60 bg-gray-100 p-0.5 text-xs dark:border-dark-700/60 dark:bg-dark-800"
        >
          <button
            v-for="tab in tabs"
            :key="tab.key"
            type="button"
            role="tab"
            :aria-selected="activeGroup === tab.key"
            class="rounded-lg px-3 py-1 transition-colors"
            :class="activeGroup === tab.key
              ? 'bg-white font-semibold text-gray-900 shadow-sm dark:bg-dark-700 dark:text-white'
              : 'text-gray-500 hover:text-gray-700 dark:text-gray-400 dark:hover:text-gray-200'"
            :data-testid="`showcase-tab-${tab.key}`"
            @click="activeGroup = tab.key"
          >
            {{ tab.label }}<span v-if="tab.count !== undefined" class="ml-1 tabular-nums opacity-60">{{ tab.count }}</span>
          </button>
        </div>
        <div v-else class="hidden md:block" />

        <div class="flex flex-wrap items-center gap-2 md:justify-end">
          <template v-if="view?.enabled && groups.length">
            <span class="inline-flex items-center rounded-full bg-gray-100 px-2.5 py-1 text-xs text-gray-600 dark:bg-dark-800 dark:text-gray-300" data-testid="showcase-keep-rule">
              {{ t('pelicanShowcase.keepRule', { count: view.max_items }) }}
            </span>
            <span
              v-if="view.retention_days > 0"
              class="inline-flex items-center rounded-full bg-gray-100 px-2.5 py-1 text-xs text-gray-600 dark:bg-dark-800 dark:text-gray-300"
              data-testid="showcase-retention-rule"
            >
              {{ t('pelicanShowcase.retentionRule', { days: view.retention_days }) }}
            </span>
          </template>
          <button
            type="button"
            class="flex h-8 w-8 items-center justify-center rounded-lg text-gray-500 transition-colors hover:bg-gray-100 hover:text-gray-700 disabled:opacity-50 dark:text-gray-400 dark:hover:bg-dark-700 dark:hover:text-gray-200"
            :disabled="loading"
            :title="t('common.refresh')"
            :aria-label="t('common.refresh')"
            @click="load"
          >
            <Icon name="refresh" size="md" :class="loading ? 'animate-spin' : ''" />
          </button>
        </div>
      </section>

      <!-- First load -->
      <div v-if="loading && !view" class="space-y-6">
        <div v-for="row in 2" :key="row" class="flex gap-5 overflow-hidden">
          <div
            v-for="i in 5"
            :key="i"
            class="overflow-hidden rounded-2xl border border-gray-200/80 bg-white/70 dark:border-dark-700/70 dark:bg-dark-800/60"
            :class="CARD_WIDTH"
          >
            <div class="aspect-[4/3] animate-pulse bg-gray-100 dark:bg-dark-900/40" />
            <div class="space-y-2 p-4">
              <div class="h-4 w-2/3 animate-pulse rounded bg-gray-200 dark:bg-dark-700" />
              <div class="h-3 w-1/3 animate-pulse rounded bg-gray-100 dark:bg-dark-700/60" />
            </div>
          </div>
        </div>
      </div>

      <EmptyState
        v-else-if="view && !view.enabled"
        :title="t('pelicanShowcase.disabled.title')"
        :description="t('pelicanShowcase.disabled.description')"
        data-testid="showcase-disabled"
      />
      <EmptyState
        v-else-if="view && !groups.length"
        :title="t('pelicanShowcase.empty.title')"
        :description="t('pelicanShowcase.empty.description')"
        data-testid="showcase-empty"
      />

      <!-- One section per showcased group -->
      <section
        v-for="groupView in shownModelGroups"
        :key="groupView.group.id"
        class="space-y-4"
        :data-testid="`showcase-group-${groupView.group.id}`"
      >
        <header class="flex min-w-0 items-center gap-3">
          <span
            class="grid h-9 w-9 flex-shrink-0 place-items-center rounded-xl ring-1 ring-black/5 dark:ring-white/10"
            :class="platformBadgeLightClass(groupView.group.platform)"
          >
            <PlatformIcon :platform="groupView.group.platform as GroupPlatform" size="sm" />
          </span>
          <div class="min-w-0">
            <h2 class="truncate text-base font-semibold text-gray-900 dark:text-white">{{ groupView.group.name }}</h2>
            <p class="text-xs text-gray-500 dark:text-gray-400">
              {{ platformLabel(groupView.group.platform) }}
              · {{ t('pelicanShowcase.modelCount', { count: groupView.models.length }) }}
              · {{ t('pelicanShowcase.itemCount', { count: groupView.group.items.length }) }}
              <template v-if="groupView.group.items.length">
                · {{ t('pelicanShowcase.latestAt', { time: formatRelativeTime(groupView.group.items[0].generated_at) }) }}
              </template>
            </p>
          </div>
        </header>

        <div
          v-if="!groupView.group.items.length"
          class="rounded-2xl border border-dashed border-gray-300 px-4 py-10 text-center text-sm text-gray-500 dark:border-dark-600 dark:text-gray-400"
        >
          {{ t('pelicanShowcase.groupEmpty') }}
        </div>
        <!-- One card per model keeps the group compact; each card switches its own history. -->
        <PelicanShowcaseRow v-else :label="t('pelicanShowcase.scrollLabel', { group: groupView.group.name })">
          <PelicanShowcaseCard
            v-for="model in groupView.models"
            :key="model.key"
            :class="CARD_WIDTH"
            :item="model.item"
            :history="model.items"
            :history-index="model.index"
            :group-name="groupView.group.name"
            :body="bodies[model.item.id]"
            :suspended="preview !== null"
            @visibility="setCardVisibility(model.key, model.item.id, $event)"
            @previous="changeHistory(model, 1)"
            @next="changeHistory(model, -1)"
            @open="openPreview(groupView.group, model.item)"
          />
        </PelicanShowcaseRow>
      </section>
    </div>

    <BaseDialog
      :show="preview !== null"
      :title="previewTitle"
      width="full"
      content-class="h-[90dvh]"
      body-class="flex min-h-0 flex-col !overflow-hidden"
      close-on-click-outside
      @close="closePreview"
    >
      <div v-if="preview" class="flex min-h-0 flex-1 flex-col gap-3" data-testid="showcase-preview">
        <!-- Chips instead of "·" separators, so a wrapped line never starts with a dot on phones. -->
        <div class="flex shrink-0 flex-wrap items-center gap-x-2 gap-y-2 text-xs text-gray-500 dark:text-gray-400">
          <span class="inline-flex items-center rounded-md px-1.5 py-0.5 font-medium" :class="platformBadgeLightClass(preview.group.platform)">
            {{ preview.group.name }}
          </span>
          <span>{{ formatDateTimeToMinute(preview.item.generated_at) }}</span>
          <span v-if="previewEffort" class="rounded-md bg-gray-100 px-1.5 py-0.5 text-gray-600 dark:bg-dark-700 dark:text-gray-300">
            {{ previewEffort }}
          </span>
          <span class="tabular-nums">{{ pelicanDurationLabel(t, preview.item.latency_ms) }}</span>
          <div role="group" :aria-label="t('pelicanShowcase.previewSizing')" class="ml-auto flex shrink-0 gap-1 rounded-lg bg-gray-100 p-1 dark:bg-dark-900/60">
            <button
              v-for="mode in (['fit', 'actual'] as const)"
              :key="mode"
              type="button"
              class="rounded-md px-2.5 py-1.5 font-medium transition-colors focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-500"
              :class="previewMode === mode ? 'bg-white text-gray-900 shadow-sm dark:bg-dark-700 dark:text-white' : 'text-gray-500 hover:text-gray-800 dark:text-gray-400 dark:hover:text-gray-100'"
              :aria-pressed="previewMode === mode"
              :data-testid="`showcase-preview-${mode}`"
              @click="previewMode = mode"
            >
              {{ t(mode === 'fit' ? 'pelicanShowcase.fitArtwork' : 'pelicanShowcase.actualSize') }}
            </button>
          </div>
        </div>
        <div class="min-h-0 flex-1 overflow-hidden rounded-xl border border-gray-200 bg-gray-100 dark:border-dark-700 dark:bg-dark-900" data-testid="showcase-preview-stage">
          <PelicanArtworkPreview
            v-if="pageVisible && previewBody?.status === 'ready'"
            :html="previewBody.html"
            :mode="previewMode"
            :title="previewTitle"
          />
          <div v-else class="flex h-full items-center justify-center p-6 text-sm text-gray-500">
            <span v-if="!previewBody || previewBody.status === 'loading'" class="animate-pulse">{{ t('pelicanShowcase.itemLoading') }}</span>
            <span v-else-if="previewBody.status === 'invalid'">{{ t('pelicanShowcase.invalidHtml') }}</span>
            <span v-else class="text-red-500">{{ t('pelicanShowcase.itemLoadError') }}</span>
          </div>
        </div>
        <p class="shrink-0 text-xs text-gray-400 dark:text-gray-500">{{ t('pelicanShowcase.sandboxNote') }}</p>
      </div>
      <template #footer>
        <div class="flex w-full items-center justify-between gap-3">
          <button
            v-if="isAdmin"
            type="button"
            class="btn btn-danger"
            :disabled="removing"
            data-testid="showcase-remove"
            @click="confirmingRemove = true"
          >
            {{ t('pelicanShowcase.remove') }}
          </button>
          <span v-else />
          <button type="button" class="btn btn-secondary" @click="closePreview">{{ t('common.close') }}</button>
        </div>
      </template>
    </BaseDialog>

    <ConfirmDialog
      :show="confirmingRemove"
      :title="t('pelicanShowcase.remove')"
      :message="t('pelicanShowcase.removeConfirm')"
      :confirm-text="t('pelicanShowcase.remove')"
      danger
      @confirm="removeItem"
      @cancel="confirmingRemove = false"
    />
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import PlatformIcon from '@/components/common/PlatformIcon.vue'
import Icon from '@/components/icons/Icon.vue'
import PelicanArtworkPreview from '@/components/user/pelican/PelicanArtworkPreview.vue'
import PelicanShowcaseCard from '@/components/user/pelican/PelicanShowcaseCard.vue'
import PelicanShowcaseRow from '@/components/user/pelican/PelicanShowcaseRow.vue'
import {
  pelicanDurationLabel,
  pelicanEffortLabel,
  type PelicanBody,
} from '@/components/user/pelican/pelicanShowcaseFormat'
import { groupPelicanItemsByModel, type PelicanModelHistory } from '@/components/user/pelican/pelicanShowcaseHistory'
import {
  getShowcase,
  getShowcaseItem,
  removeShowcaseItem,
  type PelicanShowcaseGroup,
  type PelicanShowcaseItem,
  type PelicanShowcaseView,
} from '@/api/pelicanShowcase'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import type { GroupPlatform } from '@/types'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatDateTimeToMinute, formatRelativeTime } from '@/utils/format'
import { extractPelicanHtml } from '@/utils/pelicanHtml'
import { platformBadgeLightClass, platformLabel } from '@/utils/platformColors'

const MAX_CONCURRENT_BODIES = 4
const MAX_CACHED_BODIES = 24
const MAX_CACHE_CHARACTERS = 4_000_000
// Use 70% of the former width at every breakpoint; artwork keeps its 4:3 ratio.
const CARD_WIDTH = 'w-[59.5%] shrink-0 sm:w-[calc((100%-1.25rem)/2*0.7)] lg:w-[calc((100%-2.5rem)/3*0.7)] 2xl:w-[calc((100%-3.75rem)/4*0.7)]'

type TabKey = number | 'all'

const { t } = useI18n()
const appStore = useAppStore()
const authStore = useAuthStore()
const isAdmin = computed(() => authStore.isAdmin)

const view = ref<PelicanShowcaseView | null>(null)
const loading = ref(false)
const activeGroup = ref<TabKey>('all')
const bodies = reactive<Record<number, PelicanBody>>({})
const historyIndexes = reactive<Record<string, number>>({})
const preview = ref<{ group: PelicanShowcaseGroup; item: PelicanShowcaseItem } | null>(null)
const previewMode = ref<'fit' | 'actual'>('fit')
const confirmingRemove = ref(false)
const removing = ref(false)
const pageVisible = ref(!document.hidden)

let alive = true
let loadController: AbortController | null = null
const bodyQueue: number[] = []
const bodyControllers = new Map<number, AbortController>()
const visibleCards = new Map<string, number>()
const bodyCache = new Map<number, number>()
let currentItemIds = new Set<number>()

const groups = computed(() => (view.value?.enabled ? view.value.groups : []))
const tabs = computed(() => [
  { key: 'all' as TabKey, label: t('pelicanShowcase.allGroups'), count: undefined as number | undefined },
  ...groups.value.map((group) => ({ key: group.id as TabKey, label: group.name, count: group.items.length })),
])
const modelHistoryGroups = computed(() => groups.value.map((group) => ({
  group,
  models: groupPelicanItemsByModel(group),
})))
const shownModelHistoryGroups = computed(() => activeGroup.value === 'all'
  ? modelHistoryGroups.value
  : modelHistoryGroups.value.filter(({ group }) => group.id === activeGroup.value))
const shownModelGroups = computed(() => shownModelHistoryGroups.value.map(({ group, models }) => ({
  group,
  models: models.map((history) => {
    const index = Math.min(historyIndexes[history.key] ?? 0, Math.max(0, history.items.length - 1))
    return { ...history, index, item: history.items[index] }
  }),
})))
const previewBody = computed(() => (preview.value ? bodies[preview.value.item.id] : undefined))
const previewTitle = computed(() =>
  preview.value
    ? t('pelicanShowcase.previewTitle', { group: preview.value.group.name, model: preview.value.item.model_id || '—' })
    : ''
)
const previewEffort = computed(() => (preview.value ? pelicanEffortLabel(t, preview.value.item.reasoning_effort) : ''))

// A removed group must not leave the filter pointing at nothing.
watch(groups, (list) => {
  if (activeGroup.value !== 'all' && !list.some((group) => group.id === activeGroup.value)) activeGroup.value = 'all'
})

watch(modelHistoryGroups, (list) => {
  const keys = new Set(list.flatMap((group) => group.models.map((model) => model.key)))
  for (const key of Object.keys(historyIndexes)) {
    if (!keys.has(key)) delete historyIndexes[key]
  }
}, { flush: 'post' })

function requestBody(id: number) {
  if (!alive || !pageVisible.value || !currentItemIds.has(id)) return
  if (bodies[id]) {
    if (bodyCache.has(id)) {
      const size = bodyCache.get(id)!
      bodyCache.delete(id)
      bodyCache.set(id, size)
    }
    return
  }
  // A cancelled request may still be settling. Its finalizer will restart it
  // if this result becomes visible again before cancellation completes.
  if (bodyControllers.has(id)) return
  bodies[id] = { status: 'loading', html: '' }
  bodyQueue.unshift(id)
  pumpBodies()
}

function pumpBodies() {
  while (alive && bodyControllers.size < MAX_CONCURRENT_BODIES && bodyQueue.length) {
    const id = bodyQueue.shift()!
    const controller = new AbortController()
    bodyControllers.set(id, controller)
    getShowcaseItem(id, { signal: controller.signal })
      .then((item) => {
        if (!alive || controller.signal.aborted || !currentItemIds.has(id)) return
        const html = extractPelicanHtml(item.response_text || '')
        bodies[id] = html ? { status: 'ready', html } : { status: 'invalid', html: '' }
        bodyCache.set(id, html.length)
        pruneBodyCache()
      })
      .catch(() => {
        if (alive && !controller.signal.aborted && currentItemIds.has(id)) {
          bodies[id] = { status: 'error', html: '' }
          bodyCache.set(id, 0)
          pruneBodyCache()
        }
      })
      .finally(() => {
        bodyControllers.delete(id)
        if (alive && controller.signal.aborted && wantedBodyIds().has(id)) requestBody(id)
        pumpBodies()
      })
  }
}

function wantedBodyIds() {
  const ids = new Set(pageVisible.value ? visibleCards.values() : [])
  if (pageVisible.value && preview.value) ids.add(preview.value.item.id)
  return ids
}

function pruneBodyWork() {
  const wanted = wantedBodyIds()
  for (let i = bodyQueue.length - 1; i >= 0; i--) {
    const id = bodyQueue[i]
    if (wanted.has(id) && currentItemIds.has(id)) continue
    bodyQueue.splice(i, 1)
    delete bodies[id]
  }
  for (const [id, controller] of bodyControllers) {
    if (wanted.has(id) && currentItemIds.has(id)) continue
    controller.abort()
    if (bodies[id]?.status === 'loading') delete bodies[id]
  }
}

function pruneBodyCache() {
  const wanted = wantedBodyIds()
  let characters = Array.from(bodyCache.values()).reduce((sum, size) => sum + size, 0)
  for (const [id, size] of bodyCache) {
    if (bodyCache.size <= MAX_CACHED_BODIES && characters <= MAX_CACHE_CHARACTERS) break
    if (wanted.has(id)) continue
    delete bodies[id]
    bodyCache.delete(id)
    characters -= size
  }
}

function setCardVisibility(key: string, id: number, visible: boolean) {
  if (visible) visibleCards.set(key, id)
  else visibleCards.delete(key)
  pruneBodyWork()
  if (visible) requestBody(id)
  pruneBodyCache()
}

function changeHistory(model: PelicanModelHistory & { index: number }, delta: number) {
  const current = model.index
  const next = Math.min(model.items.length - 1, Math.max(0, current + delta))
  if (next === current) return
  historyIndexes[model.key] = next
}

async function load() {
  loadController?.abort()
  const controller = new AbortController()
  loadController = controller
  loading.value = true
  try {
    const next = await getShowcase({ signal: controller.signal })
    if (!alive || controller.signal.aborted) return
    const kept = new Set(next.groups.flatMap((group) => group.items.map((item) => item.id)))
    currentItemIds = kept
    const retry: number[] = []
    for (const key of Object.keys(bodies)) {
      const id = Number(key)
      if (kept.has(id) && bodies[id].status === 'error') retry.push(id)
      if (!kept.has(id) || bodies[id].status === 'error') {
        delete bodies[id]
        bodyCache.delete(id)
      }
    }
    for (const key of Object.keys(historyIndexes)) delete historyIndexes[key]
    view.value = next
    if (preview.value) {
      const group = next.groups.find((group) => group.id === preview.value?.group.id)
      const item = group?.items.find((item) => item.id === preview.value?.item.id)
      if (next.enabled && group && item) preview.value = { group, item }
      else closePreview()
    }
    pruneBodyWork()
    const wanted = wantedBodyIds()
    retry.filter((id) => wanted.has(id)).forEach(requestBody)
  } catch (err: unknown) {
    const e = err as { name?: string; code?: string }
    if (e?.name === 'AbortError' || e?.code === 'ERR_CANCELED') return
    appStore.showError(extractApiErrorMessage(err, t('pelicanShowcase.loadError')))
  } finally {
    if (loadController === controller) {
      loading.value = false
      loadController = null
    }
  }
}

function openPreview(group: PelicanShowcaseGroup, item: PelicanShowcaseItem) {
  previewMode.value = 'fit'
  preview.value = { group, item }
  requestBody(item.id)
}

function closePreview() {
  preview.value = null
  confirmingRemove.value = false
}

async function removeItem() {
  const target = preview.value
  confirmingRemove.value = false
  if (!target || removing.value) return
  removing.value = true
  try {
    await removeShowcaseItem(target.item.id)
    // A refresh started before the deletion may still contain this result.
    loadController?.abort()
    const group = view.value?.groups.find((group) => group.id === target.group.id)
    if (group) group.items = group.items.filter((item) => item.id !== target.item.id)
    currentItemIds.delete(target.item.id)
    delete bodies[target.item.id]
    bodyCache.delete(target.item.id)
    pruneBodyWork()
    closePreview()
    appStore.showSuccess(t('pelicanShowcase.removed'))
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('pelicanShowcase.removeFailed')))
  } finally {
    removing.value = false
  }
}

function updatePageVisibility() {
  pageVisible.value = !document.hidden
  pruneBodyWork()
  if (pageVisible.value && preview.value) requestBody(preview.value.item.id)
}

onMounted(() => {
  document.addEventListener('visibilitychange', updatePageVisibility)
  load()
})
onBeforeUnmount(() => {
  alive = false
  loadController?.abort()
  bodyControllers.forEach((controller) => controller.abort())
  bodyQueue.length = 0
  document.removeEventListener('visibilitychange', updatePageVisibility)
  currentItemIds = new Set()
})
</script>
