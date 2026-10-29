import { beforeAll, describe, expect, it } from 'vitest'
import { i18n } from '@/i18n'
import en from '@/i18n/locales/en/common'
import zh from '@/i18n/locales/zh/common'
import { compileMessages } from '@/__tests__/compileMessages'
import { formatCountdown, formatCountdownWithSuffix } from '../format'

const now = Date.parse('2026-10-20T00:00:00Z')

beforeAll(() => {
  i18n.global.setLocaleMessage('en', compileMessages(en))
  i18n.global.setLocaleMessage('zh', compileMessages(zh))
})

describe('formatCountdown', () => {
  it.each([
    [1, '1s'],
    [999, '1s'],
    [1_000, '1s'],
    [59_000, '59s'],
    [59_999, '60s'],
    [60_000, '1m'],
    [3_660_000, '1h 1m'],
    [90_000_000, '1d 1h'],
  ])('formats %i ms remaining without claiming an active cooldown is zero', (remaining, expected) => {
    i18n.global.locale.value = 'en'
    expect(formatCountdown(new Date(now + remaining), now)).toBe(expected)
  })

  it('uses the supplied clock consistently for the suffix and Chinese seconds', () => {
    i18n.global.locale.value = 'zh'
    expect(formatCountdown(new Date(now + 59_000), now)).toBe('59秒')
    expect(formatCountdownWithSuffix(new Date(now + 1_000), now)).toBe('1秒 后解除')
  })

  it.each([null, undefined, 'invalid', new Date(now), new Date(now - 1)])(
    'does not display an expired or invalid deadline: %s',
    deadline => {
      expect(formatCountdown(deadline, now)).toBeNull()
      expect(formatCountdownWithSuffix(deadline, now)).toBeNull()
    }
  )
})
