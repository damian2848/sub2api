<template>
  <article
    ref="cardRef"
    class="group relative overflow-hidden rounded-lg border border-gray-200 bg-white shadow-sm transition-[border-color,box-shadow] duration-200 hover:border-gray-300 hover:shadow-md dark:border-dark-700 dark:bg-dark-800"
    data-testid="pelican-showcase-card"
    :data-result-id="item.id"
  >
    <div class="relative aspect-[4/3] overflow-hidden border-b border-gray-100 bg-gray-50 dark:border-dark-700/70 dark:bg-dark-900/40">
      <div :key="`${item.id}:${body?.status}`" class="pelican-result-surface absolute inset-0" :style="{ '--result-offset': slideOffset }">
        <PelicanArtworkPreview
          v-if="active && body?.status === 'ready'"
          :html="body.html"
          :interactive="false"
          :title="label"
        />
        <div v-else class="absolute inset-0 flex items-center justify-center p-4 text-center text-xs text-gray-400 dark:text-gray-500">
          <span v-if="!active" aria-hidden="true" />
          <span v-else-if="!body || body.status === 'loading'" class="animate-pulse">{{ t('pelicanShowcase.itemLoading') }}</span>
          <span v-else-if="body.status === 'invalid'">{{ t('pelicanShowcase.invalidHtml') }}</span>
          <span v-else class="text-red-500 dark:text-red-400">{{ t('pelicanShowcase.itemLoadError') }}</span>
        </div>
      </div>

      <div
        v-if="hasHistory"
        class="pointer-events-none absolute inset-x-2 top-1/2 z-20 flex -translate-y-1/2 justify-between"
        data-testid="pelican-showcase-history-controls"
      >
        <button
          type="button"
          class="pointer-events-auto grid h-9 w-9 place-items-center rounded-full border border-white/70 bg-white/90 text-gray-700 shadow-lg backdrop-blur transition hover:bg-white focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-500 disabled:cursor-not-allowed disabled:opacity-35 dark:border-dark-600/80 dark:bg-dark-800/90 dark:text-gray-100 dark:hover:bg-dark-700"
          :disabled="!canGoOlder"
          :aria-label="t('pelicanShowcase.previousResult')"
          :title="t('pelicanShowcase.previousResult')"
          data-testid="pelican-showcase-history-prev"
          @click.stop="emit('previous')"
        >
          <Icon name="chevronLeft" size="sm" :stroke-width="2" />
        </button>
        <button
          type="button"
          class="pointer-events-auto grid h-9 w-9 place-items-center rounded-full border border-white/70 bg-white/90 text-gray-700 shadow-lg backdrop-blur transition hover:bg-white focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-500 disabled:cursor-not-allowed disabled:opacity-35 dark:border-dark-600/80 dark:bg-dark-800/90 dark:text-gray-100 dark:hover:bg-dark-700"
          :disabled="!canGoNewer"
          :aria-label="t('pelicanShowcase.nextResult')"
          :title="t('pelicanShowcase.nextResult')"
          data-testid="pelican-showcase-history-next"
          @click.stop="emit('next')"
        >
          <Icon name="chevronRight" size="sm" :stroke-width="2" />
        </button>
      </div>
    </div>

    <div class="space-y-2 px-3 py-3">
        <p class="truncate font-mono text-sm font-medium text-gray-900 dark:text-gray-100" :title="item.model_id">
          {{ item.model_id || '—' }}
        </p>
      <div class="flex flex-wrap items-center justify-between gap-x-2 gap-y-1 text-xs text-gray-500 dark:text-gray-400">
        <time :datetime="item.generated_at">{{ formatDateTimeToMinute(item.generated_at) }}</time>
        <span v-if="hasHistory" aria-live="polite" class="shrink-0 tabular-nums text-gray-400 dark:text-gray-500">{{ historyPosition }}</span>
      </div>
      <div class="flex flex-wrap items-center justify-between gap-x-2 gap-y-1 text-xs">
        <span
          v-if="effortLabel"
          class="rounded-md bg-gray-100 px-1.5 py-0.5 font-medium text-gray-600 dark:bg-dark-700 dark:text-gray-300"
        >
          {{ effortLabel }}
        </span>
        <span class="tabular-nums text-gray-500 dark:text-gray-400">{{ durationLabel }}</span>
      </div>
    </div>

    <!-- The iframe is not interactive content of a button, so a full-card overlay opens the preview. -->
    <button
      type="button"
      class="absolute inset-0 z-10 rounded-lg focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary-500"
      :aria-label="`${label} · ${t('pelicanShowcase.preview')}`"
      data-testid="pelican-showcase-open"
      @click="emit('open')"
    />
  </article>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import type { PelicanShowcaseItem } from '@/api/pelicanShowcase'
import { formatDateTimeToMinute } from '@/utils/format'
import Icon from '@/components/icons/Icon.vue'
import PelicanArtworkPreview from './PelicanArtworkPreview.vue'
import { pelicanDurationLabel, pelicanEffortLabel, type PelicanBody } from './pelicanShowcaseFormat'

const props = withDefaults(defineProps<{
  item: PelicanShowcaseItem
  groupName: string
  body?: PelicanBody
  history?: PelicanShowcaseItem[]
  historyIndex?: number
  suspended?: boolean
}>(), {
  history: () => [],
  historyIndex: 0,
  suspended: false,
})

const emit = defineEmits<{
  (e: 'visibility', visible: boolean): void
  (e: 'open'): void
  (e: 'previous'): void
  (e: 'next'): void
}>()

// scrollMargin extends the lookahead to cards still hidden in their horizontal row, which
// clips them otherwise. Kept out of the call: TS 5.6's lib.dom does not declare the option yet.
const OBSERVER_OPTIONS = { rootMargin: '200px', scrollMargin: '200px' }
// A card must stay in view this long: cards a slider drag sweeps past would otherwise all
// load, queued ahead of the ones the drag stops at.
const VISIBLE_DWELL_MS = 150

const { t } = useI18n()
const cardRef = ref<HTMLElement | null>(null)
let observer: IntersectionObserver | null = null
let dwellTimer: ReturnType<typeof setTimeout> | undefined
const nearby = ref(false)
const pageVisible = ref(!document.hidden)
const slideOffset = ref('10px')
const active = computed(() => nearby.value && pageVisible.value && !props.suspended)

const label = computed(() => `${props.groupName} · ${props.item.model_id || '—'}`)
const effortLabel = computed(() => pelicanEffortLabel(t, props.item.reasoning_effort))
const durationLabel = computed(() => pelicanDurationLabel(t, props.item.latency_ms))
const hasHistory = computed(() => props.history.length > 1)
const canGoOlder = computed(() => props.historyIndex < props.history.length - 1)
const canGoNewer = computed(() => props.historyIndex > 0)
const historyPosition = computed(() => t('pelicanShowcase.historyPosition', { current: props.historyIndex + 1, total: props.history.length }))

watch(() => props.historyIndex, (index, previous) => {
  slideOffset.value = index > previous ? '-12px' : '12px'
})

watch([active, () => props.item.id], () => emit('visibility', active.value), { flush: 'post' })

function updatePageVisibility() {
  pageVisible.value = !document.hidden
}

// Keep observing so offscreen animations can be unmounted, while their HTML
// remains available in the parent's bounded cache for quick return visits.
onMounted(() => {
  document.addEventListener('visibilitychange', updatePageVisibility)
  if (typeof IntersectionObserver === 'undefined' || !cardRef.value) {
    nearby.value = true
    return
  }
  observer = new IntersectionObserver((entries) => {
    clearTimeout(dwellTimer)
    if (!entries[entries.length - 1].isIntersecting) {
      nearby.value = false
      return
    }
    dwellTimer = setTimeout(() => {
      nearby.value = true
    }, VISIBLE_DWELL_MS)
  }, OBSERVER_OPTIONS)
  observer.observe(cardRef.value)
})

onBeforeUnmount(() => {
  clearTimeout(dwellTimer)
  observer?.disconnect()
  document.removeEventListener('visibilitychange', updatePageVisibility)
  emit('visibility', false)
})
</script>

<style scoped>
.pelican-result-surface {
  animation: pelican-result-in 180ms ease both;
}

@keyframes pelican-result-in {
  from {
    opacity: 0;
    transform: translateX(var(--result-offset, 10px));
  }
  to {
    opacity: 1;
    transform: translateX(0);
  }
}

@media (prefers-reduced-motion: reduce) {
  .pelican-result-surface {
    animation-duration: 0ms;
  }
}
</style>
