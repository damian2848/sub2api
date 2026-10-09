import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import { useOnboardingTour } from '../useOnboardingTour'

const mocks = vi.hoisted(() => ({
  driver: vi.fn(),
  drive: vi.fn(),
  destroy: vi.fn(),
  setDriverInstance: vi.fn(),
}))

vi.mock('driver.js', () => ({ driver: mocks.driver }))
vi.mock('@/components/Guide/steps', () => ({
  getAdminSteps: () => [],
  getUserSteps: () => [],
}))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ user: { id: 1, role: 'user' }, isSimpleMode: false }),
}))
vi.mock('@/stores/onboarding', () => ({
  useOnboardingStore: () => ({
    getDriverInstance: () => null,
    setDriverInstance: mocks.setDriverInstance,
    isDriverActive: () => false,
    setControlMethods: vi.fn(),
    clearControlMethods: vi.fn(),
  }),
}))

function mountTour() {
  let tour!: ReturnType<typeof useOnboardingTour>
  const wrapper = mount(defineComponent({
    setup() {
      tour = useOnboardingTour({ autoStart: false })
      return () => null
    },
  }))
  return { wrapper, tour }
}

describe('on-demand onboarding', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mocks.driver.mockReturnValue({ drive: mocks.drive, destroy: mocks.destroy })
  })

  it('starts the requested step after the optional tour modules load', async () => {
    const { wrapper, tour } = mountTour()
    expect(mocks.driver).not.toHaveBeenCalled()

    await tour.startTour(2)

    expect(mocks.driver).toHaveBeenCalledOnce()
    expect(mocks.drive).toHaveBeenCalledWith(2)
    expect(mocks.setDriverInstance).toHaveBeenCalled()
    wrapper.unmount()
  })

  it('does not start a delayed tour after navigating away', async () => {
    const { wrapper, tour } = mountTour()
    const loading = tour.startTour()
    wrapper.unmount()

    await loading

    expect(mocks.driver).not.toHaveBeenCalled()
    expect(mocks.drive).not.toHaveBeenCalled()
  })
})
