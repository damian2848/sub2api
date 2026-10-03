import type { RouteLocation, RouteRecordRaw } from 'vue-router'
import { CHANNEL_OPERATIONS_PATH } from '@/utils/channelOperations'

export function channelOperationsRedirect(path: string) {
  return (to: Pick<RouteLocation, 'query' | 'hash'>) => ({ path, query: to.query, hash: to.hash })
}

const adminMeta = { requiresAuth: true, requiresAdmin: true }

export const channelOperationsRoutes: RouteRecordRaw[] = [
  {
    path: CHANNEL_OPERATIONS_PATH,
    component: () => import('@/views/admin/ChannelOperationsView.vue'),
    meta: { ...adminMeta, titleKey: 'channelOperations.title', descriptionKey: 'channelOperations.description' },
    children: [
      { path: '', name: 'AdminChannelOperations', redirect: channelOperationsRedirect(`${CHANNEL_OPERATIONS_PATH}/bps`) },
      { path: 'bps', name: 'ChannelOperationsBPS', component: () => import('@/views/admin/ChannelBPSView.vue'), meta: { titleKey: 'channelOperations.tabs.bps' } },
      { path: 'harvest', name: 'ChannelOperationsHarvest', component: () => import('@/views/admin/HarvestFlowView.vue'), props: { embedded: true }, meta: { titleKey: 'admin.harvestFlow.title' } },
      { path: 'prism', name: 'ChannelOperationsPrism', component: () => import('@/views/admin/ChannelPrismView.vue'), meta: { titleKey: 'channelOperations.tabs.prism' } },
      { path: 'smart', redirect: channelOperationsRedirect(`${CHANNEL_OPERATIONS_PATH}/smart/quality`) },
      { path: 'smart/quality', name: 'ChannelOperationsQuality', component: () => import('@/views/admin/AccountQualityView.vue'), props: { embedded: true }, meta: { titleKey: 'qualityOps.title' } },
      { path: 'smart/priority', name: 'ChannelOperationsPriority', component: () => import('@/views/admin/PrioritySchedulingView.vue'), props: { embedded: true }, meta: { titleKey: 'priorityScheduling.title' } },
      { path: 'smart/alerts', name: 'ChannelOperationsAlerts', component: () => import('@/views/admin/AccountOpsView.vue'), props: { embedded: true }, meta: { titleKey: 'accountOps.title' } },
      { path: 'credentials', name: 'ChannelOperationsCredentials', component: () => import('@/views/admin/ops/TokenGuardV2View.vue'), props: { embedded: true }, meta: { titleKey: 'tokenGuardV2.title' } },
      { path: 'credentials/legacy', name: 'ChannelOperationsCredentialGuard', component: () => import('@/views/admin/ops/TokenGuardView.vue'), props: { embedded: true }, meta: { titleKey: 'tokenGuard.title' } },
      { path: 'pelican', name: 'ChannelOperationsPelican', component: () => import('@/views/admin/PelicanTestsView.vue'), props: { embedded: true }, meta: { titleKey: 'pelicanTests.title' } },
    ],
  },
  ...([
    ['/admin/auto-config', 'AdminAutoConfig', 'bps'],
    ['/admin/harvest-flow', 'AdminHarvestFlow', 'harvest'],
    ['/admin/account-quality', 'AdminAccountQuality', 'smart/quality'],
    ['/admin/priority-scheduling', 'AdminPriorityScheduling', 'smart/priority'],
    ['/admin/account-ops', 'AdminAccountOps', 'smart/alerts'],
    ['/admin/token-guard-v2', 'AdminTokenGuardV2', 'credentials'],
    ['/admin/token-guard', 'AdminTokenGuard', 'credentials/legacy'],
    ['/admin/pelican-tests', 'AdminPelicanTests', 'pelican'],
    ['/admin/smart-ops', 'AdminSmartOps', 'smart/quality'],
  ] as const).map(([path, name, section]): RouteRecordRaw => ({
    path, name, meta: adminMeta,
    redirect: channelOperationsRedirect(`${CHANNEL_OPERATIONS_PATH}/${section}`),
  })),
]
