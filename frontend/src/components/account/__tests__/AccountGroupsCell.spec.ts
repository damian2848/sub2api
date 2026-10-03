import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import AccountGroupsCell from '../AccountGroupsCell.vue'
import type { Group } from '@/types'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key}:${Object.values(params).join(',')}` : key,
    }),
  }
})

function makeGroups(n: number): Group[] {
  return Array.from({ length: n }, (_, i) => ({
    id: i + 1,
    name: `very-long-group-name-${i + 1}`,
    platform: 'openai',
    subscription_type: 'standard',
    rate_multiplier: 1,
  })) as unknown as Group[]
}

function mountCell(groups: Group[] | null, props = {}) {
  return mount(AccountGroupsCell, {
    props: { groups, ...props },
    global: {
      stubs: {
        GroupBadge: {
          props: ['name'],
          template: '<span data-group-badge>{{ name }}</span>',
        },
      },
    },
  })
}

describe('AccountGroupsCell', () => {
  it('renders a dash when there are no groups', () => {
    expect(mountCell([]).text()).toBe('-')
    expect(mountCell(null).text()).toBe('-')
  })

  it('shows every group when the count is within the default limit, with no +N button', () => {
    const wrapper = mountCell(makeGroups(8))

    expect(wrapper.findAll('[data-group-badge]')).toHaveLength(8)
    expect(wrapper.find('button').exists()).toBe(false)
  })

  it('does not clip or truncate the badge container', () => {
    const wrapper = mountCell(makeGroups(3))
    const badge = wrapper.get('[data-group-badge]')
    const container = badge.element.parentElement as HTMLElement

    // 旧实现用 max-h-14 + overflow-hidden 把第 3 行起的分组直接裁掉，用 max-w-24 截断名称。
    const containerClasses = Array.from(container.classList)
    expect(containerClasses).toContain('flex-wrap')
    expect(containerClasses).not.toContain('overflow-hidden')
    expect(containerClasses.some((c) => c.startsWith('max-h-'))).toBe(false)
    expect(badge.classes()).not.toContain('max-w-24')
  })

  it('folds the overflow into a +N button once the limit is exceeded', () => {
    const wrapper = mountCell(makeGroups(12))

    // 默认上限 8：展示 7 个 + 「+5」按钮。
    expect(wrapper.findAll('[data-group-badge]')).toHaveLength(7)
    expect(wrapper.get('button').text()).toBe('+5')
  })

  it('honours an explicit maxDisplay', () => {
    const wrapper = mountCell(makeGroups(5), { maxDisplay: 3 })

    expect(wrapper.findAll('[data-group-badge]')).toHaveLength(2)
    expect(wrapper.get('button').text()).toBe('+3')
  })
})
