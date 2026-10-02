package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"sort"
	"time"
)

// Scope is captured before a quality rule can move or disable its account.
type ChannelMonitorObservationScope struct {
	Platform   string           `json:"platform"`
	GroupIDs   []int64          `json:"group_ids"`
	GroupNames map[int64]string `json:"group_names,omitempty"`
	AccountID  *int64           `json:"account_id,omitempty"`
	Name       string           `json:"name,omitempty"`
	CheckMode  string           `json:"check_mode,omitempty"`
}

type ChannelMonitorObservationPoint struct {
	CheckedAt time.Time `json:"checked_at"`
	Verdict   string    `json:"verdict"`
	LatencyMs *int64    `json:"latency_ms,omitempty"`
	Message   string    `json:"message,omitempty"`
}

type ChannelMonitorObservation struct {
	ID                string                           `json:"id"`
	Type              string                           `json:"type"`
	Name              string                           `json:"name"`
	Platform          string                           `json:"platform"`
	GroupID           *int64                           `json:"group_id,omitempty"`
	GroupName         string                           `json:"group_name"`
	Model             string                           `json:"model"`
	Enabled           bool                             `json:"enabled"`
	IntervalSeconds   int                              `json:"interval_seconds"`
	Schedule          string                           `json:"schedule,omitempty"`
	Verdict           string                           `json:"verdict"`
	CheckedAt         *time.Time                       `json:"checked_at,omitempty"`
	LatencyMs         *int64                           `json:"latency_ms,omitempty"`
	SampleCount       int64                            `json:"sample_count"`
	PassedCount       int64                            `json:"passed_count"`
	FailedCount       int64                            `json:"failed_count"`
	InconclusiveCount int64                            `json:"inconclusive_count"`
	Usage             ChannelMonitorProbeUsage         `json:"usage"`
	History           []ChannelMonitorObservationPoint `json:"history"`
	Scope             ChannelMonitorObservationScope   `json:"-"`
}

type ChannelMonitorObservationSummary struct {
	ProbeCount        int64                    `json:"probe_count"`
	PassedCount       int64                    `json:"passed_count"`
	FailedCount       int64                    `json:"failed_count"`
	InconclusiveCount int64                    `json:"inconclusive_count"`
	Usage             ChannelMonitorProbeUsage `json:"usage"`
}

type ChannelMonitorObservations struct {
	Range         string                           `json:"range"`
	Start         time.Time                        `json:"start"`
	End           time.Time                        `json:"end"`
	ComputedAt    time.Time                        `json:"computed_at"`
	GroupBy       ChannelMonitorV2GroupBy          `json:"group_by"`
	Items         []ChannelMonitorObservation      `json:"items"`
	Summary       ChannelMonitorObservationSummary `json:"summary"`
	BusinessUsage ChannelMonitorV2Metric           `json:"business_usage"`
	TotalTokens   int64                            `json:"total_tokens"`
}

type ChannelMonitorObservationRepository interface {
	ListMonitorObservations(context.Context, ChannelMonitorV2Filter) ([]ChannelMonitorObservation, error)
}

type ChannelMonitorProbeRecorder interface {
	RecordStateProbe(context.Context, *OpenAICodexStateProbeResult) error
}

func (s *ChannelMonitorService) monitorObservationScope(ctx context.Context, m *ChannelMonitor) *ChannelMonitorObservationScope {
	scope := &ChannelMonitorObservationScope{Platform: m.Provider, GroupIDs: []int64{}, Name: m.Name, CheckMode: defaultCheckMode(m.CheckMode), AccountID: cloneInt64Pointer(m.AccountID)}
	if m.GroupID != nil {
		scope.GroupIDs = []int64{*m.GroupID}
		scope.GroupNames = map[int64]string{}
		if reader, ok := s.repo.(interface {
			GetMonitorGroup(context.Context, int64) (*Group, error)
		}); ok {
			if group, err := reader.GetMonitorGroup(ctx, *m.GroupID); err == nil && group != nil {
				scope.GroupNames[*m.GroupID] = group.Name
			}
		}
	}
	return scope
}

func accountObservationScope(account *Account) *ChannelMonitorObservationScope {
	if account == nil {
		return nil
	}
	scope := &ChannelMonitorObservationScope{Platform: account.Platform, GroupIDs: append([]int64{}, account.GroupIDs...), GroupNames: map[int64]string{}, AccountID: cloneInt64Pointer(&account.ID), Name: account.Name}
	for _, group := range account.Groups {
		if group != nil {
			scope.GroupNames[group.ID] = group.Name
			if !slices.Contains(scope.GroupIDs, group.ID) {
				scope.GroupIDs = append(scope.GroupIDs, group.ID)
			}
		}
	}
	scope.GroupIDs = normalizeInt64Set(scope.GroupIDs)
	return scope
}

func (s *ChannelMonitorService) validateMonitorGroup(ctx context.Context, provider string, id *int64) error {
	if id == nil {
		return nil
	}
	reader, ok := s.repo.(interface {
		GetMonitorGroup(context.Context, int64) (*Group, error)
	})
	if !ok || *id <= 0 {
		return fmt.Errorf("%w: invalid monitor group", ErrChannelMonitorV2InvalidConfig)
	}
	group, err := reader.GetMonitorGroup(ctx, *id)
	if err != nil || group == nil {
		return fmt.Errorf("%w: monitor group unavailable", ErrChannelMonitorV2InvalidConfig)
	}
	if group.Platform != PlatformComposite && group.Platform != provider {
		return ErrChannelMonitorProviderIncompatible
	}
	return nil
}

func (s *ChannelMonitorV2Service) Observations(ctx context.Context, filter ChannelMonitorV2Filter, groupBy ChannelMonitorV2GroupBy, admin bool) (*ChannelMonitorObservations, error) {
	if !groupBy.Valid() {
		return nil, ErrChannelMonitorV2InvalidGroupBy
	}
	cfg, err := s.getEnabledConfig(ctx)
	if err != nil {
		return nil, err
	}
	repo, ok := s.repo.(ChannelMonitorObservationRepository)
	if !ok {
		return nil, fmt.Errorf("monitor observation repository unavailable")
	}
	rows, err := repo.ListMonitorObservations(ctx, filter)
	if err != nil {
		return nil, err
	}
	result := &ChannelMonitorObservations{Range: filter.Range, Start: filter.Start, End: filter.End, ComputedAt: s.now(), GroupBy: groupBy, Items: []ChannelMonitorObservation{}}
	runtime := ChannelMonitorRuntime{Enabled: false}
	if s.settings != nil {
		runtime = s.settings.GetChannelMonitorRuntime(ctx)
	}
	seen := map[string]bool{}
	for _, row := range rows {
		row.History = slices.Clone(row.History)
		switch row.Type {
		case "connectivity", "quota":
			row.Enabled = row.Enabled && runtime.ActiveProbesAllowed()
		case "candy":
			row.Enabled = row.Enabled && cfg.Enabled && runtime.QualityProbesAllowed()
		}
		groups := admittedObservationGroups(row.Scope.GroupIDs, filter, cfg.GroupIDs, admin)
		if !observationPlatformAllowed(row.Platform, filter, cfg) || !observationModelAllowed(row.Platform, row.Model, filter, cfg) || (len(groups) == 0 && (!admin || len(filter.GroupIDs) > 0 || len(cfg.GroupIDs) > 0)) {
			continue
		}
		key := fmt.Sprintf("%s:%s:%s:%s", row.ID, row.Type, row.Platform, row.Model)
		if !seen[key] {
			seen[key] = true
			result.Summary.ProbeCount += row.SampleCount
			result.Summary.PassedCount += row.PassedCount
			result.Summary.FailedCount += row.FailedCount
			result.Summary.InconclusiveCount += row.InconclusiveCount
			AddChannelMonitorProbeUsage(&result.Summary.Usage, &row.Usage)
		}
		if !admin {
			row.Name = row.Type
			for i := range row.History {
				row.History[i].Message = ""
			}
		}
		if len(groups) == 0 {
			groups = []int64{0}
		}
		if groupBy == ChannelMonitorV2GroupByPlatform || groupBy == ChannelMonitorV2GroupByPlatformModel {
			groups = groups[:1]
		}
		for _, id := range groups {
			item := row
			item.ID = fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s:%d", row.ID, id))))[:24]
			item.GroupName = row.Scope.GroupNames[id]
			if id > 0 {
				item.GroupID = cloneInt64Pointer(&id)
			}
			result.Items = append(result.Items, item)
		}
	}
	result.Items = groupMonitorObservations(result.Items, groupBy)
	snapshot, err := s.Snapshot(ctx, filter, admin)
	if err != nil {
		return nil, err
	}
	if snapshot != nil {
		result.BusinessUsage = snapshot.Metrics
	}
	result.Summary.Usage.Source = "probe"
	result.TotalTokens = result.BusinessUsage.TokenCount + result.Summary.Usage.InputTokens + result.Summary.Usage.OutputTokens + result.Summary.Usage.CacheReadTokens + result.Summary.Usage.CacheCreationTokens
	if !admin {
		result.Summary.Usage = ChannelMonitorProbeUsage{Source: "probe", UsageIncomplete: true, CostIncomplete: true}
		result.TotalTokens = 0
		for i := range result.Items {
			result.Items[i].Usage = result.Summary.Usage
		}
	}
	return result, nil
}

func observationModelAllowed(platform, model string, filter ChannelMonitorV2Filter, cfg *ChannelMonitorV2Config) bool {
	if len(filter.Models) == 0 || slices.Contains(filter.Models, model) {
		return true
	}
	if !slices.Contains(filter.Models, ChannelMonitorV2OtherModel) {
		return false
	}
	if model == "" {
		return true
	}
	for _, p := range cfg.Platforms {
		if p.Platform == platform && len(p.Models) > 0 {
			return !slices.Contains(p.Models, model)
		}
	}
	return false
}

func admittedObservationGroups(ids []int64, filter ChannelMonitorV2Filter, configured []int64, admin bool) []int64 {
	out := []int64{}
	for _, id := range ids {
		if id > 0 && (len(configured) == 0 || slices.Contains(configured, id)) && (len(filter.GroupIDs) == 0 || slices.Contains(filter.GroupIDs, id)) && (admin || !filter.RestrictGroups || slices.Contains(filter.AllowedGroupIDs, id)) && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

func observationPlatformAllowed(platform string, filter ChannelMonitorV2Filter, cfg *ChannelMonitorV2Config) bool {
	if len(filter.Platforms) > 0 && !slices.Contains(filter.Platforms, platform) {
		return false
	}
	if (platform == PlatformComposite || platform == "unknown") && len(filter.Platforms) == 0 {
		return true
	}
	for _, p := range cfg.Platforms {
		if p.Platform == platform {
			return p.Enabled
		}
	}
	return false
}

func groupMonitorObservations(rows []ChannelMonitorObservation, by ChannelMonitorV2GroupBy) []ChannelMonitorObservation {
	if by == ChannelMonitorV2GroupByPlatformGroupModel {
		return rows
	}
	out := []ChannelMonitorObservation{}
	index := map[string]int{}
	for _, row := range rows {
		group, model := int64(0), ""
		if by == ChannelMonitorV2GroupByPlatformGroup && row.GroupID != nil {
			group = *row.GroupID
		}
		if by == ChannelMonitorV2GroupByPlatformModel {
			model = row.Model
		}
		key := fmt.Sprintf("%s:%s:%d:%s", row.Type, row.Platform, group, model)
		if i, ok := index[key]; ok {
			item := &out[i]
			item.SampleCount += row.SampleCount
			item.PassedCount += row.PassedCount
			item.FailedCount += row.FailedCount
			item.InconclusiveCount += row.InconclusiveCount
			AddChannelMonitorProbeUsage(&item.Usage, &row.Usage)
			item.Enabled = item.Enabled || row.Enabled
			item.History = append(item.History, row.History...)
			if row.CheckedAt != nil && (item.CheckedAt == nil || row.CheckedAt.After(*item.CheckedAt)) {
				item.CheckedAt, row.CheckedAt = row.CheckedAt, item.CheckedAt
				item.Verdict = row.Verdict
				item.LatencyMs = row.LatencyMs
			}
		} else {
			index[key] = len(out)
			row.ID = key
			row.Name = row.Type
			row.Model = model
			if group == 0 {
				row.GroupID = nil
				row.GroupName = ""
			}
			out = append(out, row)
		}
	}
	for i := range out {
		sort.Slice(out[i].History, func(a, b int) bool { return out[i].History[a].CheckedAt.After(out[i].History[b].CheckedAt) })
		if len(out[i].History) > 100 {
			out[i].History = out[i].History[:100]
		}
	}
	return out
}

func mergeMonitorObservationDimensions(dims *ChannelMonitorV2Dimensions, rows []ChannelMonitorObservation, filter ChannelMonitorV2Filter, cfg *ChannelMonitorV2Config) {
	if dims == nil {
		return
	}
	platforms, models, groups := map[string]bool{}, map[string]bool{}, map[int64]bool{}
	for _, p := range dims.Platforms {
		platforms[p.Value] = true
	}
	for _, m := range dims.Models {
		models[m.Value+":"+m.Platform] = true
	}
	for _, g := range dims.Groups {
		groups[g.ID] = true
	}
	for _, row := range rows {
		ids := admittedObservationGroups(row.Scope.GroupIDs, filter, cfg.GroupIDs, !filter.RestrictGroups)
		if !observationPlatformAllowed(row.Platform, filter, cfg) || (len(ids) == 0 && (filter.RestrictGroups || len(filter.GroupIDs) > 0 || len(cfg.GroupIDs) > 0)) {
			continue
		}
		if !platforms[row.Platform] {
			dims.Platforms = append(dims.Platforms, ChannelMonitorV2Dimension{Value: row.Platform, Label: row.Platform})
			platforms[row.Platform] = true
		}
		if !models[row.Model+":"+row.Platform] {
			dims.Models = append(dims.Models, ChannelMonitorV2Dimension{Value: row.Model, Label: row.Model, Platform: row.Platform})
			models[row.Model+":"+row.Platform] = true
		}
		for _, id := range ids {
			if !groups[id] {
				dims.Groups = append(dims.Groups, ChannelMonitorV2GroupDimension{ID: id, Name: row.Scope.GroupNames[id], Platform: row.Platform})
				groups[id] = true
			}
		}
	}
}
