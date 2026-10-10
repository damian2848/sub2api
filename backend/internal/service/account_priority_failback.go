package service

import "time"

const priorityFailbackTimeout = 150 * time.Millisecond

// A warm session moves only to known spare capacity. Check both the scheduling
// load factor and the real concurrency limit so a large load factor cannot
// hide saturation. Unknown load and queued accounts keep the current binding.
func priorityFailbackHasHeadroom(account *Account, load *AccountLoadInfo) bool {
	return account != nil && load != nil && account.Concurrency > 0 &&
		load.WaitingCount == 0 && load.LoadRate < 80 &&
		float64(load.CurrentConcurrency) < float64(account.Concurrency)*0.8
}
