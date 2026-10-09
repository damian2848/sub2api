<template>
  <section class="mt-5" aria-live="polite">
    <template v-if="expired || terminalStatus">
      <div class="rounded-xl border border-gray-200 bg-gray-50 p-5 text-center dark:border-dark-700 dark:bg-dark-800" role="status">
        <Icon name="exclamationCircle" size="lg" class="mx-auto mb-3 text-gray-400" aria-hidden="true" />
        <p class="font-semibold text-gray-900 dark:text-white">{{ statusTitle }}</p>
        <p class="mt-2 text-sm text-gray-500 dark:text-gray-400">{{ statusHint }}</p>
      </div>
    </template>
    <template v-else>
      <div v-if="action" class="flex flex-col items-center gap-3 rounded-xl bg-gray-50 px-4 py-5 dark:bg-dark-800/50">
        <h3 class="text-base font-semibold text-gray-900 dark:text-white">{{ t(action.method === 'alipay' ? 'payment.qr.scanAlipay' : 'payment.qr.scanWxpay') }}</h3>
        <div v-if="action.qrUrl" class="max-w-full rounded-2xl border border-gray-200 bg-white p-3 shadow-sm">
          <img :src="action.qrUrl" :alt="t(action.method === 'alipay' ? 'payment.qr.scanAlipay' : 'payment.qr.scanWxpay')" class="h-auto w-48 max-w-full" width="192" height="192" />
        </div>
        <p v-if="action.qrUrl" class="max-w-xs text-center text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t(action.method === 'alipay' ? 'payment.stripeInline.alipayHint' : 'payment.qr.scanWxpayHint') }}</p>
        <a v-if="action.payUrl" :href="action.payUrl" target="_blank" rel="noopener noreferrer" class="flex items-center gap-1 text-sm font-medium text-sky-700 underline-offset-4 hover:underline focus-visible:rounded focus-visible:outline focus-visible:outline-2 focus-visible:outline-sky-500 dark:text-sky-400">
          {{ t('payment.stripeInline.openPaymentPage') }}
          <Icon name="arrowRight" size="sm" aria-hidden="true" />
        </a>
      </div>
      <div class="mt-3 flex items-center justify-center gap-2 text-xs leading-5 text-gray-500 dark:text-gray-400" role="status">
        <span class="h-1.5 w-1.5 shrink-0 rounded-full bg-emerald-500" aria-hidden="true"></span>
        {{ t(action ? 'payment.stripeInline.autoUpdate' : 'payment.result.processingHint') }}
      </div>
    </template>

    <div v-if="order" class="mt-5 border-t border-gray-100 pt-4 dark:border-dark-700">
      <h4 class="mb-3 text-xs font-medium text-gray-500 dark:text-gray-400">{{ t('payment.stripeInline.paymentInfo') }}</h4>
      <dl class="space-y-2.5 text-xs leading-5">
        <div class="flex justify-between gap-4">
          <dt class="shrink-0 text-gray-500 dark:text-gray-400">{{ t('payment.actualPay') }}</dt>
          <dd class="font-semibold text-gray-900 dark:text-white">{{ formattedAmount }}</dd>
        </div>
        <div v-if="action" class="flex justify-between gap-4">
          <dt class="shrink-0 text-gray-500 dark:text-gray-400">{{ t('payment.paymentMethod') }}</dt>
          <dd class="text-gray-900 dark:text-white">{{ t(action.method === 'alipay' ? 'payment.methods.alipay' : 'payment.methods.wxpay') }}</dd>
        </div>
        <div class="flex justify-between gap-4">
          <dt class="shrink-0 text-gray-500 dark:text-gray-400">{{ t('payment.orders.orderId') }}</dt>
          <dd class="text-gray-900 dark:text-white">#{{ order.id }}</dd>
        </div>
        <div v-if="order.out_trade_no" class="flex justify-between gap-4">
          <dt class="shrink-0 text-gray-500 dark:text-gray-400">{{ t('payment.orders.orderNo') }}</dt>
          <dd class="min-w-0 break-all text-right text-gray-900 dark:text-white">{{ order.out_trade_no }}</dd>
        </div>
        <div v-if="!expired && !terminalStatus && remainingMs !== null" class="flex justify-between gap-4">
          <dt class="shrink-0 text-gray-500 dark:text-gray-400">{{ t('payment.stripeInline.orderTimeRemaining') }}</dt>
          <dd class="font-medium tabular-nums text-gray-900 dark:text-white">{{ countdown }}</dd>
        </div>
      </dl>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import type { PaymentOrder } from '@/types/payment'
import type { StripeWalletAction } from './useStripeCheckout'

const props = defineProps<{
  action: StripeWalletAction | null
  order: PaymentOrder | null
  formattedAmount: string
  remainingMs: number | null
  expired: boolean
  terminalStatus: 'EXPIRED' | 'CANCELLED' | 'FAILED' | null
}>()
const { t } = useI18n()
const countdown = computed(() => {
  const seconds = Math.ceil((props.remainingMs || 0) / 1000)
  return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`
})
const statusTitle = computed(() => t(props.expired ? 'payment.qr.expired' : props.terminalStatus === 'CANCELLED' ? 'payment.qr.cancelled' : 'payment.result.failed'))
const statusHint = computed(() => t(props.expired ? 'payment.qr.expiredDesc' : props.terminalStatus === 'CANCELLED' ? 'payment.qr.cancelledDesc' : 'payment.stripeInline.failedHint'))
</script>
