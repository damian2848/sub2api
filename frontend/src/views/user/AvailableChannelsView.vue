<template>
  <AppLayout>
    <TablePageLayout>
      <template #filters>
        <div class="flex flex-col gap-3">
          <div class="flex flex-col justify-between gap-4 lg:flex-row lg:items-start">
            <div class="flex flex-1 flex-wrap items-center gap-3">
              <div class="relative w-full sm:w-80">
                <Icon
                  name="search"
                  size="md"
                  class="absolute left-3 top-1/2 -translate-y-1/2 text-gray-400 dark:text-gray-500"
                />
                <input
                  v-model="searchQuery"
                  type="text"
                  :placeholder="t('availableChannels.searchPlaceholder')"
                  class="input pl-10"
                />
              </div>
            </div>

            <div class="flex w-full flex-shrink-0 flex-wrap items-center justify-end gap-3 lg:w-auto">
              <button
                @click="loadChannels"
                :disabled="loading"
                class="btn btn-secondary"
                :title="t('common.refresh', 'Refresh')"
              >
                <Icon name="refresh" size="md" :class="loading ? 'animate-spin' : ''" />
              </button>
            </div>
          </div>

          <!-- 平台筛选：全部 + 实际出现的平台，计数随搜索词联动。 -->
          <div
            v-if="hasMultiplePlatforms"
            data-testid="platform-tabs"
            class="flex flex-wrap items-center gap-2"
          >
            <button
              v-for="tab in platformTabs"
              :key="tab.platform"
              type="button"
              data-testid="platform-tab"
              :class="[
                'inline-flex items-center gap-1.5 rounded-lg border px-3 py-1 text-xs font-medium transition-colors',
                activePlatform === tab.platform
                  ? 'border-primary-500 bg-primary-50 text-primary-700 dark:border-primary-400 dark:bg-primary-900/20 dark:text-primary-300'
                  : 'border-gray-200 bg-white text-gray-600 hover:bg-gray-50 dark:border-dark-600 dark:bg-dark-800 dark:text-gray-300 dark:hover:bg-dark-700',
              ]"
              @click="activePlatform = tab.platform"
            >
              <PlatformIcon v-if="tab.platform !== ALL" :platform="tab.platform as GroupPlatform" size="xs" />
              <span class="uppercase">{{ tab.platform === ALL ? t('availableChannels.allPlatforms') : tab.platform }}</span>
              <span class="text-gray-400 dark:text-gray-500">{{ tab.count }}</span>
            </button>
          </div>
        </div>
      </template>

      <template #table>
        <AvailableGroupCards
          :rows="visibleRows"
          :loading="loading"
          :user-group-rates="userGroupRates"
          :force-expand="searchQuery.trim() !== ''"
          pricing-key-prefix="availableChannels.pricing"
          :no-pricing-label="t('availableChannels.noPricing')"
          :no-models-label="t('availableChannels.noModels')"
          :empty-label="t('availableChannels.empty')"
        />
      </template>
    </TablePageLayout>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import Icon from '@/components/icons/Icon.vue'
import PlatformIcon from '@/components/common/PlatformIcon.vue'
import AvailableGroupCards from '@/components/channels/AvailableGroupCards.vue'
import userChannelsAPI, { type UserAvailableGroupView } from '@/api/channels'
import userGroupsAPI from '@/api/groups'
import type { GroupPlatform } from '@/types'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'

const ALL = 'all'

const { t } = useI18n()
const appStore = useAppStore()

const channels = ref<UserAvailableGroupView[]>([])
const userGroupRates = ref<Record<number, number>>({})
const loading = ref(false)
const searchQuery = ref('')
const activePlatform = ref<string>(ALL)

/**
 * 搜索过滤（先于平台 tab，让 tab 计数反映搜索结果）：
 * - 命中分组名 / 渠道名 / 渠道描述 / 平台 → 整张卡保留，模型完整展示；
 * - 否则只按模型名过滤，保留命中的模型 chip；一个都不命中则整张卡被过滤掉。
 */
const searchedRows = computed<UserAvailableGroupView[]>(() => {
  const q = searchQuery.value.trim().toLowerCase()
  if (!q) return channels.value
  const out: UserAvailableGroupView[] = []
  for (const row of channels.value) {
    const cardHit =
      row.group.name.toLowerCase().includes(q) ||
      row.group.platform.toLowerCase().includes(q) ||
      row.channels.some(
        (c) => c.name.toLowerCase().includes(q) || (c.description || '').toLowerCase().includes(q),
      )
    if (cardHit) {
      out.push(row)
      continue
    }
    const models = row.models.filter((m) => m.name.toLowerCase().includes(q))
    if (models.length > 0) out.push({ ...row, models })
  }
  return out
})

const platformTabs = computed(() => {
  const counts = new Map<string, number>()
  for (const row of searchedRows.value) {
    counts.set(row.group.platform, (counts.get(row.group.platform) ?? 0) + 1)
  }
  const tabs = [...counts.entries()]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([platform, count]) => ({ platform, count }))
  return [{ platform: ALL, count: searchedRows.value.length }, ...tabs]
})

// 只有一个平台时「全部」与该平台等价，不显示 tab 栏；按未过滤数据判断，避免搜索时 tab 栏忽隐忽现。
const hasMultiplePlatforms = computed(
  () => new Set(channels.value.map((row) => row.group.platform)).size > 1,
)

const visibleRows = computed(() =>
  activePlatform.value === ALL
    ? searchedRows.value
    : searchedRows.value.filter((row) => row.group.platform === activePlatform.value),
)

// 当前选中的平台在搜索/刷新后已不存在时回落到「全部」，避免出现空白页却没有可点的 tab。
watch(platformTabs, (tabs) => {
  if (!tabs.some((tab) => tab.platform === activePlatform.value)) activePlatform.value = ALL
})

async function loadChannels() {
  loading.value = true
  try {
    // 渠道列表和用户专属倍率并发拉取。专属倍率失败不阻塞渠道展示——
    // 失败时只是无法渲染专属倍率角标，降级为仅显示默认倍率。
    const [list, rates] = await Promise.all([
      userChannelsAPI.getAvailable(),
      userGroupsAPI.getUserGroupRates().catch((err: unknown) => {
        console.error('Failed to load user group rates:', err)
        return {} as Record<number, number>
      }),
    ])
    channels.value = list
    userGroupRates.value = rates
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  } finally {
    loading.value = false
  }
}

onMounted(loadChannels)
</script>
