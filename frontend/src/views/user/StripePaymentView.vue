<template>
  <component :is="isPopup ? 'div' : AppLayout" :class="isPopup ? 'min-h-screen bg-gray-50 dark:bg-dark-900' : ''">
    <div class="mx-auto max-w-lg space-y-6 py-8" :class="isPopup ? 'px-4' : ''">
      <div v-if="loading" class="flex items-center justify-center py-20">
        <div class="h-8 w-8 animate-spin rounded-full border-4 border-primary-500 border-t-transparent"></div>
      </div>
      <div v-else-if="initError" class="card p-8 text-center">
        <div class="mx-auto mb-4 flex h-16 w-16 items-center justify-center rounded-full bg-red-100 dark:bg-red-900/30">
          <Icon name="exclamationCircle" size="xl" class="text-red-500" />
        </div>
        <h3 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('payment.stripeLoadFailed') }}</h3>
        <p class="mt-2 text-sm text-gray-500 dark:text-gray-400">{{ initError }}</p>
        <button class="btn btn-primary mt-6" @click="router.push('/purchase')">{{ t('payment.result.backToRecharge') }}</button>
      </div>
      <template v-else>
        <!-- 成功状态 -->
        <template v-if="stripeSuccess">
          <div class="card p-6 text-center">
            <div class="flex flex-col items-center gap-3 py-4">
              <div class="flex h-16 w-16 items-center justify-center rounded-full bg-green-100 dark:bg-green-900/30">
                <Icon name="check" size="lg" class="text-green-500" />
              </div>
              <p class="text-lg font-bold text-gray-900 dark:text-white">{{ t('payment.result.success') }}</p>
              <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('payment.stripeSuccessProcessing') }}</p>
            </div>
          </div>
        </template>

        <!-- 扫码区域保留在支付按钮下方 -->
        <template v-else>
          <StripeCheckoutForm
            :order="order"
            :formatted-amount="formatGatewayAmount(order?.pay_amount || 0)"
            :has-payment-element="showPaymentElement"
            :ready="stripeReady"
            :locked="locked"
            :submitting="stripeSubmitting"
            :pending="pending"
            :show-status="showStatus"
            :method="directMethod || selectedType"
            :action="walletAction"
            :error="stripeError"
            :remaining-ms="remainingMs"
            :expired="expired"
            :terminal-status="terminalStatus"
            @pay="pay(directMethod)"
          >
            <div id="stripe-payment-element" :class="!stripeReady ? 'min-h-[144px]' : ''"></div>
          </StripeCheckoutForm>
          <div class="text-center">
            <button class="btn btn-secondary" @click="router.push('/purchase')">{{ t('payment.result.backToRecharge') }}</button>
          </div>
        </template>
      </template>
    </div>
  </component>
</template>

<script setup lang="ts">
import { ref, computed, nextTick, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import { usePaymentStore } from '@/stores/payment'
import { paymentAPI } from '@/api/payment'
import { extractI18nErrorMessage } from '@/utils/apiError'
import { formatPaymentAmount, normalizePaymentCurrency } from '@/components/payment/currency'
import { PAYMENT_RECOVERY_STORAGE_KEY, readPaymentRecoverySnapshot } from '@/components/payment/paymentFlow'
import type { PaymentOrder } from '@/types/payment'
import AppLayout from '@/components/layout/AppLayout.vue'
import Icon from '@/components/icons/Icon.vue'
import StripeCheckoutForm from '@/components/payment/StripeCheckoutForm.vue'
import { useStripeCheckout } from '@/components/payment/useStripeCheckout'

const i18n = useI18n()
const { t } = i18n
const route = useRoute()
const router = useRouter()
const paymentStore = usePaymentStore()

// 弹窗模式：指定支付宝或微信方式时跳过 AppLayout
const isPopup = computed(() => !!route.query.method)
const directMethod = computed(() => route.query.method === 'alipay' || route.query.method === 'wechat_pay' ? route.query.method : undefined)

const loading = ref(true)
const initError = ref('')
const order = ref<PaymentOrder | null>(null)
const currency = ref('CNY')
const showPaymentElement = ref(false)

const {
  error: stripeError, submitting: stripeSubmitting, succeeded: stripeSuccess, ready: stripeReady,
  pending, selectedType, walletAction, remainingMs, expired, terminalStatus, locked, showStatus, initialize, pay,
} = useStripeCheckout({
  orderId: () => Number(route.query.order_id),
  clientSecret: () => String(route.query.client_secret || ''),
  order,
  onSuccess: scheduleClose,
})
let disposed = false
let redirectTimer: ReturnType<typeof setTimeout> | null = null

onMounted(async () => {
  const orderId = Number(route.query.order_id)
  const clientSecret = String(route.query.client_secret || '')
  const method = String(route.query.method || '')
  const resumeToken = typeof route.query.resume_token === 'string' ? route.query.resume_token : undefined

  if (!orderId || !clientSecret) {
    loading.value = false
    initError.value = t('payment.stripeMissingParams')
    return
  }

  try {
    if (typeof window !== 'undefined') {
      const restored = readPaymentRecoverySnapshot(
        window.localStorage.getItem(PAYMENT_RECOVERY_STORAGE_KEY),
        { resumeToken },
      )
      if (restored?.orderId === orderId) {
        currency.value = normalizePaymentCurrency(restored.currency)
      }
    }
    const res = await paymentAPI.getOrder(orderId)
    if (disposed) return
    order.value = res.data
    if (res.data.currency) {
      currency.value = normalizePaymentCurrency(res.data.currency)
    }

    await paymentStore.fetchConfig()
    if (disposed) return
    const publishableKey = paymentStore.config?.stripe_publishable_key
    if (!publishableKey) { initError.value = t('payment.stripeNotConfigured'); return }

    const { loadStripe } = await import('@stripe/stripe-js/pure')
    const stripe = await loadStripe(publishableKey)
    if (disposed) return
    if (!stripe) { initError.value = t('payment.stripeLoadFailed'); return }

    loading.value = false

    if (method === 'alipay' || method === 'wechat_pay') {
      initialize(stripe)
      await pay(method)
    } else {
      showPaymentElement.value = true
      await nextTick()
      if (disposed) return
      initialize(stripe, '#stripe-payment-element')
    }
  } catch (err: unknown) {
    if (!disposed) initError.value = extractI18nErrorMessage(err, t, 'payment.errors', t('payment.stripeLoadFailed'))
  } finally {
    if (!disposed) loading.value = false
  }
})

const localeCode = computed(() => {
  const raw = i18n.locale as unknown
  if (typeof raw === 'string') return raw
  if (raw && typeof raw === 'object' && 'value' in raw) {
    return String((raw as { value?: string }).value || '')
  }
  return undefined
})

function formatGatewayAmount(value: number): string {
  return formatPaymentAmount(value, currency.value, localeCode.value)
}

function scheduleClose() {
  if (disposed || redirectTimer) return
  if (window.opener) {
    redirectTimer = setTimeout(() => { window.close() }, 2000)
  } else {
    redirectTimer = setTimeout(() => {
      router.push({ path: '/payment/result', query: { order_id: String(route.query.order_id || ''), status: 'success' } })
    }, 2000)
  }
}

onUnmounted(() => {
  disposed = true
  if (redirectTimer) clearTimeout(redirectTimer)
})
</script>
