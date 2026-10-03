<template>
  <AppLayout>
    <div class="min-w-0 space-y-6" data-testid="channel-operations">
      <header>
        <h1 class="text-2xl font-semibold text-gray-900 dark:text-gray-100">{{ t('channelOperations.title') }}</h1>
        <p class="mt-2 text-sm text-gray-500 dark:text-gray-400">{{ t('channelOperations.description') }}</p>
      </header>
      <div class="space-y-3">
        <nav :aria-label="t('channelOperations.tabsLabel')" class="flex overflow-x-auto rounded-2xl border border-gray-200 bg-white p-1.5 dark:border-dark-700 dark:bg-dark-900" data-testid="channel-operations-tabs">
          <RouterLink v-for="tab in channelOperationsTabs" :key="tab.id" :to="tab.path"
            :aria-current="activeTab === tab.id ? 'page' : undefined"
            :data-tab="tab.id"
            class="flex shrink-0 items-center gap-2 rounded-xl px-4 py-2.5 text-sm transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500"
            :class="activeTab === tab.id ? 'bg-gray-900 font-semibold text-white dark:bg-gray-100 dark:text-gray-900' : 'text-gray-500 hover:bg-gray-100 dark:text-gray-400 dark:hover:bg-dark-800'">
            <Icon :name="tab.icon" size="sm" />{{ t(tab.label) }}
          </RouterLink>
        </nav>
        <nav v-if="sections.length" :aria-label="t('channelOperations.sectionsLabel')" class="flex flex-wrap gap-2" data-testid="channel-operations-sections">
          <RouterLink v-for="section in sections" :key="section.path" :to="section.path"
            :aria-current="route.path === section.path ? 'page' : undefined"
            class="rounded-lg px-3 py-2 text-sm transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500"
            :class="route.path === section.path ? 'bg-primary-50 font-medium text-primary-700 dark:bg-primary-950/40 dark:text-primary-300' : 'text-gray-500 hover:bg-gray-100 dark:text-gray-400 dark:hover:bg-dark-800'">
            {{ t(section.label) }}
          </RouterLink>
        </nav>
      </div>
      <!-- Only the active page is mounted. Leaving a tab stops its existing
           pollers and request controllers through onBeforeUnmount. -->
      <RouterView v-slot="{ Component }">
        <component :is="Component" :key="route.path" />
      </RouterView>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { RouterLink, RouterView, useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import Icon from '@/components/icons/Icon.vue'
import { channelOperationsSections, channelOperationsTabForPath, channelOperationsTabs } from '@/utils/channelOperations'

const { t } = useI18n()
const route = useRoute()
const activeTab = computed(() => channelOperationsTabForPath(route.path))
const sections = computed(() => activeTab.value === 'smart' || activeTab.value === 'credentials'
  ? channelOperationsSections[activeTab.value]
  : [])
</script>
