package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
	"github.com/stretchr/testify/require"
)

func tripGeminiWebHealth(t *testing.T, tracker *geminiWebHealthTracker, account *Account, now time.Time) geminiWebHealthCooldown {
	t.Helper()
	var cooldown geminiWebHealthCooldown
	for i := range geminiWebHealthFailureThreshold {
		attempt, admitted := tracker.begin(account, now)
		require.True(t, admitted)
		cooldown = tracker.finish(attempt, geminiWebHealthFailure, now)
		if i < geminiWebHealthFailureThreshold-1 {
			require.True(t, cooldown.until.IsZero())
		}
	}
	require.Equal(t, now.Add(time.Minute), cooldown.until)
	return cooldown
}

func TestGeminiWebHealthFailureWindowAndSuccessReset(t *testing.T) {
	now := time.Now()
	account := &Account{ID: 243}
	var tracker geminiWebHealthTracker
	for _, at := range []time.Time{now, now.Add(30 * time.Second), now.Add(61 * time.Second)} {
		attempt, admitted := tracker.begin(account, at)
		require.True(t, admitted)
		require.True(t, tracker.finish(attempt, geminiWebHealthFailure, at).until.IsZero(),
			"failures must fit inside one rolling window")
	}
	attempt, admitted := tracker.begin(account, now.Add(62*time.Second))
	require.True(t, admitted)
	tracker.finish(attempt, geminiWebHealthSuccess, now.Add(62*time.Second))
	tripGeminiWebHealth(t, &tracker, account, now.Add(63*time.Second))
}

func TestGeminiWebHealthHalfOpenIsSingleAndEscalates(t *testing.T) {
	now := time.Now()
	account := &Account{ID: 243}
	var tracker geminiWebHealthTracker
	cooldown := tripGeminiWebHealth(t, &tracker, account, now)
	_, admitted := tracker.begin(account, cooldown.until.Add(-time.Nanosecond))
	require.False(t, admitted)

	var admittedCount atomic.Int32
	var probe geminiWebHealthAttempt
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			attempt, ok := tracker.begin(account, cooldown.until)
			if ok {
				probe = attempt // exactly one writer, asserted after the join
				admittedCount.Add(1)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), admittedCount.Load())
	require.True(t, probe.probe)
	for _, duration := range []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute} {
		now = cooldown.until
		cooldown = tracker.finish(probe, geminiWebHealthFailure, now)
		require.Equal(t, now.Add(duration), cooldown.until)
		probe, admitted = tracker.begin(account, cooldown.until)
		require.True(t, admitted)
	}
	tracker.finish(probe, geminiWebHealthSuccess, cooldown.until)
	for range 4 {
		attempt, ok := tracker.begin(account, cooldown.until)
		require.True(t, ok, "successful recovery restores ordinary concurrent admission")
		require.False(t, attempt.probe)
	}
}

func TestGeminiWebHealthStaleSuccessCannotUndoCooldown(t *testing.T) {
	now := time.Now()
	account := &Account{ID: 243}
	var tracker geminiWebHealthTracker
	old, ok := tracker.begin(account, now)
	require.True(t, ok)
	cooldown := tripGeminiWebHealth(t, &tracker, account, now)
	tracker.finish(old, geminiWebHealthSuccess, now.Add(time.Second))
	_, ok = tracker.begin(account, now.Add(2*time.Second))
	require.False(t, ok)
	probe, ok := tracker.begin(account, cooldown.until)
	require.True(t, ok)
	tracker.finish(old, geminiWebHealthFailure, cooldown.until)
	_, ok = tracker.begin(account, cooldown.until)
	require.False(t, ok, "an old result must not release the active probe")
	tracker.finish(probe, geminiWebHealthNeutral, cooldown.until)
	released := probe
	probe, ok = tracker.begin(account, cooldown.until)
	require.True(t, ok, "cancellation releases the probe without escalating")
	require.True(t, probe.probe)
	tracker.finish(released, geminiWebHealthNeutral, cooldown.until)
	_, ok = tracker.begin(account, cooldown.until)
	require.False(t, ok, "deferred cleanup of an old probe must not release its successor")
	tracker.finish(probe, geminiWebHealthSuccess, cooldown.until)
}

func TestGeminiWebHealthRestoresPersistedCooldownAndIgnoresStaleSnapshot(t *testing.T) {
	now := time.Now()
	until := now.Add(time.Minute)
	account := &Account{ID: 243, TempUnschedulableUntil: &until,
		TempUnschedulableReason: geminiWebHealthCooldownReason + "; level=2"}
	var tracker geminiWebHealthTracker
	_, ok := tracker.begin(account, now)
	require.False(t, ok)
	probe, ok := tracker.begin(account, until)
	require.True(t, ok)
	require.True(t, probe.probe)
	tracker.finish(probe, geminiWebHealthSuccess, until)
	for range 2 {
		attempt, ok := tracker.begin(account, until)
		require.True(t, ok)
		require.False(t, attempt.probe, "old snapshot must not re-open a recovered circuit")
	}
	newUntil := until.Add(time.Minute)
	account.TempUnschedulableUntil = &newUntil
	_, ok = tracker.begin(account, until)
	require.False(t, ok, "a newer persisted cooldown still wins")
}

func TestGeminiWebHealthClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		res  geminiweb.Result
		want geminiWebHealthOutcome
	}{
		{name: "503", err: &geminiweb.UpstreamHTTPError{StatusCode: 503}, want: geminiWebHealthFailure},
		{name: "wrapped 500", err: fmt.Errorf("wrapped: %w", &geminiweb.UpstreamHTTPError{StatusCode: 500}), want: geminiWebHealthFailure},
		{name: "empty", err: geminiweb.ErrEmptyResponse, want: geminiWebHealthFailure},
		{name: "Bard 1095", err: &geminiweb.UpstreamRPCError{RPCCode: 8, BardCode: 1095}, want: geminiWebHealthFailure},
		{name: "wrapped Bard 1095", err: fmt.Errorf("wrapped: %w", &geminiweb.UpstreamRPCError{RPCCode: 8, BardCode: 1095}), want: geminiWebHealthFailure},
		{name: "RPC without recognized Bard detail", err: &geminiweb.UpstreamRPCError{RPCCode: 8}, want: geminiWebHealthFailure},
		{name: "wrapped RPC without recognized Bard detail", err: fmt.Errorf("wrapped: %w", &geminiweb.UpstreamRPCError{RPCCode: 8}), want: geminiWebHealthFailure},
		{name: "unknown Bard code", err: &geminiweb.UpstreamRPCError{RPCCode: 8, BardCode: 123456}, want: geminiWebHealthFailure},
		{name: "Bard credential", err: &geminiweb.UpstreamRPCError{RPCCode: 8, BardCode: 1016}},
		{name: "Bard rate limited", err: &geminiweb.UpstreamRPCError{RPCCode: 8, BardCode: 1037}},
		{name: "Bard rate limited 1013", err: &geminiweb.UpstreamRPCError{RPCCode: 8, BardCode: 1013}},
		{name: "known Bard 1050", err: &geminiweb.UpstreamRPCError{RPCCode: 8, BardCode: 1050}},
		{name: "known Bard 1052", err: &geminiweb.UpstreamRPCError{RPCCode: 8, BardCode: 1052}},
		{name: "known Bard 1060", err: &geminiweb.UpstreamRPCError{RPCCode: 8, BardCode: 1060}},
		{name: "parameter error", err: &geminiweb.UpstreamHTTPError{StatusCode: 400}},
		{name: "unsupported model", err: errors.New("unsupported model")},
		{name: "canceled", err: context.Canceled},
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "transport", err: errors.New("connection reset")},
		{name: "credential", err: geminiweb.ErrInvalidCredential},
		{name: "429", err: geminiweb.ErrRateLimited},
		{name: "canned", res: geminiweb.Result{Text: geminiWebCannedText}},
		{name: "failed generation", res: geminiweb.Result{GenerationFailed: true, Text: "unknown failure"}},
		{name: "answer", res: geminiweb.Result{Text: "hello"}, want: geminiWebHealthSuccess},
		{name: "refusal", res: geminiweb.Result{Text: "Your request may go against my [guidelines](https://gemini.google/policy-guidelines/). Would you like to try something else?"}, want: geminiWebHealthSuccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, geminiWebClassifyHealth(context.Background(), tc.res, tc.err, geminiweb.CannedReplies{}))
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Equal(t, geminiWebHealthNeutral, geminiWebClassifyHealth(ctx, geminiweb.Result{}, geminiweb.ErrEmptyResponse, geminiweb.CannedReplies{}))
	require.Equal(t, geminiWebHealthNeutral, geminiWebClassifyHealth(ctx, geminiweb.Result{}, &geminiweb.UpstreamRPCError{RPCCode: 8, BardCode: 1095}, geminiweb.CannedReplies{}))
	require.Equal(t, geminiWebHealthNeutral, geminiWebClassifyHealth(ctx, geminiweb.Result{}, &geminiweb.UpstreamRPCError{RPCCode: 8}, geminiweb.CannedReplies{}))
}

func TestGeminiWebHealthUnknownRPCUsesBoundedCooldown(t *testing.T) {
	for _, bardCode := range []int{0, 1095, 123456} {
		t.Run(fmt.Sprintf("BardCode=%d", bardCode), func(t *testing.T) {
			now := time.Now()
			account := &Account{ID: 1095}
			var tracker geminiWebHealthTracker
			upstreamErr := &geminiweb.UpstreamRPCError{RPCCode: 8, BardCode: bardCode}
			for i := 1; i <= geminiWebHealthFailureThreshold; i++ {
				attempt, ok := tracker.begin(account, now)
				require.True(t, ok)
				outcome := geminiWebClassifyHealth(context.Background(), geminiweb.Result{}, upstreamErr, geminiweb.CannedReplies{})
				cooldown := tracker.finish(attempt, outcome, now)
				if i < geminiWebHealthFailureThreshold {
					require.True(t, cooldown.until.IsZero())
				} else {
					require.Equal(t, now.Add(geminiWebHealthInitialCooldown), cooldown.until)
				}
			}
			_, ok := tracker.begin(account, now)
			require.False(t, ok, "repeated unknown RPC errors must cool down the account")
			attempt, ok := tracker.begin(account, now.Add(geminiWebHealthInitialCooldown))
			require.True(t, ok)
			require.True(t, attempt.probe, "the bounded cooldown must allow a recovery probe")
		})
	}
}

type geminiWebHealthRepo struct {
	AccountRepository
	mu                 sync.Mutex
	writes             int
	until              time.Time
	reason             string
	ctxErr             error
	fail               bool
	currentGeneration  string
	capturedGeneration string
	rejected           int
}

func (r *geminiWebHealthRepo) SetGeminiWebTempUnschedulable(ctx context.Context, _ int64, generation string, until time.Time, reason string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.capturedGeneration = generation
	if generation != r.currentGeneration {
		r.rejected++
		return false, nil
	}
	r.writes++
	r.until, r.reason, r.ctxErr = until, reason, ctx.Err()
	if r.fail {
		return false, errors.New("storage unavailable")
	}
	return true, nil
}

type geminiWebHealthUpstream struct {
	HTTPUpstream
	status int
	calls  atomic.Int32
}

func (u *geminiWebHealthUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	status := http.StatusOK
	body := `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`
	if strings.Contains(req.URL.Path, "StreamGenerate") {
		u.calls.Add(1)
		status, body = u.status, ""
	} else if req.Method == http.MethodPost {
		// The read-only quota RPC is unrelated to generation health.
		body = "unavailable quota"
	}
	return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestGeminiWebHealthForwardIsolates503AndEmptyAcrossProtocols(t *testing.T) {
	useGeminiWebAnalyticsDir(t)
	for _, protocol := range []string{"native", "messages", "chat"} {
		for _, status := range []int{503, 200} {
			t.Run(fmt.Sprintf("%s/%d", protocol, status), func(t *testing.T) {
				upstream := &geminiWebHealthUpstream{status: status}
				repo := &geminiWebHealthRepo{}
				svc := &GeminiMessagesCompatService{httpUpstream: upstream, accountRepo: repo, geminiWeb: geminiweb.NewClient()}
				account := geminiWebTestAccount()
				account.ID, account.Status, account.Schedulable = 243, StatusActive, true
				for i := range 4 {
					_, c := geminiWebTestContext()
					var err error
					switch protocol {
					case "native":
						_, err = svc.ForwardNative(c.Request.Context(), c, account, "gemini-4-argon", "generateContent", false,
							[]byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`))
					case "messages":
						_, err = svc.Forward(c.Request.Context(), c, account, geminiWebCannedRequest("messages", false, false))
					case "chat":
						_, err = svc.ForwardAsChatCompletions(c.Request.Context(), c, account, geminiWebCannedRequest("chat", false, false))
					}
					var failover *UpstreamFailoverError
					require.ErrorAs(t, err, &failover)
					require.False(t, failover.RetryableOnSameAccount, "isolation must not add same-account retries")
					if i == 3 {
						require.Equal(t, GatewayFailureReason("gemini_web_health_cooldown"), failover.Reason)
					}
				}
				require.Equal(t, int32(3), upstream.calls.Load(), "fourth request must not reach the failed upstream")
				repo.mu.Lock()
				require.Equal(t, 1, repo.writes)
				require.NoError(t, repo.ctxErr)
				require.Contains(t, repo.reason, geminiWebHealthCooldownReason)
				require.WithinDuration(t, time.Now().Add(time.Minute), repo.until, 5*time.Second)
				repo.mu.Unlock()
				require.False(t, account.IsSchedulable(), "account snapshot immediately reflects its persisted cooldown")
			})
		}
	}
}

func TestGeminiWebHealthLocalGateSurvivesPersistenceFailure(t *testing.T) {
	now := time.Now()
	account := geminiWebTestAccount()
	account.ID = 243
	repo := &geminiWebHealthRepo{fail: true}
	svc := &GeminiMessagesCompatService{accountRepo: repo}
	for range 3 {
		attempt, ok := svc.geminiWebHealth.begin(account, now)
		require.True(t, ok)
		svc.finishGeminiWebHealth(context.Background(), account, attempt, geminiweb.Result{}, geminiweb.ErrEmptyResponse)
	}
	_, admitted := svc.geminiWebHealth.begin(account, time.Now())
	require.False(t, admitted)
	require.Equal(t, 1, repo.writes)
}

func TestGeminiWebHealthForwardReleasesProbeAfterPanic(t *testing.T) {
	account := geminiWebTestAccount()
	account.ID = 243
	expired := time.Now().Add(-time.Second)
	account.TempUnschedulableUntil = &expired
	account.TempUnschedulableReason = geminiWebHealthCooldownReason + "; level=1"
	upstream := &geminiWebUsageHTTPStub{do: func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "StreamGenerate") {
			panic("upstream adapter failed")
		}
		body := `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(body))}, nil
	}}
	svc := &GeminiMessagesCompatService{httpUpstream: upstream, geminiWeb: geminiweb.NewClient()}
	require.Panics(t, func() {
		_, _ = svc.callGeminiWeb(context.Background(), account, "gemini-4-argon", "hello", false)
	})
	probe, admitted := svc.geminiWebHealth.begin(account, time.Now())
	require.True(t, admitted, "HTTP recovery must not leave the account's probe permanently owned")
	require.True(t, probe.probe)
	svc.geminiWebHealth.finish(probe, geminiWebHealthNeutral, time.Now())
	inflight, _, done := svc.geminiWebActivity.begin(account.ID, time.Now())
	defer done()
	require.Equal(t, 1, inflight, "panicking upstream calls must also release activity accounting")
}

func TestGeminiWebHealthOldCredentialCannotQuarantineRecovery(t *testing.T) {
	now := time.Now()
	account := geminiWebTestAccount()
	account.ID = 243
	account.Credentials[geminiWebCookieGeneration] = "old"
	repo := &geminiWebHealthRepo{currentGeneration: "new"}
	svc := &GeminiMessagesCompatService{accountRepo: repo}
	for range 2 {
		attempt, ok := svc.geminiWebHealth.begin(account, now)
		require.True(t, ok)
		svc.finishGeminiWebHealth(context.Background(), account, attempt, geminiweb.Result{}, geminiweb.ErrEmptyResponse)
	}
	attempt, ok := svc.geminiWebHealth.begin(account, now)
	require.True(t, ok)
	// The selected snapshot may have been refreshed while an old request was
	// in flight. Persistence must still use that request's captured generation.
	account.Credentials[geminiWebCookieGeneration] = "new"
	svc.finishGeminiWebHealth(context.Background(), account, attempt, geminiweb.Result{}, geminiweb.ErrEmptyResponse)
	require.Equal(t, "old", repo.capturedGeneration)
	require.Equal(t, 1, repo.rejected)
	require.Zero(t, repo.writes)
	require.Nil(t, account.TempUnschedulableUntil)
	require.Empty(t, account.TempUnschedulableReason)
	newAttempt, admitted := svc.geminiWebHealth.begin(account, now)
	require.True(t, admitted, "replacement credentials must not inherit the old local circuit")
	require.False(t, newAttempt.probe)
}

func TestGeminiWebHealthQuotaAdmissionKeepsStatusAndSeparateReason(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{geminiweb.ErrRateLimited, http.StatusTooManyRequests},
		{geminiweb.ErrInvalidCredential, http.StatusUnauthorized},
	} {
		wrapped := &geminiWebQuotaPrecheckError{cause: tc.err}
		require.ErrorIs(t, wrapped, tc.err)
		var failover *UpstreamFailoverError
		require.ErrorAs(t, geminiWebFailoverError(wrapped), &failover)
		require.Equal(t, tc.status, failover.StatusCode)
		require.Equal(t, GeminiWebQuotaPrecheckReason, failover.Reason)
		require.Equal(t, GatewayFailureScopeAccount, failover.Scope)
		require.ErrorAs(t, geminiWebFailoverError(tc.err), &failover)
		require.NotEqual(t, GeminiWebQuotaPrecheckReason, failover.Reason, "real Generate errors consume an upstream attempt")
	}
}

func TestGeminiWebHealthQuotaRejectionDoesNotAttemptGeneration(t *testing.T) {
	account := geminiWebTestAccount()
	account.ID = 243
	expired := time.Now().Add(-time.Second)
	account.TempUnschedulableUntil = &expired
	account.TempUnschedulableReason = geminiWebHealthCooldownReason + "; level=1"
	upstream := geminiWebQuotaStub([]any{"", "1", 1})
	original := upstream.do
	generationCalls := 0
	upstream.do = func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "StreamGenerate") {
			generationCalls++
		}
		return original(req)
	}
	svc := &GeminiMessagesCompatService{httpUpstream: upstream, geminiWeb: geminiweb.NewClient()}
	_, err := svc.callGeminiWeb(context.Background(), account, "gemini-4-argon", "hello", false)
	require.ErrorIs(t, err, geminiweb.ErrRateLimited)
	var precheck *geminiWebQuotaPrecheckError
	require.ErrorAs(t, err, &precheck)
	require.Zero(t, generationCalls)
	require.Nil(t, svc.geminiWebActivity.last, "quota admission is not an upstream generation attempt")
	probe, admitted := svc.geminiWebHealth.begin(account, time.Now())
	require.True(t, admitted, "quota rejection must release a half-open health probe without escalation")
	require.True(t, probe.probe)
}
