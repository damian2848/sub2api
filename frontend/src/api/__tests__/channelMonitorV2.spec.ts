import { afterEach, describe, expect, it, vi } from 'vitest'
import { apiClient } from '../client'
import { getMatrix, getObservations, repeatedArrayParamsSerializer } from '../channelMonitorV2'

afterEach(() => vi.restoreAllMocks())

describe('channel monitor V2 query serialization', () => {
  it('uses repeated keys without bracket suffixes for array filters', () => {
    const query = repeatedArrayParamsSerializer({
      range: '90m',
      platform: ['openai', 'grok'],
      group_id: [1, 2],
      model: undefined,
      group_by: 'platform_group_model',
    })

    expect(query).toBe('range=90m&platform=openai&platform=grok&group_id=1&group_id=2&group_by=platform_group_model')
    expect(query).not.toContain('%5B%5D')
  })

  it('sends the matrix grouping with the shared filters', async () => {
    const get = vi.spyOn(apiClient, 'get').mockResolvedValue({
      data: { coverage: {}, group_by: 'platform_group', items: [] },
    })

    await getMatrix({ range: '24h', platforms: ['openai'], groupIds: [7], models: [] }, 'platform_group', true)

    expect(get).toHaveBeenCalledWith('/admin/channel-monitor-v2/matrix', expect.objectContaining({
      params: {
        range: '24h',
        platform: ['openai'],
        group_id: [7],
        model: undefined,
        group_by: 'platform_group',
      },
    }))
  })
  it('reads observations with the same dimension filters and never starts a probe', async () => {
    const get = vi.spyOn(apiClient, 'get').mockResolvedValue({ data: { items: [] } })
    const post = vi.spyOn(apiClient, 'post')
    await getObservations({ range: '7d', platforms: ['openai'], groupIds: [7, 9], models: ['gpt-test'] }, false, undefined, 'platform_group_model')
    expect(get).toHaveBeenCalledWith('/channel-monitor-v2/observations', expect.objectContaining({ params: {
      range: '7d', platform: ['openai'], group_id: [7, 9], model: ['gpt-test'], group_by: 'platform_group_model',
    } }))
    expect(post).not.toHaveBeenCalled()
  })
})
