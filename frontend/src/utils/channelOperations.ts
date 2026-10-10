import type { SearchNavItem } from '@/utils/featureSearch'

export const CHANNEL_OPERATIONS_PATH = '/admin/channel-ops'

export const channelOperationsTabs = [
  { id: 'bps', path: `${CHANNEL_OPERATIONS_PATH}/bps`, label: 'channelOperations.tabs.bps', icon: 'bolt' },
  { id: 'harvest', path: `${CHANNEL_OPERATIONS_PATH}/harvest`, label: 'channelOperations.tabs.harvest', icon: 'swap' },
  { id: 'prism', path: `${CHANNEL_OPERATIONS_PATH}/prism`, label: 'channelOperations.tabs.prism', icon: 'sparkles' },
  { id: 'smart', path: `${CHANNEL_OPERATIONS_PATH}/smart/quality`, label: 'channelOperations.tabs.smart', icon: 'cpu' },
  { id: 'credentials', path: `${CHANNEL_OPERATIONS_PATH}/credentials`, label: 'channelOperations.tabs.credentials', icon: 'credentialOps' },
  { id: 'pelican', path: `${CHANNEL_OPERATIONS_PATH}/pelican`, label: 'channelOperations.tabs.pelican', icon: 'beaker' },
] as const

export type ChannelOperationsTab = typeof channelOperationsTabs[number]['id']

export const channelOperationsSections = {
  smart: [
    { path: `${CHANNEL_OPERATIONS_PATH}/smart/quality`, label: 'qualityOps.title' },
    { path: `${CHANNEL_OPERATIONS_PATH}/smart/priority`, label: 'priorityScheduling.title' },
    { path: `${CHANNEL_OPERATIONS_PATH}/smart/experiments`, label: 'controlledExperiments.title' },
    { path: `${CHANNEL_OPERATIONS_PATH}/smart/alerts`, label: 'accountOps.title' },
  ],
  credentials: [
    { path: `${CHANNEL_OPERATIONS_PATH}/credentials`, label: 'tokenGuardV2.title' },
    { path: `${CHANNEL_OPERATIONS_PATH}/credentials/legacy`, label: 'tokenGuard.title' },
  ],
} as const

export function channelOperationsTabForPath(path: string): ChannelOperationsTab | undefined {
  if (!path.startsWith(`${CHANNEL_OPERATIONS_PATH}/`)) return undefined
  const section = path.slice(CHANNEL_OPERATIONS_PATH.length + 1).split('/')[0]
  return channelOperationsTabs.find(tab => tab.id === section)?.id
}

// Search can expose all sections without turning the single sidebar entry back
// into a long expanded menu. Only augment an entry the existing permissions show.
export function withChannelOperationsSearch(items: SearchNavItem[], t: (key: string) => string): SearchNavItem[] {
  return items.map(item => {
    if (item.path === CHANNEL_OPERATIONS_PATH && !item.expandOnly) {
      return {
        ...item,
        children: channelOperationsTabs.map(tab => ({
          path: tab.path,
          label: t(tab.label),
          keywords: t(`channelOperations.search.${tab.id}`),
          children: tab.id === 'smart' || tab.id === 'credentials'
            ? channelOperationsSections[tab.id].map(section => ({ ...section, label: t(section.label) }))
            : undefined,
        })),
      }
    }
    return item.children ? { ...item, children: withChannelOperationsSearch(item.children, t) } : item
  })
}
