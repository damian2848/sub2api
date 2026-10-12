<template>
  <div class="table-wrapper">
    <div v-if="loading" data-testid="channels-loading" class="py-12 text-center">
      <Icon name="refresh" size="lg" class="inline-block animate-spin text-gray-400" />
    </div>
    <div v-else-if="platforms.length === 0" data-testid="channels-empty" class="py-12 text-center">
      <Icon name="inbox" size="xl" class="mx-auto mb-3 h-12 w-12 text-gray-400" />
      <p class="text-sm text-gray-500 dark:text-gray-400">{{ emptyLabel }}</p>
    </div>
    <div v-else class="space-y-6 p-4 sm:p-6">
      <section
        v-for="section in platforms"
        :key="section.platform"
        data-testid="channel-platform"
        class="min-w-0"
      >
        <h2 class="mb-3 flex items-center gap-2">
          <span :class="['inline-flex items-center gap-1.5 rounded-md border px-2.5 py-1 text-xs font-semibold uppercase', platformBadgeClass(section.platform)]">
            <PlatformIcon :platform="section.platform as GroupPlatform" size="xs" />
            {{ section.platform }}
          </span>
        </h2>
        <div class="grid grid-cols-1 gap-3 xl:grid-cols-2">
          <article
            v-for="entry in section.groups"
            :key="entry.key"
            data-testid="channel-group"
            class="min-w-0 rounded-xl border border-gray-200 bg-gray-50/40 p-4 dark:border-dark-600 dark:bg-dark-900/30"
          >
            <header class="mb-3 flex min-w-0 flex-wrap items-center gap-2">
              <GroupBadge
                class="max-w-full"
                :name="entry.group.name"
                :platform="entry.group.platform as GroupPlatform"
                :subscription-type="(entry.group.subscription_type || 'standard') as SubscriptionType"
                :rate-multiplier="entry.group.rate_multiplier"
                :user-rate-multiplier="userGroupRates[entry.group.id] ?? null"
                always-show-rate
              />
              <span
                class="inline-flex items-center gap-1 text-[11px] text-gray-500 dark:text-gray-400"
                :title="t(entry.group.is_exclusive ? 'availableChannels.exclusiveTooltip' : 'availableChannels.publicTooltip')"
              >
                <Icon :name="entry.group.is_exclusive ? 'shield' : 'globe'" size="xs" />
                {{ t(entry.group.is_exclusive ? 'availableChannels.exclusive' : 'availableChannels.public') }}
              </span>
              <span
                v-if="groupHasPeakRate(entry.group)"
                class="inline-flex items-center gap-1 rounded-md bg-amber-50 px-1.5 py-0.5 text-[10px] font-medium text-amber-700 dark:bg-amber-900/20 dark:text-amber-300"
                :title="peakRateTitle(entry.group)"
              >
                <Icon name="clock" size="xs" />
                {{ peakRateLabel(entry.group) }}
              </span>
              <span class="break-words text-xs text-gray-400 sm:ml-auto">{{ entry.channelName }}</span>
            </header>
            <div class="mb-2 text-xs text-gray-500 dark:text-gray-400">{{ columns.supportedModels }} · {{ entry.models.length }}</div>
            <div class="flex min-w-0 flex-wrap gap-1.5">
              <SupportedModelChip
                v-for="model in entry.models"
                :key="model.name"
                class="max-w-full"
                :model="model"
                :pricing-key-prefix="pricingKeyPrefix"
                :no-pricing-label="noPricingLabel"
                :show-platform="false"
                :platform-hint="section.platform"
              />
            </div>
          </article>
        </div>
      </section>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import PlatformIcon from '@/components/common/PlatformIcon.vue'
import GroupBadge from '@/components/common/GroupBadge.vue'
import SupportedModelChip from './SupportedModelChip.vue'
import type { UserAvailableChannel, UserAvailableGroup } from '@/api/channels'
import type { GroupPlatform, SubscriptionType } from '@/types'
import { platformBadgeClass } from '@/utils/platformColors'
import { availableChannelPlatforms } from '@/utils/availableChannels'
import { useAppStore } from '@/stores/app'
import { hasPeakRate as groupHasPeakRate, formatPeakRateWindow, serverTimezoneLabel } from '@/utils/peak-rate'

const props = defineProps<{
  columns: { supportedModels: string }
  rows: UserAvailableChannel[]
  searchQuery?: string
  loading: boolean
  pricingKeyPrefix: string
  noPricingLabel: string
  emptyLabel: string
  userGroupRates: Record<number, number>
}>()
const { t } = useI18n()
const appStore = useAppStore()
const platforms = computed(() => availableChannelPlatforms(props.rows, props.searchQuery))

function peakRateLabel(group: UserAvailableGroup): string {
  return formatPeakRateWindow(group, serverTimezoneLabel(appStore.cachedPublicSettings?.server_utc_offset))
}
function peakRateTitle(group: UserAvailableGroup): string {
  return t('common.peakRateTooltip', { window: peakRateLabel(group) }) + t('common.peakRateImageNote')
}
</script>
