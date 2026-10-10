package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestOpenAIPriorityStickyFailbackAcrossSchedulers(t *testing.T) {
	for _, mode := range []string{"legacy", "legacy_batch", "advanced", "experimental"} {
		for _, scenario := range []string{"recovered", "same_priority", "full", "near_full", "queued", "load_error", "load_missing", "slot_race", "cooldown", "excluded", "wrong_group", "wrong_model", "rpm_full", "rpm_warning"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				resetOpenAIAdvancedSchedulerSettingCacheForTest()
				defer resetOpenAIAdvancedSchedulerSettingCacheForTest()
				groupID := int64(3)
				accounts := []Account{
					{ID: 218, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 10, Priority: 1, GroupIDs: []int64{groupID}},
					{ID: 10, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 100, Priority: 25, GroupIDs: []int64{groupID}},
				}
				loads := map[int64]*AccountLoadInfo{218: {AccountID: 218}, 10: {AccountID: 10}}
				acquired := []int64{}
				concurrency := schedulerTestConcurrencyCache{loadMap: loads, acquiredIDs: &acquired}
				var excluded map[int64]struct{}
				rpm := &openAIRPMTestCache{counts: map[int64]int{}}
				switch scenario {
				case "same_priority":
					accounts[0].Priority = 25
				case "full":
					loads[218].CurrentConcurrency, loads[218].LoadRate = 10, 100
				case "near_full":
					loads[218].CurrentConcurrency, loads[218].LoadRate = 8, 80
				case "queued":
					loads[218].WaitingCount = 1
				case "load_error":
					concurrency.loadBatchErr = errors.New("load unavailable")
				case "load_missing":
					delete(loads, 218)
					concurrency.skipDefaultLoad = true
				case "slot_race":
					concurrency.acquireResults = map[int64]bool{218: false}
				case "cooldown":
					end := time.Now().Add(time.Hour)
					accounts[0].RateLimitResetAt = &end
				case "excluded":
					excluded = map[int64]struct{}{218: {}}
				case "wrong_group":
					accounts[0].GroupIDs = []int64{99}
				case "wrong_model":
					accounts[0].Credentials = map[string]any{"model_mapping": map[string]any{"other-model": "other-model"}}
				case "rpm_full", "rpm_warning":
					accounts[0].Extra = map[string]any{"base_rpm": 10}
					rpm.counts[218] = 10
					if scenario == "rpm_warning" {
						rpm.counts[218] = 8
					}
				}
				cfg := &config.Config{}
				cfg.Gateway.Scheduling.LoadBatchEnabled = mode == "legacy_batch"
				cfg.Gateway.OpenAIWS.LBTopK = 1
				cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:backup-session": 10}}
				svc := &OpenAIGatewayService{
					cfg: cfg, cache: cache, rpmCache: rpm,
					accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
					concurrencyService: NewConcurrencyService(concurrency),
				}
				if mode == "advanced" {
					svc.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService("true")
				}
				if mode == "experimental" {
					priority := DefaultPrioritySchedulingConfig()
					priority.Enabled = true
					svc.settingService = &SettingService{settingRepo: &prioritySettingStub{}}
					require.NoError(t, svc.settingService.SavePrioritySchedulingConfig(context.Background(), priority))
				}
				selection, decision, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "", "backup-session", "gpt-5.1", excluded, OpenAIUpstreamTransportAny, false)
				require.NoError(t, err)
				require.NotNil(t, selection)
				require.True(t, selection.Acquired)
				t.Cleanup(selection.ReleaseFunc)
				wantID := int64(10)
				if scenario == "recovered" {
					wantID = 218
					require.Equal(t, openAIAccountScheduleLayerPriorityFailback, decision.Layer)
					require.False(t, decision.StickySessionHit)
				} else {
					require.Equal(t, openAIAccountScheduleLayerSessionSticky, decision.Layer)
					require.True(t, decision.StickySessionHit)
				}
				require.Equal(t, wantID, selection.Account.ID)
				require.Equal(t, wantID, cache.sessionBindings["openai:backup-session"], "only acquired, compatible primary capacity may replace the durable binding")
				require.Empty(t, cache.deletedSessions, "failback must never clear the original binding speculatively")
				if scenario == "slot_race" {
					require.Equal(t, []int64{218, 10}, acquired)
				}
			})
		}
	}
}

func TestOpenAIPriorityFailbackPreservesHardBindings(t *testing.T) {
	for _, scenario := range []string{"response_owner", "task_owner", "escape_disabled"} {
		t.Run(scenario, func(t *testing.T) {
			sticky := &Account{ID: 10, Priority: 25}
			scheduler := &defaultOpenAIAccountScheduler{service: &OpenAIGatewayService{concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{})}}
			req := OpenAIAccountScheduleRequest{Platform: PlatformOpenAI, SessionHash: "session", UseUpstreamTokenCost: true}
			switch scenario {
			case "response_owner":
				req.PreviousResponseID = "resp_owner"
			case "task_owner":
				req.PreserveStickyBinding = true
			case "escape_disabled":
				req.DisableStickyEscape = true
			}
			require.Nil(t, scheduler.tryPriorityStickyFailback(context.Background(), req, sticky, true))
		})
	}
}

func TestOpenAIExplicitPriorityPrecedesScoresAndLoad(t *testing.T) {
	for _, experimental := range []bool{false, true} {
		t.Run(fmt.Sprintf("experimental=%t", experimental), func(t *testing.T) {
			primary, backup := priorityCandidate(218, 0.1, 60), priorityCandidate(10, 0.1, 0)
			primary.account.Priority, backup.account.Priority = 1, 25
			primary.score, backup.score = 210, 490
			scheduler := &defaultOpenAIAccountScheduler{service: &OpenAIGatewayService{}}
			req := OpenAIAccountScheduleRequest{Platform: PlatformOpenAI, UseUpstreamTokenCost: true, StickyWeighted: true, StickyAccountID: 10}
			plan := openAIAccountLoadPlan{priorityScheduling: experimental, topK: 1, includeOverflowFallback: true, candidates: []openAIAccountCandidateScore{backup, primary}}
			for i := range 100 {
				req.SessionHash = fmt.Sprint(i)
				order := scheduler.buildOpenAISelectionOrder(req, plan)
				require.Len(t, order, 2, "backup must remain available even with Top-K=1")
				require.Equal(t, int64(218), order[0].account.ID, "lower load, a higher score, and sticky weighting cannot promote a backup")
			}
			plan.candidates[1].priorityUnhealthy = true
			order := scheduler.buildOpenAISelectionOrder(req, plan)
			require.Equal(t, int64(10), order[0].account.ID, "priority must not force traffic onto known unhealthy capacity")
		})
	}
}

func TestOpenAIExplicitPriorityFallsBackOnSlotRace(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	defer resetOpenAIAdvancedSchedulerSettingCacheForTest()
	accounts := []Account{
		{ID: 218, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 10, Priority: 1},
		{ID: 10, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 100, Priority: 25},
	}
	acquired := []int64{}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.LBTopK = 1
	svc := &OpenAIGatewayService{
		cfg: cfg, accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts},
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquiredIDs: &acquired, acquireResults: map[int64]bool{218: false, 10: true}}),
	}
	selection, _, err := svc.SelectAccountWithScheduler(context.Background(), nil, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	t.Cleanup(selection.ReleaseFunc)
	require.Equal(t, int64(10), selection.Account.ID)
	require.Equal(t, []int64{218, 10}, acquired)
}

type priorityFailbackRecheckRepo struct {
	schedulerTestOpenAIAccountRepo
	fresh Account
}

func (r priorityFailbackRecheckRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	if id == r.fresh.ID {
		return &r.fresh, nil
	}
	return r.schedulerTestOpenAIAccountRepo.GetByID(ctx, id)
}

func TestOpenAIPriorityFailbackRechecksBeforeUpdatingBinding(t *testing.T) {
	for _, scenario := range []string{"priority_demoted", "rate_limited", "model_removed"} {
		t.Run(scenario, func(t *testing.T) {
			primary := Account{ID: 218, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 10, Priority: 1}
			backup := Account{ID: 10, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 100, Priority: 25}
			fresh := primary
			switch scenario {
			case "priority_demoted":
				fresh.Priority = 30
			case "rate_limited":
				end := time.Now().Add(time.Hour)
				fresh.RateLimitResetAt = &end
			case "model_removed":
				fresh.Credentials = map[string]any{"model_mapping": map[string]any{"other-model": "other-model"}}
			}
			cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:backup-session": backup.ID}}
			acquired, released := []int64{}, []int64{}
			svc := &OpenAIGatewayService{
				cache:              cache,
				accountRepo:        priorityFailbackRecheckRepo{schedulerTestOpenAIAccountRepo{accounts: []Account{primary, backup}}, fresh},
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquiredIDs: &acquired, releasedIDs: &released}),
			}
			scheduler := &defaultOpenAIAccountScheduler{service: svc}
			selection := scheduler.tryPriorityStickyFailback(context.Background(), OpenAIAccountScheduleRequest{
				Platform: PlatformOpenAI, SessionHash: "backup-session", RequestedModel: "gpt-5.1", UseUpstreamTokenCost: true,
			}, &backup, true)
			require.Nil(t, selection)
			require.Equal(t, []int64{primary.ID}, acquired)
			require.Equal(t, acquired, released, "fresh checks must release speculative capacity")
			require.Equal(t, backup.ID, cache.sessionBindings["openai:backup-session"])
			require.Empty(t, cache.deletedSessions)
		})
	}
}
