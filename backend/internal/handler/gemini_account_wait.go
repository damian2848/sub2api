package handler

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const geminiAccountReselectInterval = time.Second

var errGeminiAccountCannotWait = errors.New("no available Gemini account wait plan")

// The Gemini Web / Antigravity admission/retry clock is request-scoped, starting
// at first selection and activated when either provider is selected. It never becomes the
// context of a forwarded request: an admitted generation may last minutes.
type geminiAdmissionBudget struct {
	startedAt          time.Time
	deadline           time.Time
	excludeForwardTime bool
}

func newGeminiAdmissionBudget() *geminiAdmissionBudget {
	return &geminiAdmissionBudget{startedAt: time.Now()}
}

func (b *geminiAdmissionBudget) observeAccount(account *service.Account) {
	if account != nil && (account.Platform == service.PlatformAntigravity || account.IsGeminiWeb()) && b.deadline.IsZero() {
		b.deadline = b.startedAt.Add(maxConcurrencyWait)
		b.excludeForwardTime = account.IsGeminiWeb()
	}
	if account != nil && account.Platform == service.PlatformAntigravity {
		b.excludeForwardTime = false
	}
}

// Web generation often takes longer than the queue allowance. Only selection,
// admission and retry backoff consume its budget; a slow failed generation
// must still be able to use the remaining allowance on another account.
func (b *geminiAdmissionBudget) pauseForForward() func(error) {
	if !b.excludeForwardTime {
		return func(error) {}
	}
	started := time.Now()
	return func(err error) {
		var failoverErr *service.UpstreamFailoverError
		if errors.As(err, &failoverErr) && isGeminiWebAdmissionRejection(failoverErr) {
			// An account rejected before generation only consumed admission time.
			return
		}
		b.deadline = b.deadline.Add(time.Since(started))
	}
}

func (b *geminiAdmissionBudget) deadlineFor(c *gin.Context) time.Time {
	deadline := b.deadline
	if retryDeadline, ok := service.AntigravityRetryDeadline(c); ok && (deadline.IsZero() || retryDeadline.Before(deadline)) {
		deadline = retryDeadline
	}
	return deadline
}

func (b *geminiAdmissionBudget) context(c *gin.Context) (context.Context, context.CancelFunc) {
	if deadline := b.deadlineFor(c); !deadline.IsZero() {
		return context.WithDeadline(c.Request.Context(), deadline)
	}
	return context.WithCancel(c.Request.Context())
}

func (b *geminiAdmissionBudget) err(c *gin.Context) error {
	if err := c.Request.Context().Err(); err != nil {
		return err
	}
	if deadline := b.deadlineFor(c); !deadline.IsZero() && !time.Now().Before(deadline) {
		return &ConcurrencyError{SlotType: "account", IsTimeout: true}
	}
	return nil
}

type geminiAlternativeSelector func(context.Context, int64) (*service.AccountSelectionResult, error)

func isGeminiWebAdmissionRejection(err *service.UpstreamFailoverError) bool {
	return err != nil && (err.Reason == service.GeminiWebHealthCooldownReason || err.Reason == service.GeminiWebQuotaPrecheckReason)
}

// Revalidation owns cleanup on rejection so every protocol balances a slot
// acquired just before another request placed this account in cooldown.
func (h *GatewayHandler) revalidateGeminiWebAdmission(c *gin.Context, selection *service.AccountSelectionResult, budget *geminiAdmissionBudget, groupID *int64, model string) error {
	if !selection.Account.IsGeminiWeb() {
		return nil
	}
	ctx, cancel := budget.context(c)
	defer cancel()
	latest, err := h.gatewayService.RevalidateGeminiWebAdmission(ctx, groupID, selection.Account, model)
	if err == nil {
		err = budget.err(c)
	}
	if err != nil {
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		return err
	}
	selection.Account = latest
	return nil
}

// acquireGeminiAccount keeps one queue reservation while checking periodically
// for an already-acquired eligible alternative. Busy alternatives are ignored;
// they neither move the queue nor create a sticky-session/signature switch.
func acquireGeminiAccount(
	c *gin.Context,
	concurrency *ConcurrencyHelper,
	selection *service.AccountSelectionResult,
	budget *geminiAdmissionBudget,
	boundAccount bool,
	streamStarted *bool,
	selectAlternative geminiAlternativeSelector,
) (*service.AccountSelectionResult, error) {
	budget.observeAccount(selection.Account)
	if err := budget.err(c); err != nil {
		if selection.Acquired && selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		return nil, err
	}
	if selection.Acquired {
		return selection, nil
	}
	if selection.WaitPlan == nil {
		return nil, errGeminiAccountCannotWait
	}
	plan := selection.WaitPlan
	_, hasRetryDeadline := service.AntigravityRetryDeadline(c)
	reselectionEnabled := selection.Account.Platform == service.PlatformAntigravity || selection.Account.IsGeminiWeb() || hasRetryDeadline
	// Other providers keep their configured per-account wait policy. An actual
	// Gemini Web / Antigravity budget still follows a mixed-pool request across accounts.
	if budget.deadlineFor(c).IsZero() {
		budget = &geminiAdmissionBudget{deadline: time.Now().Add(plan.Timeout)}
	}
	// A shorter scheduler wait policy is also a total budget, not a new allowance
	// for every account visited by the enclosing failover loop.
	if deadline := time.Now().Add(plan.Timeout); budget.deadline.IsZero() || deadline.Before(budget.deadline) {
		budget.deadline = deadline
	}
	canReselect := func() bool {
		return reselectionEnabled && !boundAccount && selectAlternative != nil && !*streamStarted && !c.Writer.Written()
	}
	tryAlternative := func() (*service.AccountSelectionResult, error) {
		if !canReselect() {
			return nil, nil
		}
		ctx, cancel := budget.context(c)
		defer cancel()
		alternative, err := selectAlternative(ctx, selection.Account.ID)
		if err != nil || alternative == nil {
			return nil, budget.err(c)
		}
		if budgetErr := budget.err(c); budgetErr != nil {
			if alternative.ReleaseFunc != nil {
				alternative.ReleaseFunc()
			}
			return nil, budgetErr
		}
		if !alternative.Acquired || alternative.Account == nil || alternative.Account.ID == selection.Account.ID || !canReselect() {
			if alternative.ReleaseFunc != nil {
				alternative.ReleaseFunc()
			}
			return nil, nil
		}
		budget.observeAccount(alternative.Account)
		return alternative, nil
	}

	waitCtx, waitCancel := budget.context(c)
	canWait, err := concurrency.IncrementAccountWaitCount(waitCtx, selection.Account.ID, plan.MaxWaiting)
	waitCancel()
	if err == nil && !canWait {
		if alternative, selectErr := tryAlternative(); alternative != nil || selectErr != nil {
			return alternative, selectErr
		}
		return nil, &WaitQueueFullError{SlotType: "account"}
	}
	if err == nil && canWait {
		defer concurrency.DecrementAccountWaitCount(c.Request.Context(), selection.Account.ID)
	}
	for {
		if err := budget.err(c); err != nil {
			return nil, err
		}
		wait := time.Until(budget.deadlineFor(c))
		if canReselect() && wait > geminiAccountReselectInterval {
			wait = geminiAccountReselectInterval
		}
		release, err := concurrency.AcquireAccountSlotWithWaitTimeout(c, selection.Account.ID, plan.MaxConcurrency, wait, false, streamStarted)
		if err == nil {
			if budgetErr := budget.err(c); budgetErr != nil {
				if release != nil {
					release()
				}
				return nil, budgetErr
			}
			admitted := *selection
			admitted.ReleaseFunc = release
			return &admitted, nil
		}
		var concurrencyErr *ConcurrencyError
		// The slot cache can surface the slice context's deadline directly before
		// the wait helper converts it to ConcurrencyError. Both end this slice,
		// while the parent request and total budget still decide whether to stop.
		sliceTimedOut := (errors.As(err, &concurrencyErr) && concurrencyErr.IsTimeout) ||
			(c.Request.Context().Err() == nil && errors.Is(err, context.DeadlineExceeded))
		if !sliceTimedOut {
			return nil, err
		}
		if err := budget.err(c); err != nil {
			return nil, err
		}
		if alternative, err := tryAlternative(); alternative != nil || err != nil {
			return alternative, err
		}
	}
}

// A local admission deadline is not a disconnected client. Preserve the last
// upstream failure on retry exhaustion instead of changing it to a queue error.
func (h *GatewayHandler) handleGeminiAdmissionFailure(c *gin.Context, err error, last *service.UpstreamFailoverError, cachedContentBound bool) {
	if failoverClientGone(c) || c.Writer.Written() {
		return
	}
	if last != nil {
		if cachedContentBound {
			h.handleGeminiCachedContentFailoverExhausted(c, last)
		} else {
			h.handleGeminiFailoverExhausted(c, last)
		}
		return
	}
	if errors.Is(err, errGeminiAccountCannotWait) {
		markOpsRoutingCapacityLimited(c)
		googleError(c, http.StatusServiceUnavailable, "No available Gemini accounts")
		return
	}
	googleConcurrencyError(c, err, "account")
}
