import { describe, expect, it } from 'vitest'
import { buildFeatureSearchEntries, searchFeatures } from '@/utils/featureSearch'
import { CHANNEL_OPERATIONS_PATH, channelOperationsTabForPath, channelOperationsTabs, withChannelOperationsSearch } from '@/utils/channelOperations'
import zh from '@/i18n/locales/zh/channelOperations'
import en from '@/i18n/locales/en/channelOperations'

function t(key: string) {
  if (!key.startsWith('channelOperations.')) return key
  return key.split('.').slice(1).reduce<unknown>((value, segment) => (value as Record<string, unknown>)[segment], zh) as string
}

describe('channel operations navigation', () => {
  it('has six distinct modules with shared smart/credentials section selection', () => {
    expect(channelOperationsTabs).toHaveLength(6)
    expect(new Set(channelOperationsTabs.map(tab => tab.path)).size).toBe(6)
    expect(channelOperationsTabForPath(`${CHANNEL_OPERATIONS_PATH}/smart/quality`)).toBe('smart')
    expect(channelOperationsTabForPath(`${CHANNEL_OPERATIONS_PATH}/smart/alerts`)).toBe('smart')
    expect(channelOperationsTabForPath(`${CHANNEL_OPERATIONS_PATH}/credentials/legacy`)).toBe('credentials')
    expect(channelOperationsTabForPath('/admin/accounts')).toBeUndefined()
    expect(channelOperationsTabForPath('/admin/channel-ops-unrelated/bps')).toBeUndefined()
  })

  it('indexes all former operations pages while leaving the actual sidebar flat', () => {
    const sidebar = [{ path: CHANNEL_OPERATIONS_PATH, label: t('channelOperations.title') }]
    const entries = buildFeatureSearchEntries(withChannelOperationsSearch(sidebar, t))
    expect(sidebar[0]).not.toHaveProperty('children')
    expect(entries.map(entry => entry.path)).toEqual(expect.arrayContaining([
      `${CHANNEL_OPERATIONS_PATH}/bps`, `${CHANNEL_OPERATIONS_PATH}/harvest`, `${CHANNEL_OPERATIONS_PATH}/prism`,
      `${CHANNEL_OPERATIONS_PATH}/smart/quality`, `${CHANNEL_OPERATIONS_PATH}/smart/priority`, `${CHANNEL_OPERATIONS_PATH}/smart/experiments`, `${CHANNEL_OPERATIONS_PATH}/smart/alerts`,
      `${CHANNEL_OPERATIONS_PATH}/credentials`, `${CHANNEL_OPERATIONS_PATH}/credentials/legacy`, `${CHANNEL_OPERATIONS_PATH}/pelican`,
    ]))
    expect(new Set(entries.map(entry => entry.path)).size).toBe(entries.length)
    for (const term of ['BPS', 'Prism', '打票', '2FA', '调度', '鹈鹕']) {
      expect(searchFeatures(entries, term).length, term).toBeGreaterThan(0)
    }
  })

  it('does not surface admin modules when the sidebar has no permitted hub entry', () => {
    const items = [{ path: '/admin/accounts', label: 'Accounts' }]
    expect(withChannelOperationsSearch(items, t)).toEqual(items)
    expect(withChannelOperationsSearch([], t)).toEqual([])
    const expandOnly = [{ path: CHANNEL_OPERATIONS_PATH, label: 'Unavailable', expandOnly: true }]
    expect(withChannelOperationsSearch(expandOnly, t)).toEqual(expandOnly)
  })

  it('keeps Chinese and English module labels and search metadata in sync', () => {
    expect(Object.keys(en)).toEqual(Object.keys(zh))
    expect(Object.keys(en.tabs)).toEqual(Object.keys(zh.tabs))
    expect(Object.keys(en.search)).toEqual(Object.keys(zh.search))
  })
})
