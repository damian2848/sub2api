import { describe, expect, it } from 'vitest'
import { defineComponent } from 'vue'
import { createMemoryHistory, createRouter, type RouteRecordRaw } from 'vue-router'
import { channelOperationsRoutes } from '@/router/channelOperations'
import { CHANNEL_OPERATIONS_PATH } from '@/utils/channelOperations'

const Page = defineComponent({ template: '<div />' })

// Exercise the production route records without mounting business pages or
// making any API requests. Lazy importers remain on the production definitions.
function router() {
  const routes = channelOperationsRoutes.map(record => ({
    ...record,
    component: record.redirect ? undefined : Page,
    children: record.children?.map(child => ({ ...child, component: child.redirect ? undefined : Page })),
  })) as RouteRecordRaw[]
  return createRouter({ history: createMemoryHistory(), routes })
}

const legacyRoutes = [
  ['/admin/auto-config', 'AdminAutoConfig', 'bps'],
  ['/admin/harvest-flow', 'AdminHarvestFlow', 'harvest'],
  ['/admin/account-quality', 'AdminAccountQuality', 'smart/quality'],
  ['/admin/priority-scheduling', 'AdminPriorityScheduling', 'smart/priority'],
  ['/admin/account-ops', 'AdminAccountOps', 'smart/alerts'],
  ['/admin/controlled-experiments', 'AdminControlledExperiments', 'smart/experiments'],
  ['/admin/token-guard-v2', 'AdminTokenGuardV2', 'credentials'],
  ['/admin/token-guard', 'AdminTokenGuard', 'credentials/legacy'],
  ['/admin/pelican-tests', 'AdminPelicanTests', 'pelican'],
  ['/admin/smart-ops', 'AdminSmartOps', 'smart/quality'],
]

describe('channel operations routes', () => {
  it('defaults to BPS without dropping incoming query parameters or an anchor', async () => {
    const instance = router()
    await instance.push(`${CHANNEL_OPERATIONS_PATH}?source=bookmark#details`)
    expect(instance.currentRoute.value.fullPath).toBe(`${CHANNEL_OPERATIONS_PATH}/bps?source=bookmark#details`)
  })

  it('also defaults named hub navigation to BPS', async () => {
    const instance = router()
    await instance.push({ name: 'AdminChannelOperations', query: { source: 'named' }, hash: '#details' })
    expect(instance.currentRoute.value.fullPath).toBe(`${CHANNEL_OPERATIONS_PATH}/bps?source=named#details`)
  })

  it.each(legacyRoutes)('preserves %s and its named link, query and hash', async (path, name, section) => {
    const instance = router()
    expect(instance.resolve({ name }).path).toBe(path)
    await instance.push({ name, query: { account_id: '42', source: ['one', 'two'] }, hash: '#details' })
    expect(instance.currentRoute.value.path).toBe(`${CHANNEL_OPERATIONS_PATH}/${section}`)
    expect(instance.currentRoute.value.query).toEqual({ account_id: '42', source: ['one', 'two'] })
    expect(instance.currentRoute.value.hash).toBe('#details')
    expect(instance.currentRoute.value.meta.requiresAdmin).toBe(true)
  })

  it('inherits admin-only access for every module, including direct deep links', () => {
    const instance = router()
    for (const section of ['bps', 'harvest', 'prism', 'smart/quality', 'smart/priority', 'smart/experiments', 'smart/alerts', 'credentials', 'credentials/legacy', 'pelican']) {
      const resolved = instance.resolve(`${CHANNEL_OPERATIONS_PATH}/${section}`)
      expect(resolved.matched).toHaveLength(2)
      expect(resolved.meta.requiresAuth).toBe(true)
      expect(resolved.meta.requiresAdmin).toBe(true)
    }
  })

  it('embeds all reused business pages and keeps their components lazily loaded', () => {
    const hub = channelOperationsRoutes[0]
    const sections = hub.children?.filter(child => child.component && !['bps', 'prism'].includes(child.path)) ?? []
    expect(sections).toHaveLength(8)
    for (const section of sections) {
      expect(section.props).toEqual({ embedded: true })
      expect(typeof section.component).toBe('function')
    }
  })
})
