import type { PelicanShowcaseGroup, PelicanShowcaseItem } from '@/api/pelicanShowcase'

export interface PelicanModelHistory {
  /** Stable identity across result changes. */
  key: string
  modelId: string
  items: PelicanShowcaseItem[]
}

function resultTime(item: PelicanShowcaseItem): number {
  const time = Date.parse(item.generated_at)
  return Number.isFinite(time) ? time : 0
}

/**
 * Collapse a group's flat result list into one history per model.
 * The API normally returns newest-first, but sorting here keeps the UI's
 * "latest result" invariant even when records arrive from mixed sources.
 */
export function groupPelicanItemsByModel(group: PelicanShowcaseGroup): PelicanModelHistory[] {
  const histories = new Map<string, PelicanModelHistory>()

  for (const item of group.items) {
    const modelId = item.model_id
    const key = JSON.stringify([group.id, modelId])
    let history = histories.get(key)
    if (!history) {
      history = { key, modelId, items: [] }
      histories.set(key, history)
    }
    history.items.push(item)
  }

  return Array.from(histories.values()).map((history) => ({
    ...history,
    items: history.items.slice().sort((a, b) => resultTime(b) - resultTime(a) || b.id - a.id),
  }))
}
