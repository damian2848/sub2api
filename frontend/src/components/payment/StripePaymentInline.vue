<template>
  <div class="space-y-4">
    <div v-if="loading" class="flex items-center justify-center py-12">
      <div class="h-8 w-8 animate-spin rounded-full border-4 border-primary-500 border-t-transparent"></div>
    </div>
    <div v-else-if="initError" class="card p-6 text-center">
      <p class="text-sm text-red-600 dark:text-red-400">{{ initError }}</p>
      <button class="btn btn-secondary mt-4" @click="$emit('back')">{{ t('payment.result.backToRecharge') }}</button>
    </div>
    <!-- Success -->
    <template v-else-if="success">
      <div class="card p-6">
        <div class="flex flex-col items-center space-y-4 py-4">
          <div class="flex h-16 w-16 items-center justify-center rounded-full bg-green-100 dark:bg-green-900/30">
            <Icon name="check" size="lg" class="text-green-500" />
          </div>
          <p class="text-lg font-bold text-gray-900 dark:text-white">{{ t('payment.result.success') }}</p>
          <div class="w-full rounded-xl bg-gray-50 p-4 dark:bg-dark-800">
            <div class="space-y-2 text-sm">
              <div class="flex justify-between">
                <span class="text-gray-500 dark:text-gray-400">{{ t('payment.orders.orderId') }}</span>
                <span class="font-medium text-gray-900 dark:text-white">#{{ orderId }}</span>
              </div>
              <div v-if="amount > 0" class="flex justify-between">
                <span class="text-gray-500 dark:text-gray-400">{{ t('payment.orders.amount') }}</span>
                <span class="font-medium text-gray-900 dark:text-white">{{ creditedAmountSymbol }}{{ amount.toFixed(2) }}</span>
              </div>
              <div class="flex justify-between">
                <span class="text-gray-500 dark:text-gray-400">{{ t('payment.orders.payAmount') }}</span>
                <span class="font-medium text-gray-900 dark:text-white">{{ paymentAmountSymbol }}{{ payAmount.toFixed(2) }}</span>
              </div>
            </div>
          </div>
          <button class="btn btn-primary" @click="$emit('done')">{{ t('common.confirm') }}</button>
        </div>
      </div>
    </template>
    <template v-else>
      <StripeCheckoutForm
        :order="order"
        :formatted-amount="paymentAmountSymbol + payAmount.toFixed(2)"
        :ready="ready"
        :locked="locked"
        :submitting="submitting"
        :pending="pending"
        :show-status="showStatus"
        :method="selectedType"
        :action="walletAction"
        :error="error"
        :remaining-ms="remainingMs"
        :expired="expired"
        :terminal-status="terminalStatus"
        @pay="handlePay"
      >
        <div ref="stripeMount" :class="!ready ? 'min-h-[144px]' : ''"></div>
      </StripeCheckoutForm>
      <!-- Cancel order -->
      <button class="btn btn-secondary w-full" :disabled="cancelling || submitting" @click="handleCancel">
        {{ cancelling ? t('common.processing') : t('payment.qr.cancelOrder') }}
      </button>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, onMounted, onUnmounted, nextTick } from 'vue'
import { useI18n } from 'vue-i18n'
import { extractI18nErrorMessage } from '@/utils/apiError'
import { paymentAPI } from '@/api/payment'
import { useAppStore } from '@/stores'
import { currencySymbol } from '@/components/payment/currency'
import type { PaymentOrder } from '@/types/payment'
import StripeCheckoutForm from './StripeCheckoutForm.vue'
import { useStripeCheckout } from './useStripeCheckout'
import Icon from '@/components/icons/Icon.vue'

const props = defineProps<{
  orderId: number
  amount: number
  clientSecret: string
  orderType?: 'balance' | 'subscription'
  publishableKey: string
  payAmount: number
  currency?: string
}>()

const emit = defineEmits<{ success: []; done: []; back: []; redirect: [orderId: number, payUrl: string] }>()

const { t } = useI18n()
const appStore = useAppStore()

const stripeMount = ref<HTMLElement | null>(null)
const loading = ref(true)
const initError = ref('')
const cancelling = ref(false)
const creditedAmountSymbol = currencySymbol('USD')
const paymentAmountSymbol = computed(() => currencySymbol(props.currency))

const order = ref<PaymentOrder | null>(null)
const {
  error, submitting, succeeded: success, ready, pending, selectedType, walletAction,
  remainingMs, expired, terminalStatus, locked, showStatus, initialize, pay,
} = useStripeCheckout({
  orderId: () => props.orderId,
  clientSecret: () => props.clientSecret,
  order,
  onSuccess: () => emit('success'),
})
let disposed = false

onMounted(async () => {
  try {
    const { data } = await paymentAPI.getOrder(props.orderId)
    if (disposed) return
    order.value = data
    const { loadStripe } = await import('@stripe/stripe-js/pure')
    const stripe = await loadStripe(props.publishableKey)
    if (disposed) return
    if (!stripe) { initError.value = t('payment.stripeLoadFailed'); return }

    loading.value = false
    await nextTick()
    if (disposed || !stripeMount.value) return

    initialize(stripe, stripeMount.value)
  } catch (err: unknown) {
    if (!disposed) initError.value = extractI18nErrorMessage(err, t, 'payment.errors', t('payment.stripeLoadFailed'))
  } finally {
    if (!disposed) loading.value = false
  }
})

async function handlePay() {
  if (!cancelling.value) await pay()
}

onUnmounted(() => { disposed = true })

async function handleCancel() {
  if (!props.orderId || cancelling.value) return
  cancelling.value = true
  try {
    await paymentAPI.cancelOrder(props.orderId)
    emit('back')
  } catch (err: unknown) {
    appStore.showError(extractI18nErrorMessage(err, t, 'payment.errors', t('common.error')))
  } finally {
    cancelling.value = false
  }
}
</script>
