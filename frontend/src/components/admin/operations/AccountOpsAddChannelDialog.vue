<template>
  <BaseDialog :show="show" :title="t('accountOps.addChannel')" width="normal" @close="emit('close')">
    <div class="grid gap-3 sm:grid-cols-2">
      <button type="button" :disabled="emailConfigured" class="rounded-lg border border-gray-200 p-4 text-left hover:border-primary-500 focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-500 disabled:cursor-not-allowed disabled:opacity-50 dark:border-dark-600" data-testid="add-channel-email" @click="emit('select', 'email')">
        <span class="text-sm font-medium">{{ t('accountOps.providers.email') }}</span>
        <span class="mt-1 block text-xs text-gray-500">{{ t(emailConfigured ? 'accountOps.channelAlreadyAdded' : 'accountOps.emailChannelHint') }}</span>
      </button>
      <button v-for="provider in providers" :key="provider" type="button" :disabled="!encryptionConfigured || webhookCount >= 5" class="rounded-lg border border-gray-200 p-4 text-left hover:border-primary-500 focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-500 disabled:cursor-not-allowed disabled:opacity-50 dark:border-dark-600" :data-testid="`add-channel-${provider}`" @click="emit('select', provider)">
        <span class="text-sm font-medium">{{ t(`accountOps.providers.${provider}`) }}</span>
        <span class="mt-1 block text-xs text-gray-500">{{ t('accountOps.robotChannelHint') }}</span>
      </button>
    </div>
    <p v-if="webhookCount >= 5" class="mt-4 text-xs text-gray-500">{{ t('accountOps.robotLimitReached') }}</p>
    <p v-else-if="!encryptionConfigured" class="mt-4 text-xs text-amber-700">{{ t('accountOps.encryptionMissing') }}</p>
    <template #footer><button type="button" class="btn btn-secondary" @click="emit('close')">{{ t('common.cancel') }}</button></template>
  </BaseDialog>
</template>
<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import type { AccountOpsWebhookProvider } from '@/api/admin/accountOps'

defineProps<{ show: boolean; emailConfigured: boolean; webhookCount: number; encryptionConfigured: boolean }>()
const emit = defineEmits<{ (event: 'close'): void; (event: 'select', provider: 'email' | AccountOpsWebhookProvider): void }>()
const { t } = useI18n()
const providers: AccountOpsWebhookProvider[] = ['wecom', 'dingtalk', 'feishu']
</script>
