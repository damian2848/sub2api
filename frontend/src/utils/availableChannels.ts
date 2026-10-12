import type { UserAvailableChannel, UserAvailableGroup, UserSupportedModel } from '@/api/channels'

export interface AvailableModelGroup {
  key: string
  channelName: string
  group: UserAvailableGroup
  models: UserSupportedModel[]
}

/** Group-specific lists are authoritative; never fall back to the pricing catalog. */
export function availableChannelPlatforms(channels: UserAvailableChannel[], query = '') {
  const search = query.trim().toLowerCase()
  const platforms = new Map<string, AvailableModelGroup[]>()
  channels.forEach((channel, index) => {
    const channelMatches = channel.name.toLowerCase().includes(search) || channel.description.toLowerCase().includes(search)
    for (const section of channel.platforms) {
      for (const group of section.groups) {
        const showAll = !search || channelMatches || section.platform.toLowerCase().includes(search) || group.name.toLowerCase().includes(search)
        const models = (group.supported_models ?? []).filter(model =>
          model.platform === section.platform && model.name.trim() && !model.name.includes('*') &&
          (showAll || model.name.toLowerCase().includes(search)),
        )
        if (!models.length) continue
        const groups = platforms.get(section.platform) ?? []
        groups.push({ key: `${index}-${group.id}`, channelName: channel.name, group, models })
        platforms.set(section.platform, groups)
      }
    }
  })
  return [...platforms].sort(([a], [b]) => a.localeCompare(b)).map(([platform, groups]) => ({ platform, groups }))
}
