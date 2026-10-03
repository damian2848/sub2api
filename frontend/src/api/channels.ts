/**
 * User Channels API endpoints (non-admin)
 * 用户侧「可用渠道」聚合查询：用户可访问的分组 + 所挂渠道 + 分组可调用模型（含定价）。
 */

import { apiClient } from './client'
import type { BillingMode } from '@/constants/channel'

export interface UserAvailableGroup {
  id: number
  name: string
  platform: string
  /** 'standard' | 'subscription' — 订阅分组视觉加深，和 API 密钥页保持一致。 */
  subscription_type: string
  /** 分组默认倍率。用户专属倍率（若有）通过 /groups/rates 获取后在前端 join。 */
  rate_multiplier: number
  peak_rate_enabled: boolean
  peak_start: string
  peak_end: string
  peak_rate_multiplier: number
  /** true = 专属分组（小范围授权）；false = 公开分组。 */
  is_exclusive: boolean
}

export interface UserPricingInterval {
  min_tokens: number
  max_tokens: number | null
  tier_label?: string
  input_price: number | null
  output_price: number | null
  cache_write_price: number | null
  cache_write_1h_price?: number | null
  cache_read_price: number | null
  input_multiplier?: number | null
  output_multiplier?: number | null
  cache_write_multiplier?: number | null
  cache_read_multiplier?: number | null
  per_request_price: number | null
}

export interface UserSupportedModelPricing {
  billing_mode: BillingMode
  input_price: number | null
  output_price: number | null
  cache_write_price: number | null
  cache_write_1h_price?: number | null
  cache_read_price: number | null
  reasoning_effort_multipliers?: Record<string, number> | null
  image_input_price: number | null
  image_output_price: number | null
  per_request_price: number | null
  intervals: UserPricingInterval[]
}

export interface UserSupportedModel {
  name: string
  platform: string
  pricing: UserSupportedModelPricing | null
}

/** 分组所挂渠道的简要信息。 */
export interface UserGroupChannelRef {
  name: string
  description: string
}

/**
 * 分组视角的可用渠道条目：同平台的多个分组各自独立一条，
 * models 只含该分组真实可调用的模型（账号映射 ∩ 渠道定价，再叠加分组白名单与用户禁用）。
 */
export interface UserAvailableGroupView {
  group: UserAvailableGroup
  channels: UserGroupChannelRef[]
  models: UserSupportedModel[]
}

/** 列出当前用户可见的「可用渠道」（以分组为顶层，同平台分组不合并）。 */
export async function getAvailable(options?: { signal?: AbortSignal }): Promise<UserAvailableGroupView[]> {
  const { data } = await apiClient.get<UserAvailableGroupView[]>('/channels/available', {
    signal: options?.signal
  })
  return data
}

export const userChannelsAPI = { getAvailable }

export default userChannelsAPI
