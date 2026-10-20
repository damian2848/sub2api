import { createSharedComposable, useTimestamp } from '@vueuse/core'

// All visible account rows share one clock; VueUse stops it when the last
// consumer unmounts. Time changes do not require replacing account objects.
export const useSharedNow = createSharedComposable(() => useTimestamp({ interval: 1000 }))
