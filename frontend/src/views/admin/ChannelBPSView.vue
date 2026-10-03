<template>
  <div class="space-y-8" data-testid="channel-bps">
    <AutoConfigView embedded />
    <BPSImageSettingsPanel />
  </div>
</template>
<script setup lang="ts">
import AutoConfigView from './AutoConfigView.vue'
import BPSImageSettingsPanel from '@/components/admin/operations/BPSImageSettingsPanel.vue'
</script>
