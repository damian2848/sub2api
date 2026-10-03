<template>
  <!-- .table-wrapper 是 TablePageLayout 滚动链的挂载点：外层 .table-scroll-container
       负责卡片外观并 overflow-hidden，本层接收 overflow-y-auto 才能在内容超高时滚动。 -->
  <div class="table-wrapper">
    <div v-if="loading" data-testid="channels-loading" class="py-10 text-center">
      <Icon name="refresh" size="lg" class="inline-block animate-spin text-gray-400" />
    </div>
    <div v-else-if="rows.length === 0" data-testid="channels-empty" class="py-12 text-center">
      <Icon name="inbox" size="xl" class="mx-auto mb-3 h-12 w-12 text-gray-400" />
      <p class="text-sm text-gray-500 dark:text-gray-400">{{ emptyLabel }}</p>
    </div>

    <!-- 一个分组一张卡：同平台分组彼此独立，不合并。 -->
    <div v-else data-testid="group-cards" class="grid grid-cols-1 gap-4 p-4 xl:grid-cols-2">
      <section
        v-for="row in rows"
        :key="row.group.id"
        data-testid="group-card"
        class="flex min-w-0 flex-col overflow-hidden rounded-xl border bg-white dark:bg-dark-800/60"
        :class="platformBorderClass(row.group.platform)"
      >
        <header
          class="flex min-w-0 flex-col gap-1.5 border-b px-4 py-3"
          :class="[platformBadgeLightClass(row.group.platform), platformBorderClass(row.group.platform)]"
        >
          <div class="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1.5">
            <span
              :class="[
                'inline-flex items-center gap-1 rounded-md border px-2 py-0.5 text-[11px] font-medium uppercase',
                platformBadgeClass(row.group.platform),
              ]"
            >
              <PlatformIcon :platform="row.group.platform as GroupPlatform" size="xs" />
              {{ row.group.platform }}
            </span>
            <GroupBadge
              class="max-w-full"
              :name="row.group.name"
              :platform="row.group.platform as GroupPlatform"
              :subscription-type="(row.group.subscription_type || 'standard') as SubscriptionType"
              :rate-multiplier="row.group.rate_multiplier"
              :user-rate-multiplier="userGroupRates[row.group.id] ?? null"
              always-show-rate
            />
            <span
              v-if="row.group.is_exclusive"
              class="inline-flex items-center gap-0.5 text-[10px] font-medium uppercase text-purple-600 dark:text-purple-400"
              :title="t('availableChannels.exclusiveTooltip')"
            >
              <Icon name="shield" size="xs" class="h-3 w-3" />
              {{ t('availableChannels.exclusive') }}
            </span>
            <span
              v-else
              class="inline-flex items-center gap-0.5 text-[10px] font-medium uppercase text-gray-500 dark:text-gray-400"
              :title="t('availableChannels.publicTooltip')"
            >
              <Icon name="globe" size="xs" class="h-3 w-3" />
              {{ t('availableChannels.public') }}
            </span>
            <span
              v-if="hasPeakRate(row.group)"
              class="inline-flex items-center gap-1 rounded-md bg-amber-50 px-1.5 py-0.5 text-[10px] font-medium text-amber-700 dark:bg-amber-900/20 dark:text-amber-300"
              :title="peakRateTitle(row.group)"
            >
              <Icon name="clock" size="xs" class="h-3 w-3" />
              {{ peakRateLabel(row.group) }}
            </span>
            <span class="ml-auto flex-shrink-0 text-xs text-gray-500 dark:text-gray-400">
              {{ t('availableChannels.modelCount', { count: row.models.length }) }}
            </span>
          </div>
          <p
            v-if="row.channels.length > 0"
            class="truncate text-xs text-gray-500 dark:text-gray-400"
            :title="channelNames(row)"
          >
            {{ t('availableChannels.viaChannels') }}：{{ channelNames(row) }}
          </p>
        </header>

        <div class="flex min-w-0 flex-1 flex-col gap-2 px-4 py-3">
          <div v-if="row.models.length > 0" class="flex min-w-0 flex-wrap gap-1">
            <SupportedModelChip
              v-for="m in visibleModels(row)"
              :key="`${row.group.id}-${m.platform}-${m.name}`"
              class="max-w-full [&>span]:max-w-full [&>span]:truncate"
              :model="m"
              :pricing-key-prefix="pricingKeyPrefix"
              :no-pricing-label="noPricingLabel"
              :show-platform="row.group.platform === 'composite'"
              :platform-hint="row.group.platform"
            />
          </div>
          <span v-else class="text-xs text-gray-400">{{ noModelsLabel }}</span>

          <button
            v-if="canToggle(row)"
            type="button"
            data-testid="toggle-models"
            class="inline-flex w-fit items-center gap-1 text-xs font-medium text-primary-600 hover:text-primary-700 dark:text-primary-400 dark:hover:text-primary-300"
            @click="toggle(row.group.id)"
          >
            <template v-if="isExpanded(row)">{{ t('availableChannels.collapse') }}</template>
            <template v-else>{{ t('availableChannels.expandAll', { count: hiddenCount(row) }) }}</template>
          </button>
        </div>
      </section>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import PlatformIcon from '@/components/common/PlatformIcon.vue'
import GroupBadge from '@/components/common/GroupBadge.vue'
import SupportedModelChip from './SupportedModelChip.vue'
import type { UserAvailableGroup, UserAvailableGroupView, UserSupportedModel } from '@/api/channels'
import type { GroupPlatform, SubscriptionType } from '@/types'
import { platformBadgeClass, platformBadgeLightClass, platformBorderClass } from '@/utils/platformColors'
import { useAppStore } from '@/stores/app'
import { hasPeakRate as groupHasPeakRate, formatPeakRateWindow, serverTimezoneLabel } from '@/utils/peak-rate'

/** 卡内默认展示的模型 chip 数量，超出部分折叠。 */
const COLLAPSED_MODEL_LIMIT = 24

const props = defineProps<{
  rows: UserAvailableGroupView[]
  loading: boolean
  pricingKeyPrefix: string
  noPricingLabel: string
  noModelsLabel: string
  emptyLabel: string
  /** 用户专属倍率（group_id → multiplier）；无专属时由 GroupBadge 仅显示默认倍率。 */
  userGroupRates: Record<number, number>
  /** 搜索词：非空时所有卡片自动展开（父级已把 models 过滤为命中项）。 */
  forceExpand?: boolean
}>()

const { t } = useI18n()
const appStore = useAppStore()

const expanded = ref<Set<number>>(new Set())

function isExpanded(row: UserAvailableGroupView): boolean {
  return !!props.forceExpand || expanded.value.has(row.group.id)
}

function toggle(groupId: number) {
  const next = new Set(expanded.value)
  if (next.has(groupId)) next.delete(groupId)
  else next.add(groupId)
  expanded.value = next
}

function visibleModels(row: UserAvailableGroupView): UserSupportedModel[] {
  return isExpanded(row) ? row.models : row.models.slice(0, COLLAPSED_MODEL_LIMIT)
}

function hiddenCount(row: UserAvailableGroupView): number {
  return props.forceExpand ? 0 : Math.max(0, row.models.length - COLLAPSED_MODEL_LIMIT)
}

/** 仅当模型数超过折叠上限时才提供展开/收起；搜索态强制展开，不再提供切换。 */
function canToggle(row: UserAvailableGroupView): boolean {
  return !props.forceExpand && row.models.length > COLLAPSED_MODEL_LIMIT
}

function channelNames(row: UserAvailableGroupView): string {
  return row.channels.map((c) => c.name).join(' · ')
}

function hasPeakRate(group: UserAvailableGroup): boolean {
  return groupHasPeakRate(group)
}

function peakRateLabel(group: UserAvailableGroup): string {
  return formatPeakRateWindow(group, serverTimezoneLabel(appStore.cachedPublicSettings?.server_utc_offset))
}

function peakRateTitle(group: UserAvailableGroup): string {
  return t('common.peakRateTooltip', { window: peakRateLabel(group) }) + t('common.peakRateImageNote')
}
</script>
