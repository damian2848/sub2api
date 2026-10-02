package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

const (
	channelCandyMaxProbes             = 64
	ChannelMonitorV2CandyHistoryLimit = 100
	ChannelMonitorV2CandyRetention    = 30 * 24 * time.Hour
)

// Probe consumption is an upstream cost snapshot and never a customer debit.
type ChannelMonitorProbeUsage struct {
	Source                string   `json:"source"`
	RequestCount          int64    `json:"request_count"`
	InputTokens           int64    `json:"input_tokens"`
	OutputTokens          int64    `json:"output_tokens"`
	CacheReadTokens       int64    `json:"cache_read_tokens"`
	CacheCreationTokens   int64    `json:"cache_creation_tokens"`
	CacheCreation5mTokens int64    `json:"cache_creation_5m_tokens"`
	CacheCreation1hTokens int64    `json:"cache_creation_1h_tokens"`
	ReasoningTokens       int64    `json:"reasoning_tokens"`
	CostUSD               *float64 `json:"cost_usd"`
	CostIncomplete        bool     `json:"cost_incomplete"`
	UsageIncomplete       bool     `json:"usage_incomplete"`
}

func (u *ChannelMonitorProbeUsage) Add(other *ChannelMonitorProbeUsage) {
	u.Source = "probe"
	if other == nil {
		u.CostIncomplete, u.UsageIncomplete = true, true
		return
	}
	u.RequestCount += other.RequestCount
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CacheReadTokens += other.CacheReadTokens
	u.CacheCreationTokens += other.CacheCreationTokens
	u.CacheCreation5mTokens += other.CacheCreation5mTokens
	u.CacheCreation1hTokens += other.CacheCreation1hTokens
	u.ReasoningTokens += other.ReasoningTokens
	u.CostIncomplete = u.CostIncomplete || other.CostIncomplete
	u.UsageIncomplete = u.UsageIncomplete || other.UsageIncomplete
	if other.CostUSD != nil {
		// A skipped request's zero cannot fill unknown pricing from an earlier call.
		if u.CostUSD == nil && u.CostIncomplete && other.RequestCount == 0 && *other.CostUSD == 0 {
			return
		}
		cost := *other.CostUSD
		if u.CostUSD != nil {
			cost += *u.CostUSD
		}
		// Usage snapshots may be projected into several groups. Never mutate
		// a cost pointer shared with the original sample or another projection.
		u.CostUSD = &cost
	} else {
		u.CostIncomplete = true
	}
}

func AddChannelMonitorProbeUsage(total, sample *ChannelMonitorProbeUsage) *ChannelMonitorProbeUsage {
	if total == nil {
		total = &ChannelMonitorProbeUsage{Source: "probe"}
	}
	total.Add(sample)
	return total
}

func redactChannelMonitorProbeUsage(usage *ChannelMonitorProbeUsage) *ChannelMonitorProbeUsage {
	if usage == nil {
		return nil
	}
	return &ChannelMonitorProbeUsage{Source: usage.Source, CostIncomplete: usage.CostIncomplete, UsageIncomplete: usage.UsageIncomplete}
}

type ChannelMonitorProbeAttempt struct {
	AccountID      int64                     `json:"account_id,omitempty"`
	Platform       string                    `json:"platform"`
	RequestedModel string                    `json:"requested_model"`
	UpstreamModel  string                    `json:"upstream_model"`
	Usage          *ChannelMonitorProbeUsage `json:"usage"`
}

type ChannelMonitorV2CandyProbe struct {
	GroupID         int64  `json:"group_id"`
	Enabled         bool   `json:"enabled"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
	IntervalMinutes int    `json:"interval_minutes"`
}

func (p ChannelMonitorV2CandyProbe) key() string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("candy-v1:%s:%s:%d", p.Model, p.ReasoningEffort, p.IntervalMinutes))))
}

type ChannelMonitorV2CandyResult struct {
	Source         string                       `json:"source"`
	ID             int64                        `json:"-"`
	GroupID        int64                        `json:"-"`
	ConfigKey      string                       `json:"-"`
	CheckedAt      time.Time                    `json:"checked_at"`
	Verdict        string                       `json:"verdict"`
	LatencyMs      int64                        `json:"latency_ms"`
	AnswerPreview  string                       `json:"answer_preview,omitempty"`
	Reason         string                       `json:"reason,omitempty"`
	Platform       string                       `json:"platform,omitempty"`
	AccountID      int64                        `json:"account_id,omitempty"`
	RequestedModel string                       `json:"requested_model,omitempty"`
	UpstreamModel  string                       `json:"upstream_model,omitempty"`
	AttemptCount   int                          `json:"attempt_count"`
	Usage          *ChannelMonitorProbeUsage    `json:"usage,omitempty"`
	Attempts       []ChannelMonitorProbeAttempt `json:"attempts,omitempty"`
}

type ChannelMonitorV2CandyHistory struct {
	GroupID         int64                         `json:"group_id"`
	Platform        string                        `json:"platform,omitempty"`
	Model           string                        `json:"model"`
	ReasoningEffort string                        `json:"reasoning_effort"`
	IntervalMinutes int                           `json:"interval_minutes"`
	Results         []ChannelMonitorV2CandyResult `json:"results"`
	Usage           *ChannelMonitorProbeUsage     `json:"usage,omitempty"`
}

type ChannelMonitorV2CandyMultiRepository interface {
	CandyHistoryMany(context.Context, map[int64][]string, time.Time) ([]ChannelMonitorV2CandyResult, error)
}

type ChannelMonitorV2CandyRepository interface {
	ClaimCandyProbe(context.Context, ChannelMonitorV2CandyProbe, string, time.Time, int) (int64, error)
	FinishCandyProbe(context.Context, ChannelMonitorV2CandyResult) error
	CandyHistory(context.Context, map[int64]string, time.Time) ([]ChannelMonitorV2CandyResult, error)
	PruneCandyHistory(context.Context, time.Time) error
}

type ChannelMonitorV2CandyService struct {
	config   ChannelMonitorV2Repository
	repo     ChannelMonitorV2CandyRepository
	groups   *PelicanGroupTestService
	settings channelMonitorRuntimeReader
	tick     sync.Mutex
	now      func() time.Time
}

func newChannelMonitorV2CandyService(repo ChannelMonitorV2Repository, groups *PelicanGroupTestService, settings channelMonitorRuntimeReader) *ChannelMonitorV2CandyService {
	store, ok := repo.(ChannelMonitorV2CandyRepository)
	if !ok || groups == nil {
		return nil
	}
	return &ChannelMonitorV2CandyService{config: repo, repo: store, groups: groups, settings: settings, now: time.Now}
}

func normalizeChannelMonitorV2CandyProbes(probes []ChannelMonitorV2CandyProbe) error {
	if len(probes) > channelCandyMaxProbes {
		return fmt.Errorf("%w: at most %d candy probes", ErrChannelMonitorV2InvalidConfig, channelCandyMaxProbes)
	}
	seen := map[string]bool{}
	for i := range probes {
		p := &probes[i]
		if p.GroupID <= 0 {
			return fmt.Errorf("%w: invalid candy group", ErrChannelMonitorV2InvalidConfig)
		}
		p.Model = strings.TrimSpace(p.Model)
		if p.Model == "" || len(p.Model) > 100 {
			return fmt.Errorf("%w: candy model is required (maximum 100 bytes)", ErrChannelMonitorV2InvalidConfig)
		}
		if p.ReasoningEffort == "" {
			p.ReasoningEffort = "medium"
		}
		p.ReasoningEffort = normalizePelicanReasoningEffort(p.ReasoningEffort)
		if p.ReasoningEffort == "" {
			return fmt.Errorf("%w: invalid candy reasoning effort", ErrChannelMonitorV2InvalidConfig)
		}
		identity := fmt.Sprintf("%d:%s:%s", p.GroupID, p.Model, p.ReasoningEffort)
		if seen[identity] {
			return fmt.Errorf("%w: duplicate candy group/model/effort", ErrChannelMonitorV2InvalidConfig)
		}
		seen[identity] = true
		if p.IntervalMinutes == 0 {
			p.IntervalMinutes = 1
		}
		if p.IntervalMinutes < 1 || p.IntervalMinutes > 1440 {
			return fmt.Errorf("%w: candy interval must be 1-1440 minutes", ErrChannelMonitorV2InvalidConfig)
		}
	}
	return nil
}

func (s *ChannelMonitorV2CandyService) validateGroups(ctx context.Context, probes []ChannelMonitorV2CandyProbe) error {
	for _, p := range probes {
		group, err := s.groups.groups.GetByID(ctx, p.GroupID)
		if err != nil || group == nil {
			return fmt.Errorf("%w: candy group %d is unavailable", ErrChannelMonitorV2InvalidConfig, p.GroupID)
		}
	}
	return nil
}

// RunDue uses the same group scheduler, model mapping, account concurrency and
// failover policy as /admin/pelican-tests. A wrong answer is retained and never
// retried on another account to obtain a passing sample. It does not bill a user,
// publish a showcase item, apply answer-based account actions, or enter passive
// usage totals. Authentication/rate-limit health handling remains in the shared
// account-test and gateway paths.
func (s *ChannelMonitorV2CandyService) RunDue(ctx context.Context, now time.Time) {
	if s == nil || !s.tick.TryLock() {
		return
	}
	defer s.tick.Unlock()
	if err := s.repo.PruneCandyHistory(ctx, now); err != nil {
		logger.LegacyPrintf("service.channel_monitor_v2", "candy retention failed: %v", err)
	}
	if s.settings == nil || !s.settings.GetChannelMonitorRuntime(ctx).QualityProbesAllowed() {
		return
	}
	cfg, err := s.config.GetConfig(ctx)
	if err != nil || cfg == nil || !cfg.Enabled {
		return
	}
	jobs := make(chan ChannelMonitorV2CandyProbe)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for probe := range jobs {
				s.runOne(ctx, probe, cfg, now)
			}
		}()
	}
	for _, probe := range cfg.CandyProbes {
		if !probe.Enabled || (len(cfg.GroupIDs) > 0 && !slices.Contains(cfg.GroupIDs, probe.GroupID)) {
			continue
		}
		select {
		case jobs <- probe:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		}
	}
	close(jobs)
	wg.Wait()
}

func (s *ChannelMonitorV2CandyService) runOne(ctx context.Context, probe ChannelMonitorV2CandyProbe, cfg *ChannelMonitorV2Config, tick time.Time) {
	if ctx.Err() != nil || !s.settings.GetChannelMonitorRuntime(ctx).QualityProbesAllowed() {
		return
	}
	group, err := s.groups.groups.GetByID(ctx, probe.GroupID)
	if err != nil || group == nil || group.Status != StatusActive {
		return
	}
	platformEnabled := false
	for _, p := range cfg.Platforms {
		if p.Enabled && (p.Platform == group.Platform || group.Platform == PlatformComposite) {
			platformEnabled = true
			break
		}
	}
	if !platformEnabled {
		return
	}
	slot := tick.UTC().Truncate(time.Duration(probe.IntervalMinutes) * time.Minute)
	id, err := s.repo.ClaimCandyProbe(ctx, probe, probe.key(), slot, cfg.Version)
	if err != nil {
		logger.LegacyPrintf("service.channel_monitor_v2", "candy claim group=%d failed: %v", probe.GroupID, err)
		return
	}
	if id == 0 {
		return
	}
	result := ChannelMonitorV2CandyResult{ID: id, GroupID: probe.GroupID, ConfigKey: probe.key(), Source: "probe", CheckedAt: s.now(), Verdict: "error", Reason: "interrupted"}
	defer func() {
		if recover() != nil {
			result.Verdict = "error"
			result.Reason = "probe_failed"
		}
		saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.repo.FinishCandyProbe(saveCtx, result); err != nil {
			logger.LegacyPrintf("service.channel_monitor_v2", "candy save group=%d failed: %v", probe.GroupID, err)
		}
	}()
	runCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	plan := &PelicanGroupTestPlan{GroupID: probe.GroupID, ModelID: probe.Model, PelicanConfig: &PelicanTestConfig{QuestionKind: "candy", Prompt: CandyPrompt, ReasoningEffort: probe.ReasoningEffort, ParallelCount: 1, ModelID: probe.Model}}
	sample := s.groups.runSample(runCtx, plan, group)
	result.LatencyMs = s.now().Sub(result.CheckedAt).Milliseconds()
	result.RequestedModel = probe.Model
	if sample != nil {
		result.Platform = sample.Platform
		result.AccountID = sample.AccountID
		result.UpstreamModel = sample.UpstreamModel
		result.AttemptCount = sample.AttemptCount
		result.Usage = sample.Usage
		result.Attempts = sample.ProbeAttempts
	}
	if runCtx.Err() != nil {
		result.Reason = "timeout"
		return
	}
	if sample == nil {
		result.Reason = "probe_failed"
		return
	}
	answer := strings.TrimSpace(sample.ResponseText)
	preview := []rune(answer)
	result.AnswerPreview = string(preview[:min(len(preview), 512)])
	if sample.Status != "success" && !strings.HasPrefix(sample.ErrorMessage, "answer_mismatch:") {
		result.Reason = "probe_failed"
		return
	}
	if answer == "" {
		result.Reason = "empty_answer"
		return
	}
	result.Reason = ""
	result.Verdict = "incorrect"
	if CandyAnswerCorrect(answer) {
		result.Verdict = "correct"
	}
}

func (s *ChannelMonitorV2CandyService) attachHistory(ctx context.Context, matrix *ChannelMonitorV2Matrix, cfg *ChannelMonitorV2Config, admin bool) error {
	probes := map[int64][]ChannelMonitorV2CandyProbe{}
	for _, probe := range cfg.CandyProbes {
		if probe.Enabled {
			probes[probe.GroupID] = append(probes[probe.GroupID], probe)
		}
	}
	groupPlatforms := map[int64]string{}
	configs := map[int64][]string{}
	for _, row := range matrix.Items {
		if row.GroupID == nil || len(probes[*row.GroupID]) == 0 {
			continue
		}
		id := *row.GroupID
		if _, loaded := groupPlatforms[id]; !loaded {
			group, err := s.groups.groups.GetByID(ctx, id)
			if err != nil || group == nil {
				continue
			}
			groupPlatforms[id] = group.Platform
		}
		for _, probe := range probes[id] {
			if candyProbeMatchesRow(probe, groupPlatforms[id], row) && !slices.Contains(configs[id], probe.key()) {
				configs[id] = append(configs[id], probe.key())
			}
		}
	}
	if len(configs) == 0 {
		return nil
	}
	// Only groups and models already admitted by the matrix's server-side scope are read.
	history, err := s.historyMany(ctx, configs, s.now().Add(-ChannelMonitorV2CandyRetention))
	if err != nil {
		return err
	}
	byConfig := map[int64]map[string][]ChannelMonitorV2CandyResult{}
	for _, result := range history {
		if !slices.Contains(configs[result.GroupID], result.ConfigKey) {
			continue
		}
		if result.Platform == "" {
			result.Platform = groupPlatforms[result.GroupID]
		}
		if byConfig[result.GroupID] == nil {
			byConfig[result.GroupID] = map[string][]ChannelMonitorV2CandyResult{}
		}
		byConfig[result.GroupID][result.ConfigKey] = append(byConfig[result.GroupID][result.ConfigKey], result)
	}
	for i := range matrix.Items {
		row := &matrix.Items[i]
		row.Candy, row.CandyHistories = nil, nil
		if row.GroupID == nil {
			continue
		}
		id := *row.GroupID
		for _, probe := range probes[id] {
			if !candyProbeMatchesRow(probe, groupPlatforms[id], *row) {
				continue
			}
			item := &ChannelMonitorV2CandyHistory{GroupID: id, Platform: row.Platform, Model: probe.Model, ReasoningEffort: probe.ReasoningEffort, IntervalMinutes: probe.IntervalMinutes, Results: []ChannelMonitorV2CandyResult{}}
			for _, result := range byConfig[id][probe.key()] {
				if row.Platform != "" && result.Platform != row.Platform {
					continue
				}
				if result.RequestedModel != "" && result.RequestedModel != probe.Model {
					continue
				}
				item.Usage = AddChannelMonitorProbeUsage(item.Usage, result.Usage)
				if !admin {
					result.AnswerPreview, result.Reason, result.UpstreamModel = "", "", ""
					result.AccountID, result.AttemptCount, result.Attempts = 0, 0, nil
					result.Usage = redactChannelMonitorProbeUsage(result.Usage)
				}
				item.Results = append(item.Results, result)
			}
			if !admin {
				item.Usage = redactChannelMonitorProbeUsage(item.Usage)
			}
			row.CandyHistories = append(row.CandyHistories, item)
			if row.Candy == nil {
				row.Candy = item
			}
		}
	}
	return nil
}

func candyProbeMatchesRow(probe ChannelMonitorV2CandyProbe, platform string, row ChannelMonitorV2MatrixRow) bool {
	return (row.Model == "" || row.Model == probe.Model) && (row.Platform == "" || platform == PlatformComposite || row.Platform == platform)
}

func (s *ChannelMonitorV2CandyService) historyMany(ctx context.Context, configs map[int64][]string, since time.Time) ([]ChannelMonitorV2CandyResult, error) {
	if repo, ok := s.repo.(ChannelMonitorV2CandyMultiRepository); ok {
		return repo.CandyHistoryMany(ctx, configs, since)
	}
	var results []ChannelMonitorV2CandyResult
	for group, keys := range configs {
		for _, key := range keys {
			items, err := s.repo.CandyHistory(ctx, map[int64]string{group: key}, since)
			if err != nil {
				return nil, err
			}
			for _, item := range items {
				if item.GroupID == group && item.ConfigKey == key {
					results = append(results, item)
				}
			}
		}
	}
	return results, nil
}
