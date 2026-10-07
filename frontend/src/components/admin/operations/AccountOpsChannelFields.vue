<template>
  <section class="mt-6 space-y-3">
    <div class="flex items-center justify-between gap-3">
      <h4 class="text-sm font-medium">{{ t('accountOps.robotChannels') }}</h4>
      <button v-if="!single" type="button" class="text-xs text-primary-600 disabled:opacity-50" :disabled="modelValue.length >= 5 || !encryptionConfigured" data-testid="account-ops-add-webhook" @click="add">{{ t('accountOps.addRobot') }}</button>
    </div>
    <p class="text-xs leading-5 text-gray-500">{{ t('accountOps.robotHint') }}</p>
    <p v-if="!encryptionConfigured" role="alert" class="text-xs text-amber-700">{{ t('accountOps.encryptionMissing') }}</p>
    <div v-for="hook in modelValue" :key="hook.id" class="space-y-3 rounded-xl border border-gray-200 p-3 dark:border-dark-600">
      <div class="flex items-center gap-2">
        <label class="flex shrink-0 items-center gap-1.5 text-xs"><input type="checkbox" :checked="hook.enabled" :data-testid="`account-ops-webhook-enabled-${hook.id}`" @change="patch(hook.id, { enabled: ($event.target as HTMLInputElement).checked })" />{{ t('accountOps.channelEnabled') }}</label>
        <select :value="hook.provider" :aria-label="t('accountOps.provider')" class="input min-w-0 flex-1 text-xs" :data-testid="`account-ops-webhook-provider-${hook.id}`" @change="changeProvider(hook.id, ($event.target as HTMLSelectElement).value as AccountOpsWebhookProvider)">
          <option v-for="provider in providers" :key="provider" :value="provider">{{ t(`accountOps.providers.${provider}`) }}</option>
        </select>
        <button v-if="!single" type="button" class="shrink-0 text-xs text-red-600" :aria-label="t('accountOps.removeRobot')" @click="remove(hook.id)">{{ t('common.delete') }}</button>
      </div>
      <label class="block space-y-1 text-xs"><span>{{ t('accountOps.webhookUrl') }}</span><input v-model.trim="inputs[hook.id]!.url" type="password" autocomplete="new-password" spellcheck="false" class="input w-full" :disabled="!encryptionConfigured" :placeholder="hook.url_configured ? t('accountOps.retainCredential') : t('accountOps.webhookPlaceholder')" :data-testid="`account-ops-webhook-url-${hook.id}`" /></label>
      <label v-if="hook.provider !== 'wecom'" class="block space-y-1 text-xs"><span>{{ t('accountOps.signingSecret') }}</span><input v-model="inputs[hook.id]!.secret" type="password" autocomplete="new-password" spellcheck="false" class="input w-full" :disabled="!encryptionConfigured || inputs[hook.id]!.clearSecret" :placeholder="hook.secret_configured ? t('accountOps.retainCredential') : t('accountOps.optionalSecret')" :data-testid="`account-ops-webhook-secret-${hook.id}`" /></label>
      <label v-if="hook.secret_configured && hook.provider !== 'wecom'" class="flex items-center gap-2 text-xs text-gray-500"><input v-model="inputs[hook.id]!.clearSecret" type="checkbox" :data-testid="`account-ops-webhook-clear-secret-${hook.id}`" />{{ t('accountOps.clearSecret') }}</label>
      <div class="flex items-center justify-between gap-2 text-xs text-gray-500"><span>{{ t(hook.url_configured ? 'accountOps.credentialSaved' : 'accountOps.saveBeforeTest') }}</span><button type="button" class="text-primary-600 disabled:opacity-40" :disabled="!canTest(hook)" :data-testid="`account-ops-webhook-test-${hook.id}`" @click="emit('test', hook.id)">{{ t(testingId === hook.id ? 'common.loading' : 'accountOps.testRobot') }}</button></div>
    </div>
  </section>
</template>
<script setup lang="ts">
import { computed, onBeforeUnmount, reactive, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import type { AccountOpsWebhook, AccountOpsWebhookInput, AccountOpsWebhookProvider } from '@/api/admin/accountOps'
const props = defineProps<{ modelValue: AccountOpsWebhook[]; encryptionConfigured: boolean; testingId?: string | null; single?: boolean }>()
const emit = defineEmits<{ (event: 'update:modelValue', value: AccountOpsWebhook[]): void; (event: 'test', id: string): void }>()
const { t } = useI18n()
const providers: AccountOpsWebhookProvider[] = ['wecom', 'dingtalk', 'feishu']
const inputs = reactive<Record<string, { url: string; secret: string; clearSecret: boolean }>>({})
watch(() => props.modelValue.map(h => h.id), ids => {
  for (const id of ids) inputs[id] ??= { url: '', secret: '', clearSecret: false }
  for (const id of Object.keys(inputs)) if (!ids.includes(id)) delete inputs[id]
}, { immediate: true, flush: 'sync' })
const hasSensitiveChanges = computed(() => Object.values(inputs).some(i => !!i.url || !!i.secret || i.clearSecret))
const patch = (id: string, values: Partial<AccountOpsWebhook>) => emit('update:modelValue', props.modelValue.map(h => h.id === id ? { ...h, ...values } : h))
const changeProvider = (id: string, provider: AccountOpsWebhookProvider) => {
  inputs[id] = { url: '', secret: '', clearSecret: false }
  patch(id, { provider, url_configured: false, secret_configured: false })
}
const add = () => {
  if (props.modelValue.length >= 5 || !props.encryptionConfigured) return
  const id = Array.from(crypto.getRandomValues(new Uint8Array(16)), byte => byte.toString(16).padStart(2, '0')).join('')
  emit('update:modelValue', [...props.modelValue, { id, provider: 'wecom', enabled: true, url_configured: false, secret_configured: false }])
}
const remove = (id: string) => { delete inputs[id]; emit('update:modelValue', props.modelValue.filter(h => h.id !== id)) }
const canTest = (h: AccountOpsWebhook) => !!h.url_configured && !props.testingId && !inputs[h.id]?.url && !inputs[h.id]?.secret && !inputs[h.id]?.clearSecret
const prepare = (): AccountOpsWebhookInput[] => props.modelValue.map(h => {
  const i = inputs[h.id]
  return { id: h.id, provider: h.provider, enabled: h.enabled, ...(i?.url ? { url: i.url } : {}), ...(i?.secret && !i.clearSecret ? { secret: i.secret } : {}), ...(i?.clearSecret ? { clear_secret: true } : {}) }
})
const clearInputs = () => { for (const id of Object.keys(inputs)) inputs[id] = { url: '', secret: '', clearSecret: false } }
onBeforeUnmount(clearInputs)
defineExpose({ prepare, clearInputs, hasSensitiveChanges })
</script>
