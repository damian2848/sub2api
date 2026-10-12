<template>
  <div v-if="groups?.length" class="flex w-max min-w-40 max-w-72 flex-col items-start gap-1">
    <GroupBadge
      v-for="group in groups"
      :key="group.id"
      :name="group.name"
      :platform="group.platform"
      :subscription-type="group.subscription_type"
      :rate-multiplier="group.rate_multiplier"
      :show-rate="false"
      :truncate-name="false"
      class="max-w-full"
    />
  </div>
  <span v-else class="text-sm text-gray-400 dark:text-dark-500">-</span>
</template>

<script setup lang="ts">
import GroupBadge from '@/components/common/GroupBadge.vue'
import type { Group } from '@/types'

defineProps<{
  groups: Group[] | null | undefined
}>()
</script>
