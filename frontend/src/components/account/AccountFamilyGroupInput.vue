<template>
  <div>
    <label :for="inputId" class="input-label">{{ t('admin.accounts.familyGroup') }}</label>
    <input
      :id="inputId" :value="modelValue" :list="`${inputId}-options`"
      type="text" maxlength="100" class="input" data-testid="family-group-input"
      :placeholder="t('admin.accounts.familyGroupPlaceholder')"
      @input="$emit('update:modelValue', ($event.target as HTMLInputElement).value)"
      @focus="loadOptions"
    />
    <datalist :id="`${inputId}-options`">
      <option v-for="group in groups" :key="group.name" :value="group.name">{{ group.name }} ({{ group.account_count }})</option>
    </datalist>
    <p class="input-hint">{{ t('admin.accounts.familyGroupHint') }}</p>
  </div>
</template>

<script setup lang="ts">
import { ref, useId } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { AccountFamilyGroup } from '@/api/admin/accounts'

defineProps<{ modelValue: string }>()
defineEmits<{ 'update:modelValue': [value: string] }>()
const { t } = useI18n()
const inputId = `family-group-${useId()}`
const groups = ref<AccountFamilyGroup[]>([])
const loadOptions = async () => {
  try {
    groups.value = await adminAPI.accounts.listFamilyGroups()
  } catch {
    // Existing names are suggestions; typing a family name remains available.
  }
}
</script>
