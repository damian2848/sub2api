package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// Existing recovery fixtures represent a session with known idle history.
// Redis clock/overlap/lease semantics are exercised in repository tests.
type testSessionActivityCache struct {
	GatewayCache
	idle      bool
	err       error
	denyAfter int64
	begun     atomic.Int64
	ended     atomic.Int64
	checks    atomic.Int64
}

func withIdleActivity(cache GatewayCache) *testSessionActivityCache {
	return &testSessionActivityCache{GatewayCache: cache, idle: true}
}

func (c *testSessionActivityCache) BeginSessionActivity(context.Context, int64, string, string) (bool, error) {
	c.begun.Add(1)
	return c.idle, c.err
}

func (c *testSessionActivityCache) RefreshSessionActivity(context.Context, int64, string, string) error {
	return c.err
}

func (c *testSessionActivityCache) EndSessionActivity(ctx context.Context, _ int64, _, _ string) error {
	if ctx.Err() == nil {
		c.ended.Add(1)
	}
	return nil
}

func (c *testSessionActivityCache) SessionActivityAllowsMigration(context.Context, int64, string, string) (bool, error) {
	checks := c.checks.Add(1)
	return c.idle && (c.denyAfter == 0 || checks <= c.denyAfter), c.err
}

func idleSessionContext(ctx context.Context, groupID *int64, sessionKey string) context.Context {
	key := sessionActivityKey{groupID: derefGroupID(groupID), session: sessionKey}
	return context.WithValue(ctx, key, &sessionActivity{key: key, cache: withIdleActivity(nil), wasIdle: true})
}

func TestSessionActivityTracksRequestBeyondSelection(t *testing.T) {
	for _, result := range []string{"acquired", "wait_plan", "metadata", "error"} {
		t.Run(result, func(t *testing.T) {
			requestCtx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cache := withIdleActivity(&schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:chat": 10}})
			accounts := []Account{{ID: 10, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Priority: 25, Status: StatusActive, Schedulable: true, Concurrency: 10}}
			if result == "error" {
				accounts = nil
			}
			svc := &OpenAIGatewayService{cache: cache, accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts},
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquireResults: map[int64]bool{10: result != "wait_plan"}})}
			if result == "metadata" {
				account, err := svc.selectAccountWithoutSlot(requestCtx, OpenAIAccountScheduleRequest{Platform: PlatformOpenAI, SessionHash: "chat", RequestedModel: "gpt-5.1"})
				require.NoError(t, err)
				require.EqualValues(t, 10, account.ID)
			} else {
				selection, _, err := svc.SelectAccountWithScheduler(requestCtx, nil, "", "chat", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
				if result == "error" {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
					if result == "wait_plan" {
						require.NotNil(t, selection.WaitPlan)
					}
					if selection.ReleaseFunc != nil {
						selection.ReleaseFunc()
					}
				}
			}
			require.EqualValues(t, 1, cache.begun.Load(), "nested selectors share one request activity")
			require.Zero(t, cache.ended.Load(), "selection and slot release do not finish the HTTP request")
			cancel()
			require.Eventually(t, func() bool { return cache.ended.Load() == 1 }, time.Second, time.Millisecond)
		})
	}
}

func TestSessionActivityConservativeOnUnknownOrFailedHistory(t *testing.T) {
	for _, scenario := range []string{"unsupported", "active_or_unknown", "read_error", "non_cancellable", "overlap", "wrong_group", "wrong_provider"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			activity := withIdleActivity(&schedulerTestGatewayCache{})
			var cache GatewayCache = activity
			var groupID *int64
			key := "openai:chat"
			switch scenario {
			case "unsupported":
				cache = &schedulerTestGatewayCache{}
			case "active_or_unknown":
				activity.idle = false
			case "read_error":
				activity.err = errors.New("redis unavailable")
			case "non_cancellable":
				ctx = context.Background()
			case "overlap":
				activity.denyAfter = 1
			}
			ctx = trackSessionActivity(ctx, cache, nil, key)
			if scenario == "overlap" {
				require.True(t, sessionActivityAllowsMigration(ctx, nil, key))
			}
			if scenario == "wrong_group" {
				id := int64(2)
				groupID = &id
			}
			if scenario == "wrong_provider" {
				key = "gemini:chat"
			}
			require.False(t, sessionActivityAllowsMigration(ctx, groupID, key))
		})
	}
}

func TestWarmSessionPriorityAndNecessaryFailover(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo} {
		for _, mode := range []string{"legacy", "advanced", "weighted", "metadata", "oauth"} {
			if mode == "oauth" && platform != PlatformOpenAI {
				continue
			}
			for _, scenario := range []string{"warm", "idle", "cooldown", "excluded", "unbound", "activity_error", "activity_revoked"} {
				if mode == "metadata" && scenario == "excluded" {
					continue
				}
				t.Run(platform+"/"+mode+"/"+scenario, func(t *testing.T) {
					resetOpenAIAdvancedSchedulerSettingCacheForTest()
					t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
					model := "gpt-5.1"
					if platform == PlatformGrok {
						model = "grok-4.3"
					}
					if platform == PlatformDeepseek {
						model = "deepseek-v4-pro"
					}
					primary := Account{ID: 1, Platform: platform, Type: AccountTypeAPIKey, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 10}
					backup := primary
					backup.ID, backup.Priority = 2, 25
					if mode == "oauth" {
						backup.Type = AccountTypeOAuth
					}
					bindings := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:chat": 2}}
					activity := &testSessionActivityCache{GatewayCache: bindings, idle: scenario == "idle" || scenario == "activity_revoked"}
					var excluded map[int64]struct{}
					switch scenario {
					case "cooldown":
						end := time.Now().Add(time.Hour)
						backup.RateLimitResetAt = &end
					case "excluded":
						excluded = map[int64]struct{}{2: {}}
					case "unbound":
						bindings.sessionBindings = nil
					case "activity_error":
						activity.err = errors.New("redis unavailable")
					case "activity_revoked":
						activity.denyAfter = 1
					}
					cfg := &config.Config{}
					cfg.Gateway.OpenAIWS.LBTopK = 1
					acquired, released := []int64{}, []int64{}
					svc := &OpenAIGatewayService{cfg: cfg, cache: activity,
						accountRepo:        schedulerTestOpenAIAccountRepo{accounts: []Account{primary, backup}},
						concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquiredIDs: &acquired, releasedIDs: &released})}
					switch mode {
					case "advanced", "oauth":
						svc.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService("true")
					case "weighted":
						svc.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService("true", "true")
					}
					var account *Account
					if mode == "metadata" {
						var err error
						account, err = svc.SelectAccountForTokenCount(t.Context(), nil, "chat", model, OpenAIEndpointCapabilityChatCompletions, platform)
						require.NoError(t, err)
						require.Empty(t, acquired)
					} else {
						selection, _, err := svc.SelectAccountWithSchedulerForCapability(t.Context(), nil, "", "chat", model, excluded,
							OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions, false, true, true, platform)
						require.NoError(t, err)
						require.NotNil(t, selection)
						account = selection.Account
						if selection.ReleaseFunc != nil {
							selection.ReleaseFunc()
						}
					}
					want := int64(2)
					if scenario == "idle" || scenario == "cooldown" || scenario == "unbound" || scenario == "excluded" {
						want = 1
					}
					require.NotNil(t, account)
					require.Equal(t, want, account.ID)
					require.Equal(t, want, bindings.sessionBindings["openai:chat"])
					require.EqualValues(t, 1, activity.begun.Load())
					require.ElementsMatch(t, acquired, released, "including discarded migration candidates")
				})
			}
		}
	}
}
