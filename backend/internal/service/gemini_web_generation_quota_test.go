package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
	"github.com/stretchr/testify/require"
)

func geminiWebGenerationQuotaAccount() *Account {
	return &Account{
		ID: 259, Platform: PlatformGemini, Type: AccountTypeGeminiWeb,
		Credentials: map[string]any{"gemini_web_cookie": "__Secure-1PSID=psid"},
	}
}

func TestPrecheckGeminiWebGenerationQuota(t *testing.T) {
	resetAt := time.Now().Add(time.Hour).Truncate(time.Second)
	for _, tc := range []struct {
		name      string
		window    []any
		exhausted bool
		wantReset time.Time
	}{
		{"available", []any{"", "0.25", 1}, false, time.Time{}},
		{"exhausted known reset", []any{"", "1", 1, []any{strconv.FormatInt(resetAt.Unix(), 10), "0"}}, true, resetAt},
		{"exhausted unknown reset", []any{"", "1", 1}, true, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &geminiWebQuotaRateLimitRepo{}
			upstream := geminiWebQuotaStub(tc.window)
			calls := 0
			original := upstream.do
			upstream.do = func(req *http.Request) (*http.Response, error) {
				calls++
				return original(req)
			}
			svc := &GeminiMessagesCompatService{accountRepo: repo, httpUpstream: upstream, geminiWeb: geminiweb.NewClient()}
			account := geminiWebGenerationQuotaAccount()
			start := time.Now()

			err := svc.precheckGeminiWebGenerationQuota(context.Background(), account)

			if tc.exhausted {
				require.ErrorIs(t, err, geminiweb.ErrRateLimited)
				require.Len(t, repo.setCalls, 1)
				require.Zero(t, repo.legacySetCalls)
				if tc.wantReset.IsZero() {
					require.WithinDuration(t, start.Add(time.Minute), *account.RateLimitResetAt, time.Second)
				} else {
					require.True(t, tc.wantReset.Equal(*account.RateLimitResetAt))
				}
			} else {
				require.NoError(t, err)
				require.Empty(t, repo.setCalls)
				// The selected account's next precheck uses the shared cache.
				require.NoError(t, svc.precheckGeminiWebGenerationQuota(context.Background(), account))
				require.Equal(t, 2, calls, "one bootstrap and one usage RPC")
			}
		})
	}
}

func TestPrecheckGeminiWebGenerationQuotaFailsOpenOnReadTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc := &GeminiMessagesCompatService{httpUpstream: &geminiWebUsageHTTPStub{
			do: func(req *http.Request) (*http.Response, error) {
				<-req.Context().Done()
				return nil, req.Context().Err()
			},
		}}
		start := time.Now()

		err := svc.precheckGeminiWebGenerationQuota(context.Background(), geminiWebGenerationQuotaAccount())

		require.NoError(t, err)
		require.Equal(t, geminiWebQuotaPrecheckTimeout, time.Since(start))
	})
}

func TestPrecheckGeminiWebGenerationQuotaRecoversAfterUnknownResetCooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bucket := []any{"", "1", 1}
		repo := &geminiWebQuotaRateLimitRepo{}
		svc := &GeminiMessagesCompatService{accountRepo: repo, httpUpstream: geminiWebQuotaStub(bucket)}
		account := geminiWebGenerationQuotaAccount()
		require.ErrorIs(t, svc.precheckGeminiWebGenerationQuota(context.Background(), account), geminiweb.ErrRateLimited)
		require.NotNil(t, account.RateLimitResetAt)

		// Both the inferred cooldown and quota cache expire before probing
		// again. A recovered window must not re-arm the inferred limit.
		time.Sleep(time.Minute + time.Second)
		bucket[1] = "0.2"
		require.NoError(t, svc.precheckGeminiWebGenerationQuota(context.Background(), account))
		require.Len(t, repo.setCalls, 1)
		require.True(t, account.RateLimitResetAt.Before(time.Now()))
	})
}

func TestPrecheckGeminiWebGenerationQuotaPreservesCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	svc := &GeminiMessagesCompatService{httpUpstream: &geminiWebUsageHTTPStub{
		do: func(req *http.Request) (*http.Response, error) {
			cancel()
			return nil, req.Context().Err()
		},
	}}

	err := svc.precheckGeminiWebGenerationQuota(ctx, geminiWebGenerationQuotaAccount())

	require.ErrorIs(t, err, context.Canceled)
}

func TestPrecheckGeminiWebGenerationQuotaPersistsKnownExhaustionAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	repo := &geminiWebQuotaRateLimitRepo{}
	upstream := geminiWebQuotaStub([]any{"", "1", 1})
	original := upstream.do
	upstream.do = func(req *http.Request) (*http.Response, error) {
		resp, err := original(req)
		if req.Method == http.MethodPost {
			cancel()
		}
		return resp, err
	}
	svc := &GeminiMessagesCompatService{accountRepo: repo, httpUpstream: upstream}

	err := svc.precheckGeminiWebGenerationQuota(ctx, geminiWebGenerationQuotaAccount())

	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, repo.setCalls, 1)
	require.Nil(t, repo.stateContextErr)
}

func TestPrecheckGeminiWebGenerationQuotaRejectsMissingCookie(t *testing.T) {
	svc := &GeminiMessagesCompatService{}
	account := geminiWebGenerationQuotaAccount()
	account.Credentials = nil

	require.ErrorIs(t, svc.precheckGeminiWebGenerationQuota(context.Background(), account), geminiweb.ErrInvalidCredential)
}

func TestPrecheckGeminiWebGenerationQuotaClassifiesReadErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   error
	}{
		{"quota 429", http.StatusTooManyRequests, geminiweb.ErrRateLimited},
		{"invalid credential", http.StatusUnauthorized, geminiweb.ErrInvalidCredential},
		{"transient quota endpoint 503", http.StatusServiceUnavailable, nil},
		{"undecodable usage", http.StatusOK, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &GeminiMessagesCompatService{httpUpstream: &geminiWebUsageHTTPStub{
				do: func(req *http.Request) (*http.Response, error) {
					status, body := http.StatusOK, `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`
					if req.Method == http.MethodPost {
						status, body = tc.status, "unavailable"
					}
					return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
				},
			}}
			err := svc.precheckGeminiWebGenerationQuota(context.Background(), geminiWebGenerationQuotaAccount())
			if tc.want == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.want)
			}
		})
	}
}

func TestApplyGeminiWebQuotaRateLimitDoesNotRecoverFromOlderCache(t *testing.T) {
	now := time.Now()
	limitedAt, resetAt := now.Add(-time.Minute), now.Add(time.Hour)
	account := &Account{ID: 259, RateLimitedAt: &limitedAt, RateLimitResetAt: &resetAt}
	repo := &geminiWebQuotaRateLimitRepo{}
	quota := geminiweb.Quota{
		ObservedAt: limitedAt.Add(-time.Second),
		Windows:    []geminiweb.QuotaWindow{geminiWebTestWindow("five_hour", 20, nil)},
	}

	applyGeminiWebQuotaRateLimit(context.Background(), repo, account, quota, now)

	require.Zero(t, repo.clearCalls)
	require.Zero(t, repo.legacyClearCalls)
	require.Equal(t, &resetAt, account.RateLimitResetAt)
}

func TestPrecheckGeminiWebGenerationQuotaFailsOpenOnTransportError(t *testing.T) {
	svc := &GeminiMessagesCompatService{httpUpstream: &geminiWebUsageHTTPStub{
		do: func(*http.Request) (*http.Response, error) {
			return nil, errors.New("upstream connection reset")
		},
	}}
	require.NoError(t, svc.precheckGeminiWebGenerationQuota(context.Background(), geminiWebGenerationQuotaAccount()))
}
