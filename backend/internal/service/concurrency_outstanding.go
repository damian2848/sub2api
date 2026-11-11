package service

import (
	"context"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

type outstandingKind uint8

const (
	outstandingAccount outstandingKind = iota + 1
	outstandingUser
)

type outstandingSlotKey struct {
	kind      outstandingKind
	id        int64
	requestID string
}

type outstandingWaitKey struct {
	kind outstandingKind
	id   int64
}

// outstandingLedger remembers the account/user slot members and wait-count
// increments this process still owns, so shutdown can release whatever the
// handlers could not release themselves (hijacked WebSocket handlers survive
// Server.Close; other handlers may ignore cancellation past the post-close
// wait). Each entry is released exactly once: either by its owner or by the
// shutdown flush, never both. The zero value is ready to use.
type outstandingLedger struct {
	mu      sync.Mutex
	slots   map[outstandingSlotKey]struct{}
	waits   map[outstandingWaitKey]int
	flushed bool
}

func (l *outstandingLedger) addSlot(key outstandingSlotKey) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.slots == nil {
		l.slots = make(map[outstandingSlotKey]struct{})
	}
	l.slots[key] = struct{}{}
}

// takeSlot reports whether the caller still owns the member and must delete it.
func (l *outstandingLedger) takeSlot(key outstandingSlotKey) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.slots[key]; !ok {
		return false
	}
	delete(l.slots, key)
	return true
}

func (l *outstandingLedger) addWait(key outstandingWaitKey) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.waits == nil {
		l.waits = make(map[outstandingWaitKey]int)
	}
	l.waits[key]++
}

// takeWait reports whether a decrement must still be sent. An increment
// already flushed at shutdown must not be decremented a second time; an
// unpaired decrement before shutdown keeps the legacy behavior (sent; the
// counter script clamps at zero).
func (l *outstandingLedger) takeWait(key outstandingWaitKey) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n := l.waits[key]; n > 0 {
		if n == 1 {
			delete(l.waits, key)
		} else {
			l.waits[key] = n - 1
		}
		return true
	}
	return !l.flushed
}

func (l *outstandingLedger) drain() ([]outstandingSlotKey, map[outstandingWaitKey]int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flushed = true
	slots := make([]outstandingSlotKey, 0, len(l.slots))
	for key := range l.slots {
		slots = append(slots, key)
	}
	waits := l.waits
	l.slots = nil
	l.waits = nil
	return slots, waits
}

// ReleaseOutstanding releases, by exact member, every account/user slot and
// wait-count increment this process still holds. It is meant for shutdown,
// after in-flight handlers had their chance to release on their own; later
// calls to the owners' release/decrement functions become no-ops so nothing is
// released twice. Only this process's own entries are touched, so it is safe
// with multiple replicas sharing Redis.
func (s *ConcurrencyService) ReleaseOutstanding(ctx context.Context) {
	if s == nil {
		return
	}
	slots, waits := s.outstanding.drain()
	if s.cache == nil || (len(slots) == 0 && len(waits) == 0) {
		return
	}
	var releasedSlots, releasedWaits, failed int
	for _, key := range slots {
		if ctx.Err() != nil {
			failed++
			continue
		}
		var err error
		switch key.kind {
		case outstandingAccount:
			err = s.cache.ReleaseAccountSlot(ctx, key.id, key.requestID)
		case outstandingUser:
			err = s.cache.ReleaseUserSlot(ctx, key.id, key.requestID)
		}
		if err != nil {
			failed++
			continue
		}
		releasedSlots++
	}
	for key, n := range waits {
		for i := 0; i < n; i++ {
			if ctx.Err() != nil {
				failed++
				continue
			}
			var err error
			switch key.kind {
			case outstandingAccount:
				err = s.cache.DecrementAccountWaitCount(ctx, key.id)
			case outstandingUser:
				err = s.cache.DecrementWaitCount(ctx, key.id)
			}
			if err != nil {
				failed++
				continue
			}
			releasedWaits++
		}
	}
	logger.LegacyPrintf("service.concurrency", "Shutdown released outstanding concurrency state: slots=%d waits=%d failed=%d", releasedSlots, releasedWaits, failed)
}
