//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// A native sticky binding yields to BPS only when some BPS account could
// actually take the request. Unavailable BPS accounts (cooling, excluded by
// this request's failover, unschedulable) must not clear the binding on every
// request.
func TestOpenAINativeStickyYieldsOnlyToEligibleBPS(t *testing.T) {
	const sessionHash = "sticky_bps_yield"
	const cacheKey = "openai:" + sessionHash
	for _, tc := range []struct {
		name      string
		mutate    func(svc *OpenAIGatewayService, bps *Account, req *OpenAIAccountScheduleRequest)
		wantYield bool
	}{
		{name: "available BPS still takes over", wantYield: true},
		{name: "BPS 429 cooldown keeps native sticky", mutate: func(svc *OpenAIGatewayService, bps *Account, _ *OpenAIAccountScheduleRequest) {
			svc.excelBPSCooldownUntil.Store(bps.ID, time.Now().Add(time.Minute))
		}},
		{name: "expired BPS cooldown yields again", wantYield: true, mutate: func(svc *OpenAIGatewayService, bps *Account, _ *OpenAIAccountScheduleRequest) {
			svc.excelBPSCooldownUntil.Store(bps.ID, time.Now().Add(-time.Second))
		}},
		{name: "BPS excluded by failover keeps native sticky", mutate: func(_ *OpenAIGatewayService, bps *Account, req *OpenAIAccountScheduleRequest) {
			req.ExcludedIDs = map[int64]struct{}{bps.ID: {}}
		}},
		{name: "unschedulable BPS keeps native sticky", mutate: func(_ *OpenAIGatewayService, bps *Account, _ *OpenAIAccountScheduleRequest) {
			bps.Schedulable = false
		}},
		{name: "BPS without the requested model keeps native sticky", mutate: func(_ *OpenAIGatewayService, bps *Account, _ *OpenAIAccountScheduleRequest) {
			bps.Credentials = map[string]any{"model_mapping": map[string]any{"gpt-6-other": "gpt-6-other"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			groupID := int64(5301)
			bps := Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
				Concurrency: 1, Priority: 10, GroupIDs: []int64{groupID}, Extra: map[string]any{"openai_excel_bps": true}}
			native := Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
				Concurrency: 1, GroupIDs: []int64{groupID}}
			cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{cacheKey: native.ID}}
			svc := &OpenAIGatewayService{
				cfg: &config.Config{}, cache: cache,
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquireResults: map[int64]bool{1: true, 2: true}}),
			}
			req := OpenAIAccountScheduleRequest{GroupID: &groupID, Platform: PlatformOpenAI, SessionHash: sessionHash, RequestedModel: "gpt-6-astra"}
			if tc.mutate != nil {
				tc.mutate(svc, &bps, &req)
			}
			svc.accountRepo = schedulerTestOpenAIAccountRepo{accounts: []Account{bps, native}}
			scheduler := newDefaultOpenAIAccountScheduler(svc, nil).(*defaultOpenAIAccountScheduler)

			selection, escape, err := scheduler.selectBySessionHash(context.Background(), req)
			require.NoError(t, err)
			require.False(t, escape)
			if tc.wantYield {
				require.Nil(t, selection)
				require.Equal(t, 1, cache.deletedSessions[cacheKey], "native binding must be cleared for an eligible BPS account")
				return
			}
			require.NotNil(t, selection)
			require.Equal(t, native.ID, selection.Account.ID)
			require.True(t, selection.Acquired)
			selection.ReleaseFunc()
			require.Zero(t, cache.deletedSessions[cacheKey], "unavailable BPS must not clear the native binding")
			require.Equal(t, native.ID, cache.sessionBindings[cacheKey])
		})
	}
}

func TestHasEligibleExcelBPSAlternativeSkipsStickyAndNative(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	scheduler := &defaultOpenAIAccountScheduler{service: svc}
	req := OpenAIAccountScheduleRequest{Platform: PlatformOpenAI, RequestedModel: "gpt-6-astra"}
	sticky := Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
		Concurrency: 1, Extra: map[string]any{"openai_excel_bps": true}}
	native := Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1}
	require.False(t, scheduler.hasEligibleExcelBPSAlternative(context.Background(), req, sticky.ID, []Account{sticky, native}))
	other := sticky
	other.ID = 3
	require.True(t, scheduler.hasEligibleExcelBPSAlternative(context.Background(), req, sticky.ID, []Account{sticky, native, other}))
	wrongPlatform := other
	wrongPlatform.Platform = PlatformGrok
	require.False(t, scheduler.hasEligibleExcelBPSAlternative(context.Background(), req, sticky.ID, []Account{wrongPlatform}))
}

// count_tokens shares the session hash with the main request and goes through
// the legacy sticky path. A BPS account that cannot serve the request must not
// make it drop the native binding and rebind the session to another account.
func TestOpenAITokenCountStickyYieldsOnlyToEligibleBPS(t *testing.T) {
	const sessionHash = "sticky_bps_token_count"
	const cacheKey = "openai:" + sessionHash
	for _, tc := range []struct {
		name      string
		cooldown  bool
		wantYield bool
	}{
		{name: "available BPS takes over", wantYield: true},
		{name: "BPS 429 cooldown keeps native sticky", cooldown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			groupID := int64(5302)
			bps := Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
				Concurrency: 1, Priority: 10, GroupIDs: []int64{groupID}, Extra: map[string]any{"openai_excel_bps": true}}
			native := Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
				Concurrency: 1, Priority: 20, GroupIDs: []int64{groupID}}
			otherNative := Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
				Concurrency: 1, Priority: 5, GroupIDs: []int64{groupID}}
			cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{cacheKey: native.ID}}
			svc := &OpenAIGatewayService{
				cfg: &config.Config{}, cache: cache,
				accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{bps, native, otherNative}},
			}
			if tc.cooldown {
				svc.excelBPSCooldownUntil.Store(bps.ID, time.Now().Add(time.Minute))
			}

			account, err := svc.SelectAccountForTokenCount(context.Background(), &groupID, sessionHash, "gpt-6-astra", "", PlatformOpenAI)
			require.NoError(t, err)
			require.NotNil(t, account)
			if tc.wantYield {
				require.Equal(t, bps.ID, account.ID)
				return
			}
			require.Equal(t, native.ID, account.ID, "the session stays on its bound account")
			require.Zero(t, cache.deletedSessions[cacheKey], "an unavailable BPS account must not clear the binding")
			require.Equal(t, native.ID, cache.sessionBindings[cacheKey])
		})
	}
}
