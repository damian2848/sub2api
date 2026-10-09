import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'

const routeState = vi.hoisted(() => ({ query: {} as Record<string, unknown> }))
const routerPush = vi.hoisted(() => vi.fn())
const getOrder = vi.hoisted(() => vi.fn())
const cancelOrder = vi.hoisted(() => vi.fn())
const paymentStore = vi.hoisted(() => ({
  config: { stripe_publishable_key: 'pk_test' } as { stripe_publishable_key?: string },
  fetchConfig: vi.fn(), pollOrderStatus: vi.fn(),
}))
const loadStripe = vi.hoisted(() => vi.fn())
const toDataURL = vi.hoisted(() => vi.fn())
const stripeElements = vi.hoisted(() => ({ create: vi.fn(), submit: vi.fn() }))
const stripePaymentElement = vi.hoisted(() => ({ mount: vi.fn(), on: vi.fn(), update: vi.fn(), destroy: vi.fn() }))
const stripeInstance = vi.hoisted(() => ({
  elements: vi.fn(), createPaymentMethod: vi.fn(), confirmPayment: vi.fn(),
  confirmAlipayPayment: vi.fn(), confirmWechatPayPayment: vi.fn(),
}))
const handlers = vi.hoisted(() => ({} as Record<string, (event?: { value: { type: string } }) => void>))

vi.mock('vue-router', async () => ({
  ...await vi.importActual<typeof import('vue-router')>('vue-router'),
  useRoute: () => routeState, useRouter: () => ({ push: routerPush }),
}))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key, locale: { value: 'zh-CN' } }),
}))
vi.mock('@/stores/payment', () => ({ usePaymentStore: () => paymentStore }))
vi.mock('@/stores', () => ({ useAppStore: () => ({ showError: vi.fn() }) }))
vi.mock('@/api/payment', () => ({ paymentAPI: { getOrder, cancelOrder } }))
vi.mock('@stripe/stripe-js/pure', () => ({ loadStripe }))
vi.mock('qrcode', () => ({ toDataURL }))

import StripePaymentView from '../StripePaymentView.vue'
import StripePaymentInline from '@/components/payment/StripePaymentInline.vue'
import { formatPaymentAmount } from '@/components/payment/currency'
import type { PaymentOrder } from '@/types/payment'

function orderFactory(overrides: Partial<PaymentOrder> = {}): PaymentOrder {
  return {
    id: 42, user_id: 7, amount: 100, pay_amount: 103, currency: 'CNY', fee_rate: 0.03,
    payment_type: 'stripe', out_trade_no: 'sub2_stripe_42', status: 'PENDING', order_type: 'balance',
    created_at: '2026-10-10T12:00:00Z', expires_at: '2026-10-10T12:30:00Z', refund_amount: 0,
    ...overrides,
  }
}
const wrappers: VueWrapper[] = []
function mountView() {
  const wrapper = mount(StripePaymentView, {
    global: { stubs: { AppLayout: { template: '<div><slot /></div>' }, Icon: true } },
  })
  wrappers.push(wrapper)
  return wrapper
}
function selectMethod(type: string) { handlers.change?.({ value: { type } }) }
async function pay(wrapper: VueWrapper, method = 'alipay') {
  selectMethod(method)
  await wrapper.get('button.btn-stripe').trigger('click')
  await flushPromises()
}
function alipayIntent(action: Record<string, { url: string }> = { alipay_handle_redirect: { url: 'https://hooks.stripe.com/alipay/authorize' } }) {
  return { paymentIntent: { id: 'pi_42', status: 'requires_action', next_action: action } }
}

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date('2026-10-10T12:00:00Z'))
  routeState.query = { order_id: '42', client_secret: 'pi_secret_42' }
  routerPush.mockReset()
  getOrder.mockReset().mockResolvedValue({ data: orderFactory() })
  cancelOrder.mockReset().mockResolvedValue({})
  paymentStore.config = { stripe_publishable_key: 'pk_test' }
  paymentStore.fetchConfig.mockReset().mockResolvedValue(undefined)
  paymentStore.pollOrderStatus.mockReset().mockResolvedValue(null)
  loadStripe.mockReset().mockResolvedValue(stripeInstance)
  toDataURL.mockReset().mockResolvedValue('data:image/png;base64,QR')
  stripeElements.create.mockReset().mockReturnValue(stripePaymentElement)
  stripeElements.submit.mockReset().mockResolvedValue({})
  stripePaymentElement.mount.mockReset()
  stripePaymentElement.update.mockReset()
  stripePaymentElement.destroy.mockReset()
  stripePaymentElement.on.mockReset().mockImplementation((event, callback) => {
    handlers[event] = callback
    if (event === 'ready') callback()
  })
  stripeInstance.elements.mockReset().mockReturnValue(stripeElements)
  stripeInstance.createPaymentMethod.mockReset().mockResolvedValue({ paymentMethod: { id: 'pm_42' } })
  stripeInstance.confirmPayment.mockReset().mockResolvedValue({ paymentIntent: { status: 'succeeded' } })
  stripeInstance.confirmAlipayPayment.mockReset().mockResolvedValue(alipayIntent())
  stripeInstance.confirmWechatPayPayment.mockReset().mockResolvedValue({
    paymentIntent: { status: 'requires_action', next_action: {
      wechat_pay_display_qr_code: { image_data_url: 'data:image/png;base64,WECHAT', data: 'weixin://pay/42' },
    } },
  })
  window.localStorage.clear()
})
afterEach(() => {
  for (const wrapper of wrappers.splice(0)) wrapper.unmount()
  vi.useRealTimers()
  vi.restoreAllMocks()
})

describe('StripePaymentView inline wallet checkout', () => {
  it('uses the order currency when the local recovery snapshot is absent', async () => {
    getOrder.mockResolvedValue({ data: orderFactory({ currency: 'HKD' }) })
    const wrapper = mountView()
    await flushPromises()
    expect(getOrder).toHaveBeenCalledWith(42)
    expect(loadStripe).toHaveBeenCalledWith('pk_test')
    expect(wrapper.text()).toContain(formatPaymentAmount(103, 'HKD', 'zh-CN'))
  })

  it('shows Alipay QR and order details below Pay Now without opening a window', async () => {
    const open = vi.spyOn(window, 'open').mockReturnValue(null)
    const wrapper = mountView()
    await flushPromises()
    expect(wrapper.find('section').exists()).toBe(false)
    await pay(wrapper)
    expect(stripeElements.submit).toHaveBeenCalledOnce()
    expect(stripeInstance.createPaymentMethod).toHaveBeenCalledWith({ elements: stripeElements })
    expect(stripeInstance.confirmAlipayPayment).toHaveBeenCalledWith('pi_secret_42', {
      payment_method: 'pm_42', return_url: 'http://localhost:3000/payment/result?order_id=42&status=success',
    }, { handleActions: false })
    expect(toDataURL).toHaveBeenCalledWith('https://hooks.stripe.com/alipay/authorize', expect.any(Object))
    const card = wrapper.get('button.btn-stripe').element.parentElement!
    expect(card.querySelector('section')).not.toBeNull()
    expect(card.querySelector('section')!.compareDocumentPosition(card.querySelector('button')!) & Node.DOCUMENT_POSITION_PRECEDING).toBeTruthy()
    expect(wrapper.get('section img').attributes('src')).toBe('data:image/png;base64,QR')
    expect(wrapper.get('section a').attributes('href')).toBe('https://hooks.stripe.com/alipay/authorize')
    expect(wrapper.get('section').text()).toContain('sub2_stripe_42')
    expect(wrapper.get('section').text()).toContain('#42')
    expect(wrapper.get('section').text()).toContain('30:00')
    expect(wrapper.get('button.btn-stripe').attributes('disabled')).toBeDefined()
    expect(stripePaymentElement.update).toHaveBeenCalledWith({ readOnly: true })
    expect(routerPush).not.toHaveBeenCalled()
    expect(open).not.toHaveBeenCalled()
  })

  it('handles the redirect_to_url Alipay response too', async () => {
    stripeInstance.confirmAlipayPayment.mockResolvedValue(alipayIntent({ redirect_to_url: { url: 'https://payments.example/alipay' } }))
    const wrapper = mountView()
    await flushPromises()
    await pay(wrapper)
    expect(wrapper.get('section a').attributes('href')).toBe('https://payments.example/alipay')
  })

  it('shows the WeChat QR in the same checkout card using manual actions', async () => {
    const wrapper = mountView()
    await flushPromises()
    await pay(wrapper, 'wechat_pay')
    expect(stripeInstance.confirmWechatPayPayment).toHaveBeenCalledWith('pi_secret_42', {
      payment_method: 'pm_42', payment_method_options: { wechat_pay: { client: 'web' } },
    }, { handleActions: false })
    expect(wrapper.get('section img').attributes('src')).toBe('data:image/png;base64,WECHAT')
    expect(wrapper.get('section').text()).toContain('payment.qr.scanWxpay')
    expect(wrapper.find('section a').exists()).toBe(false)
    expect(stripeInstance.confirmPayment).not.toHaveBeenCalled()
    expect(routerPush).not.toHaveBeenCalled()
  })

  it('renders WeChat QR data when Stripe does not provide an image', async () => {
    stripeInstance.confirmWechatPayPayment.mockResolvedValue({ paymentIntent: { status: 'requires_action', next_action: {
      wechat_pay_display_qr_code: { data: 'weixin://pay/42' },
    } } })
    const wrapper = mountView()
    await flushPromises()
    await pay(wrapper, 'wechat_pay')
    expect(toDataURL).toHaveBeenCalledWith('weixin://pay/42', expect.any(Object))
    expect(wrapper.find('section img').exists()).toBe(true)
  })

  it.each(['alipay', 'wechat_pay'])('also keeps the direct %s route on the page', async (method) => {
    routeState.query.method = method
    const wrapper = mountView()
    await flushPromises()
    expect(wrapper.find('section img').exists()).toBe(true)
    expect(stripeInstance.createPaymentMethod).not.toHaveBeenCalled()
    expect(routerPush).not.toHaveBeenCalled()
  })

  it('prevents duplicate confirmations while submitting and waiting for payment', async () => {
    let resolve!: (result: ReturnType<typeof alipayIntent>) => void
    stripeInstance.confirmAlipayPayment.mockReturnValue(new Promise((done) => { resolve = done }))
    const wrapper = mountView()
    await flushPromises()
    await pay(wrapper)
    await pay(wrapper)
    expect(stripeInstance.confirmAlipayPayment).toHaveBeenCalledOnce()
    resolve(alipayIntent())
    await flushPromises()
    await pay(wrapper)
    expect(stripeInstance.confirmAlipayPayment).toHaveBeenCalledOnce()
  })

  it('stops before confirmation on invalid fields and permits correction', async () => {
    stripeElements.submit.mockResolvedValue({ error: { message: 'Billing name required' } })
    const wrapper = mountView()
    await flushPromises()
    await pay(wrapper)
    expect(wrapper.text()).toContain('Billing name required')
    expect(stripeInstance.createPaymentMethod).not.toHaveBeenCalled()
    expect(stripeInstance.confirmAlipayPayment).not.toHaveBeenCalled()
    expect(wrapper.get('button.btn-stripe').attributes('disabled')).toBeUndefined()
  })

  it('shows tokenization and confirmation errors without displaying success', async () => {
    stripeInstance.createPaymentMethod.mockResolvedValue({ error: { message: 'Payment method unavailable' } })
    const wrapper = mountView()
    await flushPromises()
    await pay(wrapper)
    expect(wrapper.text()).toContain('Payment method unavailable')
    stripeInstance.createPaymentMethod.mockResolvedValue({ paymentMethod: { id: 'pm_42' } })
    stripeInstance.confirmAlipayPayment.mockResolvedValue({ error: { message: 'Authorization failed' } })
    await pay(wrapper)
    expect(wrapper.text()).toContain('Authorization failed')
    expect(wrapper.text()).not.toContain('payment.result.success')
  })

  it('does not mark a processing card payment as successful', async () => {
    stripeInstance.confirmPayment.mockResolvedValue({ paymentIntent: { status: 'processing' } })
    const wrapper = mountView()
    await flushPromises()
    await pay(wrapper, 'card')
    expect(stripeInstance.confirmPayment).toHaveBeenCalledWith(expect.objectContaining({
      confirmParams: expect.objectContaining({ payment_method: 'pm_42' }), redirect: 'if_required',
    }))
    expect(wrapper.text()).toContain('payment.result.processingHint')
    expect(wrapper.text()).not.toContain('payment.result.success')
    expect(routerPush).not.toHaveBeenCalled()
  })

  it('keeps successful card confirmation working', async () => {
    const wrapper = mountView()
    await flushPromises()
    await pay(wrapper, 'card')
    expect(wrapper.text()).toContain('payment.result.success')
    await vi.advanceTimersByTimeAsync(2000)
    expect(routerPush).toHaveBeenCalledWith({ path: '/payment/result', query: { order_id: '42', status: 'success' } })
  })

  it('updates to success after the backend confirms payment and stops polling', async () => {
    paymentStore.pollOrderStatus.mockResolvedValue(orderFactory({ status: 'PAID' }))
    const wrapper = mountView()
    await flushPromises()
    await pay(wrapper)
    await vi.advanceTimersByTimeAsync(3000)
    expect(wrapper.text()).toContain('payment.result.success')
    expect(wrapper.find('section img').exists()).toBe(false)
    await vi.advanceTimersByTimeAsync(9000)
    expect(paymentStore.pollOrderStatus).toHaveBeenCalledOnce()
    expect(routerPush).toHaveBeenCalledOnce()
  })

  it.each(['EXPIRED', 'CANCELLED', 'FAILED'] as const)('hides QR for a terminal %s order', async (status) => {
    paymentStore.pollOrderStatus.mockResolvedValue(orderFactory({ status }))
    const wrapper = mountView()
    await flushPromises()
    await pay(wrapper)
    await vi.advanceTimersByTimeAsync(3000)
    expect(wrapper.find('section img').exists()).toBe(false)
    expect(wrapper.find('section a').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('payment.result.success')
    expect(wrapper.get('button.btn-stripe').attributes('disabled')).toBeDefined()
    await vi.advanceTimersByTimeAsync(6000)
    expect(paymentStore.pollOrderStatus).toHaveBeenCalledOnce()
  })

  it('hides expired QR locally but still catches a payment made just before expiry', async () => {
    getOrder.mockResolvedValue({ data: orderFactory({ expires_at: '2026-10-10T12:00:02Z' }) })
    const wrapper = mountView()
    await flushPromises()
    await pay(wrapper)
    await vi.advanceTimersByTimeAsync(2000)
    expect(wrapper.text()).toContain('payment.qr.expired')
    expect(wrapper.find('section img').exists()).toBe(false)
    paymentStore.pollOrderStatus.mockResolvedValue(orderFactory({ status: 'PAID' }))
    await vi.advanceTimersByTimeAsync(1000)
    expect(wrapper.text()).toContain('payment.result.success')
  })

  it('does not confirm an order that expires while validating its payment fields', async () => {
    getOrder.mockResolvedValue({ data: orderFactory({ expires_at: '2026-10-10T12:00:02Z' }) })
    let resolve!: (result: object) => void
    stripeElements.submit.mockReturnValue(new Promise((done) => { resolve = done }))
    const wrapper = mountView()
    await flushPromises()
    await pay(wrapper)
    await vi.advanceTimersByTimeAsync(2000)
    resolve({})
    await flushPromises()
    expect(stripeInstance.createPaymentMethod).not.toHaveBeenCalled()
    expect(stripeInstance.confirmAlipayPayment).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('payment.qr.expired')
  })

  it('preserves the payment link if QR generation fails', async () => {
    toDataURL.mockRejectedValue(new Error('QR renderer unavailable'))
    const wrapper = mountView()
    await flushPromises()
    await pay(wrapper)
    expect(wrapper.find('section img').exists()).toBe(false)
    expect(wrapper.find('section a').exists()).toBe(true)
    expect(wrapper.text()).toContain('payment.stripeInline.qrUnavailable')
  })

  it('rejects unsafe authorization URLs', async () => {
    stripeInstance.confirmAlipayPayment.mockResolvedValue(alipayIntent({ alipay_handle_redirect: { url: 'javascript:alert(1)' } }))
    const wrapper = mountView()
    await flushPromises()
    await pay(wrapper)
    expect(toDataURL).not.toHaveBeenCalled()
    expect(wrapper.find('section a').exists()).toBe(false)
    expect(wrapper.text()).toContain('payment.result.failed')
  })

  it('does not overlap polls and ignores responses after leaving the checkout', async () => {
    let resolve!: (order: PaymentOrder) => void
    paymentStore.pollOrderStatus.mockReturnValue(new Promise((done) => { resolve = done }))
    const wrapper = mountView()
    await flushPromises()
    await pay(wrapper)
    await vi.advanceTimersByTimeAsync(9000)
    expect(paymentStore.pollOrderStatus).toHaveBeenCalledOnce()
    wrapper.unmount()
    wrappers.splice(wrappers.indexOf(wrapper), 1)
    resolve(orderFactory({ status: 'PAID' }))
    await flushPromises()
    await vi.advanceTimersByTimeAsync(9000)
    expect(routerPush).not.toHaveBeenCalled()
    expect(paymentStore.pollOrderStatus).toHaveBeenCalledOnce()
    expect(stripePaymentElement.destroy).toHaveBeenCalledOnce()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('continues polling after a transient network failure', async () => {
    paymentStore.pollOrderStatus.mockRejectedValueOnce(new Error('offline')).mockResolvedValue(orderFactory({ status: 'COMPLETED' }))
    const wrapper = mountView()
    await flushPromises()
    await pay(wrapper)
    await vi.advanceTimersByTimeAsync(3000)
    expect(wrapper.find('section img').exists()).toBe(true)
    await vi.advanceTimersByTimeAsync(3000)
    expect(wrapper.text()).toContain('payment.result.success')
  })
})

describe('StripePaymentInline', () => {
  it.each(['alipay', 'wechat_pay'])('uses the same inline panel for %s without emitting a popup redirect', async (method) => {
    const open = vi.spyOn(window, 'open').mockReturnValue(null)
    const wrapper = mount(StripePaymentInline, {
      props: { orderId: 42, amount: 100, payAmount: 103, currency: 'CNY', clientSecret: 'pi_secret_42', publishableKey: 'pk_test' },
      global: { stubs: { Icon: true } },
    })
    wrappers.push(wrapper)
    await flushPromises()
    await pay(wrapper, method)
    expect(wrapper.find('section img').exists()).toBe(true)
    expect(wrapper.emitted('redirect')).toBeUndefined()
    expect(open).not.toHaveBeenCalled()
    paymentStore.pollOrderStatus.mockResolvedValue(orderFactory({ status: 'PAID' }))
    await vi.advanceTimersByTimeAsync(3000)
    expect(wrapper.emitted('success')).toHaveLength(1)
  })
})
