package handler

import (
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

var errAntigravityRetryWaitExhausted = errors.New("Antigravity retry budget exhausted while waiting for an account")

// Only an existing Antigravity retry deadline limits this queue wait. The
// original request context is retained so an admitted generation can finish.
func (h *ConcurrencyHelper) AcquireAccountSlotWithRetryBudget(c *gin.Context, accountID int64, maxConcurrency int, timeout time.Duration, isStream bool, streamStarted *bool) (func(), error) {
	deadline, limited := service.AntigravityRetryDeadline(c)
	if !limited {
		return h.AcquireAccountSlotWithWaitTimeout(c, accountID, maxConcurrency, timeout, isStream, streamStarted)
	}
	return h.acquireAccountSlotBeforeRetryDeadline(c, accountID, maxConcurrency, timeout, streamStarted, deadline)
}

func (h *ConcurrencyHelper) acquireAccountSlotBeforeRetryDeadline(c *gin.Context, accountID int64, maxConcurrency int, timeout time.Duration, streamStarted *bool, deadline time.Time) (func(), error) {
	if err := c.Request.Context().Err(); err != nil {
		return nil, err
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return nil, errAntigravityRetryWaitExhausted
	}
	if remaining < timeout {
		timeout = remaining
	}
	// Do not commit a new heartbeat while waiting within this short retry
	// budget: exhaustion must still be able to return the last upstream status.
	release, err := h.AcquireAccountSlotWithWaitTimeout(c, accountID, maxConcurrency, timeout, false, streamStarted)
	if c.Request.Context().Err() != nil {
		if release != nil {
			release()
		}
		return nil, c.Request.Context().Err()
	}
	if !time.Now().Before(deadline) {
		if release != nil {
			release()
		}
		return nil, errAntigravityRetryWaitExhausted
	}
	return release, err
}
