import { computed, onUnmounted, ref, watch, type Ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { usePaymentStore } from '@/stores/payment'
import { extractI18nErrorMessage } from '@/utils/apiError'
import type { PaymentOrder } from '@/types/payment'
import type { PaymentIntent, Stripe, StripeElements, StripePaymentElement } from '@stripe/stripe-js'

export type StripeWalletMethod = 'alipay' | 'wechat_pay'

export interface StripeWalletAction {
  method: StripeWalletMethod
  qrUrl: string
  payUrl: string
}

// Alipay's direct API exposes this action, although Stripe.js types omit it.
type AlipayNextAction = PaymentIntent.NextAction & {
  alipay_handle_redirect?: { url?: string }
}

function paymentPageUrl(value?: string | null): string {
  if (!value) return ''
  try {
    const url = new URL(value)
    return url.protocol === 'https:' && !url.username && !url.password ? url.href : ''
  } catch {
    return ''
  }
}

async function qrImage(data: string): Promise<string> {
  const QRCode = await import('qrcode')
  return QRCode.toDataURL(data, { width: 280, margin: 2, errorCorrectionLevel: 'M' })
}

export function useStripeCheckout(options: {
  orderId: () => number
  clientSecret: () => string
  order: Ref<PaymentOrder | null>
  onSuccess: () => void
}) {
  const { t } = useI18n()
  const paymentStore = usePaymentStore()
  const error = ref('')
  const submitting = ref(false)
  const ready = ref(false)
  const succeeded = ref(false)
  const pending = ref(false)
  const selectedType = ref('')
  const walletAction = ref<StripeWalletAction | null>(null)
  const terminalStatus = ref<'EXPIRED' | 'CANCELLED' | 'FAILED' | null>(null)
  const now = ref(Date.now())
  const remainingMs = computed(() => {
    const expiry = Date.parse(options.order.value?.expires_at || '')
    return Number.isFinite(expiry) ? Math.max(0, expiry - now.value) : null
  })
  const expired = computed(() => remainingMs.value === 0 || terminalStatus.value === 'EXPIRED')
  const locked = computed(() => submitting.value || pending.value || succeeded.value || expired.value || !!terminalStatus.value)
  const showStatus = computed(() => pending.value || expired.value || !!terminalStatus.value)

  let stripe: Stripe | null = null
  let elements: StripeElements | null = null
  let paymentElement: StripePaymentElement | null = null
  let pollTimer: ReturnType<typeof setInterval> | null = null
  const clockTimer = setInterval(() => { now.value = Date.now() }, 1000)
  let polling = false
  let disposed = false

  watch(locked, (value) => { paymentElement?.update({ readOnly: value }) })

  function stopPolling() {
    if (pollTimer) clearInterval(pollTimer)
    pollTimer = null
  }

  function finish() {
    if (disposed || succeeded.value) return
    stopPolling()
    pending.value = false
    walletAction.value = null
    succeeded.value = true
    options.onSuccess()
  }

  function syncOrderStatus() {
    const status = options.order.value?.status
    if (status === 'PAID' || status === 'RECHARGING' || status === 'COMPLETED') {
      finish()
    } else if (status === 'EXPIRED' || status === 'CANCELLED' || status === 'FAILED') {
      terminalStatus.value = status
      walletAction.value = null
      pending.value = false
      stopPolling()
    }
  }

  function initialize(instance: Stripe, target?: HTMLElement | string) {
    if (disposed) return
    stripe = instance
    syncOrderStatus()
    if (!target || succeeded.value) return
    elements = stripe.elements({
      clientSecret: options.clientSecret(),
      paymentMethodCreation: 'manual',
      appearance: {
        theme: document.documentElement.classList.contains('dark') ? 'night' : 'stripe',
        variables: { borderRadius: '8px' },
      },
    })
    paymentElement = elements.create('payment', {
      layout: 'tabs',
      paymentMethodOrder: ['alipay', 'wechat_pay', 'card', 'link'],
      readOnly: locked.value,
    })
    paymentElement.on('ready', () => { if (!disposed) ready.value = true })
    paymentElement.on('change', (event) => { if (!disposed) selectedType.value = event.value.type })
    paymentElement.mount(target)
  }

  function startPolling() {
    if (pollTimer || disposed) return
    pollTimer = setInterval(async () => {
      if (polling || disposed) return
      polling = true
      try {
        const order = await paymentStore.pollOrderStatus(options.orderId())
        if (disposed || !order) return
        options.order.value = order
        syncOrderStatus()
      } catch {
        // A transient request failure must not discard a valid pending payment.
      } finally {
        polling = false
      }
    }, 3000)
  }

  async function acceptIntent(intent: PaymentIntent | undefined, method?: StripeWalletMethod) {
    if (disposed) return
    if (intent?.status === 'succeeded') {
      finish()
      return
    }
    if (intent?.status === 'processing') {
      pending.value = true
      startPolling()
      return
    }
    if (intent?.status !== 'requires_action' || !method) {
      error.value = t('payment.result.failed')
      return
    }

    const nextAction = intent.next_action as AlipayNextAction | null
    const payUrl = method === 'alipay'
      ? paymentPageUrl(nextAction?.alipay_handle_redirect?.url || nextAction?.redirect_to_url?.url)
      : ''
    const wechatQr = nextAction?.wechat_pay_display_qr_code
    let qrUrl = method === 'wechat_pay' ? wechatQr?.image_data_url || '' : ''
    const qrData = method === 'alipay' ? payUrl : wechatQr?.data
    if (!qrUrl && !qrData) {
      error.value = t('payment.result.failed')
      return
    }

    // Once confirmed, prevent repeat confirmation even if rendering the QR fails.
    pending.value = true
    startPolling()
    if (!qrUrl && qrData) {
      try {
        qrUrl = await qrImage(qrData)
      } catch {
        if (!disposed) error.value = t('payment.stripeInline.qrUnavailable')
      }
    }
    if (disposed || succeeded.value || terminalStatus.value) return
    walletAction.value = { method, qrUrl, payUrl }
  }

  async function pay(directMethod?: StripeWalletMethod) {
    if (!stripe || locked.value || (!directMethod && (!elements || !ready.value))) return
    submitting.value = true
    error.value = ''
    const method = directMethod || selectedType.value
    const returnUrl = `${window.location.origin}/payment/result?order_id=${options.orderId()}&status=success`
    try {
      let paymentMethodId: string | undefined
      if (!directMethod && elements) {
        const validation = await elements.submit()
        if (disposed || expired.value || terminalStatus.value) return
        if (validation.error) { error.value = validation.error.message || t('payment.result.failed'); return }
        const result = await stripe.createPaymentMethod({ elements })
        if (disposed || expired.value || terminalStatus.value) return
        if (result.error || !result.paymentMethod) {
          error.value = result.error?.message || t('payment.result.failed')
          return
        }
        paymentMethodId = result.paymentMethod.id
      }

      const result = method === 'alipay'
        ? await stripe.confirmAlipayPayment(options.clientSecret(), {
          return_url: returnUrl, ...(paymentMethodId ? { payment_method: paymentMethodId } : {}),
        }, { handleActions: false })
        : method === 'wechat_pay'
          ? await stripe.confirmWechatPayPayment(options.clientSecret(), {
            payment_method_options: { wechat_pay: { client: 'web' } },
            ...(paymentMethodId ? { payment_method: paymentMethodId } : {}),
          }, { handleActions: false })
          : await stripe.confirmPayment({
            elements: elements!,
            confirmParams: { return_url: returnUrl, payment_method: paymentMethodId },
            redirect: 'if_required',
          })
      if (disposed) return
      if (result.error) {
        error.value = result.error.message || t('payment.result.failed')
      } else {
        await acceptIntent(result.paymentIntent, method === 'alipay' || method === 'wechat_pay' ? method : undefined)
      }
    } catch (err: unknown) {
      if (!disposed) error.value = extractI18nErrorMessage(err, t, 'payment.errors', t('payment.result.failed'))
    } finally {
      if (!disposed) submitting.value = false
    }
  }

  onUnmounted(() => {
    disposed = true
    stopPolling()
    clearInterval(clockTimer)
    paymentElement?.destroy()
  })

  return { error, submitting, ready, succeeded, pending, walletAction, terminalStatus, remainingMs, expired, locked, showStatus, initialize, pay }
}
