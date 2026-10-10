//go:build unit

package service

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestGatewayPriorityRecoveryAcrossPlatforms(t *testing.T) {
	for _, platform := range []string{PlatformAnthropic, PlatformGemini, PlatformAntigravity, PlatformTypeSafe} {
		for _, mode := range []string{"batch", "legacy", "metadata"} {
			for _, scenario := range []string{"recovered", "active", "activity_error", "activity_revoked", "backup_cooldown", "backup_excluded", "unbound", "same_priority", "near_full", "real_full", "queued", "load_error", "load_missing", "slot_race", "cooldown", "wrong_model", "wrong_group", "excluded"} {
				t.Run(platform+"/"+mode+"/"+scenario, func(t *testing.T) {
					groupID := int64(7)
					primary := Account{ID: 1, Platform: platform, Type: AccountTypeAPIKey, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 10,
						GroupIDs: []int64{groupID}, AccountGroups: []AccountGroup{{GroupID: groupID}}}
					backup := primary
					backup.ID, backup.Priority = 2, 25
					loads := map[int64]*AccountLoadInfo{1: {AccountID: 1}, 2: {AccountID: 2}}
					acquired := []int64{}
					concurrency := schedulerTestConcurrencyCache{loadMap: loads, acquiredIDs: &acquired}
					var excluded map[int64]struct{}
					switch scenario {
					case "backup_cooldown":
						end := time.Now().Add(time.Hour)
						backup.RateLimitResetAt = &end
					case "backup_excluded":
						excluded = map[int64]struct{}{2: {}}
					case "same_priority":
						primary.Priority = 25
					case "near_full":
						loads[1].LoadRate, loads[1].CurrentConcurrency = 80, 8
					case "real_full":
						factor := 100
						primary.LoadFactor = &factor
						loads[1].LoadRate, loads[1].CurrentConcurrency = 10, 10
					case "queued":
						loads[1].WaitingCount = 1
					case "load_error":
						concurrency.loadBatchErr = errors.New("unavailable")
					case "load_missing":
						delete(loads, 1)
						concurrency.skipDefaultLoad = true
					case "slot_race":
						concurrency.acquireResults = map[int64]bool{1: false}
					case "cooldown":
						end := time.Now().Add(time.Hour)
						primary.RateLimitResetAt = &end
					case "wrong_model":
						primary.Credentials = map[string]any{"model_mapping": map[string]any{"unrelated": "unrelated"}}
					case "wrong_group":
						primary.GroupIDs, primary.AccountGroups = []int64{99}, []AccountGroup{{GroupID: 99}}
					case "excluded":
						excluded = map[int64]struct{}{1: {}}
					}
					repo := &mockAccountRepoForPlatform{accounts: []Account{primary, backup}, accountsByID: map[int64]*Account{1: &primary, 2: &backup}}
					cache := &mockGatewayCacheForPlatform{sessionBindings: map[string]int64{"session": 2}}
					activity := withIdleActivity(cache)
					switch scenario {
					case "active", "backup_cooldown", "backup_excluded", "unbound":
						activity.idle = false
						if scenario == "unbound" {
							cache.sessionBindings = nil
						}
					case "activity_error":
						activity.err = errors.New("activity unavailable")
					case "activity_revoked":
						activity.denyAfter = 1
					}
					cfg := &config.Config{}
					cfg.Gateway.Scheduling.LoadBatchEnabled = mode == "batch"
					svc := &GatewayService{accountRepo: repo, cache: activity, cfg: cfg,
						groupRepo:          &mockGroupRepoForGateway{groups: map[int64]*Group{groupID: {ID: groupID, Platform: platform, Status: StatusActive}}},
						concurrencyService: NewConcurrencyService(concurrency)}
					model := "claude-sonnet-4-5"
					if platform == PlatformGemini {
						model = "gemini-2.5-flash"
					}
					if platform == PlatformTypeSafe {
						model = "jev-latest"
					}
					var account *Account
					if mode == "metadata" {
						var err error
						account, err = svc.SelectAccountForModelWithExclusions(t.Context(), &groupID, "session", model, excluded)
						require.NoError(t, err)
						require.Empty(t, acquired, "metadata must not reserve generation capacity")
					} else {
						selection, err := svc.SelectAccountWithLoadAwareness(t.Context(), &groupID, "session", model, excluded, "", 0)
						require.NoError(t, err)
						require.True(t, selection.Acquired)
						t.Cleanup(selection.ReleaseFunc)
						account = selection.Account
					}
					wantID := int64(2)
					if scenario == "recovered" || scenario == "backup_cooldown" || scenario == "backup_excluded" || scenario == "unbound" || mode == "metadata" && scenario == "slot_race" {
						wantID = 1
					}
					require.Equal(t, wantID, account.ID)
					require.Equal(t, wantID, cache.sessionBindings["session"])
					if scenario != "backup_cooldown" {
						require.Empty(t, cache.deletedSessions)
					}
				})
			}
		}
	}
}

func TestGatewayPriorityRecoveryRespectsRoutingAndMixedPools(t *testing.T) {
	for _, scenario := range []string{"mixed", "mixed_disabled", "routed", "outside_route", "profit_veto", "privacy", "rpm_warning", "session_limit"} {
		t.Run(scenario, func(t *testing.T) {
			groupID := int64(7)
			primary := Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 10,
				GroupIDs: []int64{groupID}, AccountGroups: []AccountGroup{{GroupID: groupID}}}
			backup := primary
			backup.ID, backup.Priority, backup.Type = 2, 25, AccountTypeAPIKey
			group := &Group{ID: groupID, Platform: PlatformAnthropic, Status: StatusActive}
			ctx := idleSessionContext(t.Context(), &groupID, "session")
			rpm := &openAIRPMTestCache{counts: map[int64]int{1: 8}}
			switch scenario {
			case "mixed", "mixed_disabled":
				primary.Platform = PlatformAntigravity
				primary.Extra = map[string]any{"mixed_scheduling": scenario == "mixed"}
			case "routed", "outside_route":
				group.ModelRoutingEnabled = true
				group.ModelRouting = map[string][]int64{"claude-sonnet-4-5": {2}}
				if scenario == "routed" {
					group.ModelRouting["claude-sonnet-4-5"] = []int64{1, 2}
				}
			case "profit_veto":
				rate := 2.0
				primary.RateMultiplier = &rate
				ctx = context.WithValue(ctx, openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: groupID, platform: PlatformAnthropic, threshold: 0.5})
			case "privacy":
				group.RequirePrivacySet = true
				primary.Platform = PlatformAntigravity
				primary.Extra = map[string]any{"mixed_scheduling": true}
			case "rpm_warning":
				primary.Extra = map[string]any{"base_rpm": 10}
			case "session_limit":
				primary.Extra = map[string]any{"max_sessions": 1}
			}
			repo := &mockAccountRepoForPlatform{accounts: []Account{primary, backup}, accountsByID: map[int64]*Account{1: &primary, 2: &backup}}
			cache := &mockGatewayCacheForPlatform{sessionBindings: map[string]int64{"session": 2}}
			svc := &GatewayService{accountRepo: repo, cache: withIdleActivity(cache), rpmCache: rpm,
				groupRepo:          &mockGroupRepoForGateway{groups: map[int64]*Group{groupID: group}},
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{})}
			if scenario == "session_limit" {
				svc.sessionLimitCache = &priorityFullSessionCache{}
			}
			selection := svc.tryPriorityStickyFailback(ctx, &groupID, PlatformAnthropic, false, "session", "claude-sonnet-4-5", nil, 2, true)
			if scenario == "mixed" || scenario == "routed" {
				require.NotNil(t, selection)
				t.Cleanup(selection.ReleaseFunc)
				require.Equal(t, int64(1), selection.Account.ID)
				require.Equal(t, int64(1), cache.sessionBindings["session"])
			} else {
				require.Nil(t, selection)
				require.Equal(t, int64(2), cache.sessionBindings["session"])
			}
		})
	}
}

type priorityFullSessionCache struct{ SessionLimitCache }

func (*priorityFullSessionCache) RegisterSession(context.Context, int64, string, int, time.Duration) (bool, error) {
	return false, nil
}

func TestCompatiblePriorityRecoveryAcrossPlatformsAndMedia(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo} {
		for _, advanced := range []bool{false, true} {
			for _, kind := range []string{"text", "media"} {
				if kind == "media" && platform != PlatformOpenAI && platform != PlatformGrok {
					continue
				}
				t.Run(platform+"/"+kind+"/advanced="+strconv.FormatBool(advanced), func(t *testing.T) {
					resetOpenAIAdvancedSchedulerSettingCacheForTest()
					defer resetOpenAIAdvancedSchedulerSettingCacheForTest()
					accounts := []Account{
						{ID: 1, Platform: platform, Type: AccountTypeAPIKey, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 10},
						{ID: 2, Platform: platform, Type: AccountTypeAPIKey, Priority: 25, Status: StatusActive, Schedulable: true, Concurrency: 100},
					}
					cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:session": 2}}
					cfg := &config.Config{}
					cfg.Gateway.OpenAIWS.LBTopK = 1
					svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}, cache: withIdleActivity(cache), cfg: cfg,
						concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{})}
					if advanced {
						svc.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService("true")
					}
					model := "gpt-5.1"
					if platform == PlatformDeepseek {
						model = "deepseek-v4-pro"
					}
					if platform == PlatformGrok {
						model = "grok-4.3"
						if kind == "media" {
							model = "grok-imagine-video"
						}
					}
					var selection *AccountSelectionResult
					var decision OpenAIAccountScheduleDecision
					var err error
					if kind == "media" && platform == PlatformOpenAI {
						selection, decision, err = svc.SelectAccountWithSchedulerForImages(t.Context(), nil, "session", "gpt-image-1", nil, OpenAIImagesCapabilityAPIKey)
					} else {
						capability := OpenAIEndpointCapabilityChatCompletions
						if kind == "media" && platform == PlatformGrok {
							capability = OpenAIEndpointCapabilityGrokMediaGeneration
						}
						selection, decision, err = svc.SelectAccountWithSchedulerForCapability(t.Context(), nil, "", "session", model, nil,
							OpenAIUpstreamTransportAny, capability, false, false, kind == "text", platform)
					}
					require.NoError(t, err)
					require.NotNil(t, selection)
					t.Cleanup(selection.ReleaseFunc)
					require.Equal(t, int64(1), selection.Account.ID)
					require.Equal(t, openAIAccountScheduleLayerPriorityFailback, decision.Layer)
					require.Equal(t, int64(1), cache.sessionBindings["openai:session"])
					// Verify free selection too: a backup's score/soft affinity
					// cannot remove the primary or its overflow with Top-K=1.
					primary, backup := priorityCandidate(1, 0.1, 60), priorityCandidate(2, 0.1, 0)
					primary.account.Platform, backup.account.Platform = platform, platform
					primary.account.Priority, backup.account.Priority = 1, 25
					primary.score, backup.score = 1, 999
					order := (&defaultOpenAIAccountScheduler{service: svc}).buildOpenAISelectionOrder(OpenAIAccountScheduleRequest{Platform: platform},
						openAIAccountLoadPlan{topK: 1, includeOverflowFallback: true, candidates: []openAIAccountCandidateScore{backup, primary}})
					require.Equal(t, int64(1), order[0].account.ID)
					require.Len(t, order, 2)
				})
			}
		}
	}
}

func TestPriorityRecoverySlotFreeCompatibilityAPIs(t *testing.T) {
	for _, provider := range []string{"gemini", "openai_token_count"} {
		t.Run(provider, func(t *testing.T) {
			platform, model, key := PlatformGemini, "gemini-2.5-flash", "gemini:session"
			if provider == "openai_token_count" {
				platform, model, key = PlatformOpenAI, "gpt-5.1", "openai:session"
			}
			primary := Account{ID: 1, Platform: platform, Type: AccountTypeAPIKey, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 10}
			backup := primary
			backup.ID, backup.Priority = 2, 25
			acquired := []int64{}
			concurrency := NewConcurrencyService(schedulerTestConcurrencyCache{acquiredIDs: &acquired})
			cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{key: 2}}
			var account *Account
			var err error
			if provider == "gemini" {
				repo := &mockAccountRepoForPlatform{accounts: []Account{primary, backup}, accountsByID: map[int64]*Account{1: &primary, 2: &backup}}
				svc := &GeminiMessagesCompatService{accountRepo: repo, cache: withIdleActivity(cache), concurrencyService: concurrency}
				account, err = svc.SelectAccountForModel(t.Context(), nil, "session", model)
			} else {
				svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{primary, backup}}, cache: withIdleActivity(cache), concurrencyService: concurrency}
				account, err = svc.SelectAccountForTokenCount(t.Context(), nil, "session", model, OpenAIEndpointCapabilityChatCompletions, platform)
			}
			require.NoError(t, err)
			require.Equal(t, int64(1), account.ID)
			require.Equal(t, int64(1), cache.sessionBindings[key])
			require.Empty(t, acquired, "slot-free APIs must not consume generation concurrency")
		})
	}
}

func TestGatewayPriorityRecoveryRechecksAccountChanges(t *testing.T) {
	for _, change := range []string{"demoted", "group_removed", "model_removed", "concurrency_changed", "disabled"} {
		t.Run(change, func(t *testing.T) {
			primary := Account{ID: 1, Platform: PlatformGemini, Type: AccountTypeAPIKey, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 10}
			backup := primary
			backup.ID, backup.Priority = 2, 25
			fresh := primary
			switch change {
			case "demoted":
				fresh.Priority = 30
			case "group_removed":
				fresh.GroupIDs = []int64{99}
			case "model_removed":
				fresh.Credentials = map[string]any{"model_mapping": map[string]any{"other": "other"}}
			case "concurrency_changed":
				fresh.Concurrency = 1
			case "disabled":
				fresh.Schedulable = false
			}
			cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"session": 2}}
			acquired, released := []int64{}, []int64{}
			repo := &mockAccountRepoForPlatform{accounts: []Account{primary, backup}, accountsByID: map[int64]*Account{1: &fresh, 2: &backup}}
			svc := &GatewayService{accountRepo: repo, cache: withIdleActivity(cache), concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquiredIDs: &acquired, releasedIDs: &released})}
			require.Nil(t, svc.tryPriorityStickyFailback(idleSessionContext(t.Context(), nil, "session"), nil, PlatformGemini, false, "session", "gemini-2.5-flash", nil, 2, true))
			require.Equal(t, []int64{1}, acquired)
			require.Equal(t, acquired, released)
			require.Equal(t, int64(2), cache.sessionBindings["session"])
		})
	}
}

func TestPriorityRecoveryPreservesMediaCapabilitiesAndOwnership(t *testing.T) {
	for _, scenario := range []string{"image_apikey_required", "transport_required", "video_owner"} {
		t.Run(scenario, func(t *testing.T) {
			primary := Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 10}
			backup := primary
			backup.ID, backup.Priority = 2, 25
			if scenario == "image_apikey_required" {
				primary.Type = AccountTypeOAuth
			}
			if scenario == "video_owner" {
				primary.Platform, backup.Platform = PlatformGrok, PlatformGrok
			}
			cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:session": 2}}
			acquired := []int64{}
			svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{primary, backup}}, cache: withIdleActivity(cache),
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquiredIDs: &acquired})}
			req := OpenAIAccountScheduleRequest{Platform: PlatformOpenAI, SessionHash: "session", RequestedModel: "gpt-5.1"}
			switch scenario {
			case "image_apikey_required":
				req.RequiredImageCapability = OpenAIImagesCapabilityAPIKey
			case "transport_required":
				req.RequiredTransport = OpenAIUpstreamTransportResponsesWebsocketV2
			case "video_owner":
				selection, _, err := svc.SelectMediaVideoRequestAccount(t.Context(), nil, "session", 2, "grok-imagine-video", PlatformGrok)
				require.NoError(t, err)
				require.Equal(t, int64(2), selection.Account.ID)
				t.Cleanup(selection.ReleaseFunc)
				require.Equal(t, []int64{2}, acquired)
				return
			}
			require.Nil(t, (&defaultOpenAIAccountScheduler{service: svc}).tryPriorityStickyFailback(idleSessionContext(t.Context(), nil, "openai:session"), req, &backup, true))
			require.Empty(t, acquired)
			require.Equal(t, int64(2), cache.sessionBindings["openai:session"])
		})
	}
}

func TestPriorityRecoveryAcrossAccountTypes(t *testing.T) {
	for _, kind := range []string{AccountTypeOAuth, AccountTypeSetupToken, AccountTypeAPIKey, AccountTypeBedrock, AccountTypeServiceAccount} {
		t.Run(kind, func(t *testing.T) {
			primary := Account{ID: 1, Platform: PlatformAnthropic, Type: kind, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 10}
			backup := primary
			backup.ID, backup.Priority, backup.Type = 2, 25, AccountTypeAPIKey
			repo := &mockAccountRepoForPlatform{accounts: []Account{primary, backup}, accountsByID: map[int64]*Account{1: &primary, 2: &backup}}
			cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"session": 2}}
			svc := &GatewayService{accountRepo: repo, cache: withIdleActivity(cache), concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{})}
			selection := svc.tryPriorityStickyFailback(idleSessionContext(t.Context(), nil, "session"), nil, PlatformAnthropic, false, "session", "claude-sonnet-4-5-20250929", nil, 2, true)
			require.NotNil(t, selection)
			t.Cleanup(selection.ReleaseFunc)
			require.Equal(t, int64(1), selection.Account.ID)
		})
	}
}

func TestGeminiAIStudioPriorityWithinCompatibleAccounts(t *testing.T) {
	for _, scenario := range []string{"oauth", "ai_studio_oauth", "unknown_scopes"} {
		t.Run(scenario, func(t *testing.T) {
			primary := Account{ID: 1, Platform: PlatformGemini, Type: AccountTypeOAuth, Priority: 1, Status: StatusActive, Schedulable: true}
			if scenario != "oauth" {
				primary.Credentials = map[string]any{"project_id": "project"}
			}
			if scenario == "ai_studio_oauth" {
				primary.Credentials["oauth_type"] = "ai_studio"
			}
			backup := Account{ID: 2, Platform: PlatformGemini, Type: AccountTypeAPIKey, Priority: 25, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"api_key": "test"}}
			repo := &mockAccountRepoForPlatform{accounts: []Account{backup, primary}}
			svc := &GeminiMessagesCompatService{accountRepo: repo}
			account, err := svc.SelectAccountForAIStudioEndpoints(t.Context(), nil)
			require.NoError(t, err)
			want := int64(1)
			if scenario == "unknown_scopes" {
				want = 2
			}
			require.Equal(t, want, account.ID)
		})
	}
}
