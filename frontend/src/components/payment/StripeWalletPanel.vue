<template>
  <section class="mt-6 border-t border-gray-200 pt-6 dark:border-dark-600" aria-live="polite">
    <template v-if="expired || terminalStatus">
      <div class="rounded-xl bg-gray-50 p-5 text-center dark:bg-dark-800" role="status">
        <p class="font-semibold text-gray-900 dark:text-white">{{ statusTitle }}</p>
        <p class="mt-2 text-sm text-gray-500 dark:text-gray-400">{{ statusHint }}</p>
      </div>
    </template>
    <template v-else>
      <div v-if="action" class="flex flex-col items-center gap-3">
        <h3 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t(action.method === 'alipay' ? 'payment.qr.scanAlipay' : 'payment.qr.scanWxpay') }}</h3>
        <div v-if="action.qrUrl" class="max-w-full rounded-xl border-2 bg-white p-2" :class="action.method === 'alipay' ? 'border-sky-400' : 'border-green-500'">
          <img :src="action.qrUrl" :alt="t(action.method === 'alipay' ? 'payment.qr.scanAlipay' : 'payment.qr.scanWxpay')" class="h-auto w-56 max-w-full" width="224" height="224" />
        </div>
        <p v-if="action.qrUrl" class="text-center text-sm leading-6 text-gray-500 dark:text-gray-400">{{ t(action.method === 'alipay' ? 'payment.stripeInline.alipayHint' : 'payment.qr.scanWxpayHint') }}</p>
        <a v-if="action.payUrl" :href="action.payUrl" target="_blank" rel="noopener noreferrer" class="btn btn-secondary text-sm">{{ t('payment.stripeInline.openPaymentPage') }}</a>
      </div>
      <div class="mt-4 flex items-center justify-center gap-2 text-sm text-gray-500 dark:text-gray-400" role="status">
        <span class="h-4 w-4 animate-spin rounded-full border-2 border-primary-500 border-t-transparent" aria-hidden="true"></span>
        {{ t(action ? 'payment.qr.waitingPayment' : 'payment.result.processingHint') }}
      </div>
    </template>

    <div v-if="order" class="mt-5 rounded-xl bg-gray-50 p-4 dark:bg-dark-800">
      <h4 class="mb-3 text-sm font-semibold text-gray-900 dark:text-white">{{ t('payment.stripeInline.paymentInfo') }}</h4>
      <dl class="space-y-3 text-sm">
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
