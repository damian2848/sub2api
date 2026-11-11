package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

const (
	// slotAdmissionTimeout bounds one account/user admission script on its own
	// detached context, so a request deadline or client cancellation cannot cut
	// the socket read after Redis already executed the ZADD.
	slotAdmissionTimeout = 2 * time.Second
	// slotCompensationBudget bounds the exact-member cleanup of an ambiguous
	// admission, including retries and backoff.
	slotCompensationBudget = 2 * time.Second
)

type slotAdmissionResult struct {
	acquired bool
	err      error
}

// admitSlotDetached runs one slot admission write without letting the caller's
// cancellation or deadline interrupt it mid-flight. The caller still returns as
// soon as its context ends (preserving the old cancellation latency); the write
// then finishes in the background and an admitted or ambiguous member is
// removed by its exact ID instead of lingering until the slot TTL.
//
// Any admission error is treated as ambiguous: the script may have executed even
// though its reply was lost, and ZREM of a unique request ID is idempotent.
func admitSlotDetached(
	ctx context.Context,
	acquire func(context.Context) (bool, error),
	release func(context.Context) error,
	describe func() string,
) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		// Nothing has been sent yet, so nothing needs compensation.
		return false, err
	}
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), slotAdmissionTimeout)
	done := make(chan slotAdmissionResult, 1)
	go func() {
		defer cancel()
		acquired, err := acquire(opCtx)
		done <- slotAdmissionResult{acquired: acquired, err: err}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			compensateSlotAdmission(release, describe)
			return false, res.err
		}
		return res.acquired, nil
	case <-ctx.Done():
		go func() {
			res := <-done
			if res.err != nil || res.acquired {
				compensateSlotAdmission(release, describe)
			}
		}()
		return false, ctx.Err()
	}
}

func compensateSlotAdmission(release func(context.Context) error, describe func() string) {
	if err := releaseExactSlotWithRetry(slotCompensationBudget, release); err != nil {
		logger.LegacyPrintf("service.concurrency", "Warning: failed to compensate ambiguous %s admission after bounded retries: %v", describe(), err)
	}
}

// releaseExactSlotWithRetry removes one exact slot member with up to three
// attempts (1s each, 100/200ms backoff) inside a total budget detached from any
// request. A short backoff also covers an ambiguous write still queued on a
// discarded connection behind the first delete.
func releaseExactSlotWithRetry(budget time.Duration, release func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		opCtx, stop := context.WithTimeout(ctx, time.Second)
		err = release(opCtx)
		stop()
		if err == nil {
			return nil
		}
		if attempt < 2 {
			timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
		if ctx.Err() != nil {
			break
		}
	}
	return err
}
