<template>
  <div class="stripe-checkout rounded-2xl border border-gray-200 bg-white p-5 shadow-sm dark:border-dark-700 dark:bg-dark-900 sm:p-6">
    <div class="mb-5 flex items-center justify-between gap-4 border-b border-gray-100 pb-5 dark:border-dark-700">
      <div class="min-w-0">
        <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('payment.actualPay') }}</p>
        <p class="mt-1 break-words text-3xl font-semibold tracking-tight text-gray-900 dark:text-white">{{ formattedAmount }}</p>
      </div>
      <span v-if="order" class="shrink-0 rounded-lg bg-gray-50 px-3 py-1.5 text-xs tabular-nums text-gray-500 dark:bg-dark-800 dark:text-gray-400">#{{ order.id }}</span>
    </div>

    <!-- Keep Stripe mounted so its fields and validation survive status changes. -->
    <div v-show="!showStatus" v-if="hasPaymentElement">
      <p class="mb-3 text-sm font-medium text-gray-700 dark:text-gray-200">{{ t('payment.paymentMethod') }}</p>
      <slot />
    </div>
    <div v-if="walletIcon && (showStatus || !hasPaymentElement)" class="mb-4 flex items-center gap-3">
      <div class="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-gray-50 dark:bg-dark-800">
        <img :src="walletIcon" alt="" class="h-7 w-7" width="28" height="28" />
      </div>
      <span class="text-sm font-medium text-gray-900 dark:text-gray-100">{{ t(walletMethod === 'alipay' ? 'payment.methods.alipay' : 'payment.methods.wxpay') }}</span>
    </div>
    <p v-if="error" class="mt-4 rounded-lg bg-red-50 px-3 py-2.5 text-sm leading-6 text-red-600 dark:bg-red-950/30 dark:text-red-400" role="alert">{{ error }}</p>
    <button
      type="button"
      class="btn-stripe checkout-pay mt-5 flex min-h-12 w-full items-center justify-center gap-2 rounded-xl px-4 py-3 text-sm font-semibold transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500 focus-visible:ring-offset-2 dark:focus-visible:ring-offset-dark-900"
      :class="{ 'checkout-pay--status': showStatus }"
      :data-wallet="walletMethod"
      :disabled="locked || (hasPaymentElement && !ready)"
      @click="$emit('pay')"
    >
      <span v-if="submitting || (pending && !expired && !terminalStatus)" class="h-4 w-4 animate-spin rounded-full border-2 border-current border-t-transparent" aria-hidden="true"></span>
      {{ buttonLabel }}
      <Icon v-if="!locked" name="arrowRight" size="sm" aria-hidden="true" />
    </button>
    <StripeWalletPanel
      v-if="showStatus"
      :action="action"
      :order="order"
      :formatted-amount="formattedAmount"
      :remaining-ms="remainingMs"
      :expired="expired"
      :terminal-status="terminalStatus"
    />
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import alipayIcon from '@/assets/icons/alipay.svg'
import wxpayIcon from '@/assets/icons/wxpay.svg'
import type { PaymentOrder } from '@/types/payment'
import StripeWalletPanel from './StripeWalletPanel.vue'
import type { StripeWalletAction } from './useStripeCheckout'

const props = withDefaults(defineProps<{
  formattedAmount: string
  order: PaymentOrder | null
  hasPaymentElement?: boolean
  ready: boolean
  locked: boolean
  submitting: boolean
  pending: boolean
  showStatus: boolean
  method?: string
  action: StripeWalletAction | null
  error: string
  remainingMs: number | null
  expired: boolean
  terminalStatus: 'EXPIRED' | 'CANCELLED' | 'FAILED' | null
}>(), { hasPaymentElement: true, method: '' })
defineEmits<{ pay: [] }>()
const { t } = useI18n()
const walletMethod = computed(() => props.action?.method || props.method)
const walletIcon = computed(() => walletMethod.value === 'alipay' ? alipayIcon : walletMethod.value === 'wechat_pay' ? wxpayIcon : '')
const buttonLabel = computed(() => t(
  props.expired ? 'payment.qr.expired'
    : props.terminalStatus === 'CANCELLED' ? 'payment.qr.cancelled'
      : props.terminalStatus === 'FAILED' ? 'payment.result.failed'
        : props.submitting ? 'common.processing'
          : props.pending ? 'payment.qr.waitingPayment' : 'payment.stripePay',
))
</script>

<style scoped>
.checkout-pay {
  background: #4f46e5;
  color: #fff;
  box-shadow: none;
}
.checkout-pay:hover:not(:disabled) { background: #4338ca; }
.checkout-pay[data-wallet='alipay'] { background: #0369a1; }
.checkout-pay[data-wallet='alipay']:hover:not(:disabled) { background: #075985; }
.checkout-pay[data-wallet='wechat_pay'] { background: #15803d; }
.checkout-pay[data-wallet='wechat_pay']:hover:not(:disabled) { background: #166534; }
.checkout-pay:disabled { cursor: not-allowed; opacity: 0.6; }
.checkout-pay.checkout-pay--status {
  @apply border border-gray-200 bg-gray-50 text-gray-600 dark:border-dark-700 dark:bg-dark-800 dark:text-gray-300;
  opacity: 1;
  cursor: default;
}
</style>
