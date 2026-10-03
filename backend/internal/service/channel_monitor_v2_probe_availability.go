package service

import (
	"context"
	"sort"
	"strings"
	"time"
)

// ChannelMonitorV2DisplayModel is shared by business and probe aggregation so
// both sources use the same configured-model / __other__ dimensions.
func ChannelMonitorV2DisplayModel(cfg ChannelMonitorV2Config, platform, model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ChannelMonitorV2OtherModel
	}
	for _, p := range cfg.Platforms {
		if p.Platform != platform {
			continue
		}
		if len(p.Models) == 0 {
			return model
		}
		for _, selected := range p.Models {
			if selected == model {
				return model
			}
		}
		return ChannelMonitorV2OtherModel
	}
	return model
}

type channelMonitorAvailabilityProbe struct {
	observation ChannelMonitorObservation
	groups      []int64
	model       string
}

func (s *ChannelMonitorV2Service) loadAvailabilityProbes(ctx context.Context, filter ChannelMonitorV2Filter, cfg *ChannelMonitorV2Config, admin bool) ([]channelMonitorAvailabilityProbe, error) {
	repo, ok := s.repo.(ChannelMonitorObservationRepository)
	if !ok || s.settings == nil || filter.Source == "business" {
		return nil, nil
	}
	runtime := s.settings.GetChannelMonitorRuntime(ctx)
	if !runtime.ActiveProbesAllowed() && !runtime.QualityProbesAllowed() {
		return nil, nil
	}
	// Do not call Observations here: it calls Snapshot for business usage.
	rows, err := repo.ListMonitorObservations(ctx, filter)
	if err != nil {
		return nil, err
	}
	now := s.now()
	probes := []channelMonitorAvailabilityProbe{}
	seen := map[[4]string]bool{}
	for _, row := range rows {
		if !availabilityProbeEnabled(row, cfg, runtime) || !availabilityProbeFresh(row, now) {
			continue
		}
		if (!filter.Start.IsZero() && row.CheckedAt.Before(filter.Start)) || (!filter.End.IsZero() && !row.CheckedAt.Before(filter.End)) {
			continue
		}
		groups := admittedObservationGroups(row.Scope.GroupIDs, filter, cfg.GroupIDs, admin)
		if !observationPlatformAllowed(row.Platform, filter, cfg) || !observationModelAllowed(row.Platform, row.Model, filter, cfg) ||
			(len(groups) == 0 && (!admin || filter.RestrictGroups || len(filter.GroupIDs) > 0 || len(cfg.GroupIDs) > 0)) {
			continue
		}
		key := [4]string{row.ID, row.Type, row.Platform, row.Model}
		if seen[key] {
			continue
		}
		seen[key] = true
		probes = append(probes, channelMonitorAvailabilityProbe{observation: row, groups: groups, model: ChannelMonitorV2DisplayModel(*cfg, row.Platform, row.Model)})
	}
	return probes, nil
}

func availabilityProbeEnabled(row ChannelMonitorObservation, cfg *ChannelMonitorV2Config, runtime ChannelMonitorRuntime) bool {
	// Manual checks and disabled/deleted schedules remain in observation history,
	// but must not advertise an actively monitored channel.
	if !row.Enabled {
		return false
	}
	switch row.Type {
	case "connectivity", "quota":
		return runtime.ActiveProbesAllowed()
	case "candy":
		return cfg.Enabled && runtime.QualityProbesAllowed()
	case "quality", "state_probe":
		return runtime.QualityProbesAllowed()
	default:
		return false
	}
}

func availabilityProbeFresh(row ChannelMonitorObservation, now time.Time) bool {
	if row.CheckedAt == nil || row.SampleCount <= 0 || row.CheckedAt.After(now) {
		return false
	}
	checked := *row.CheckedAt
	expires := checked.Add(15 * time.Minute)
	if row.IntervalSeconds > 0 {
		expires = checked.Add(time.Duration(row.IntervalSeconds)*time.Second + 2*time.Minute)
	} else if row.Schedule != "" {
		// Cron without an explicit timezone uses the scheduler's local timezone,
		// not the UTC location of a timestamp decoded from the database.
		if next, err := computeNextRun(row.Schedule, checked.In(time.Local)); err == nil && next.After(checked) {
			expires = next.Add(2 * time.Minute)
		}
	}
	if minimum := checked.Add(3 * time.Minute); expires.Before(minimum) {
		expires = minimum
	}
	return !now.After(expires)
}

type channelMonitorProbeAvailability struct {
	samples, passed, failed, inconclusive int64
	checkedAt                             *time.Time
	groupName                             string
}

func (p *channelMonitorProbeAvailability) add(row ChannelMonitorObservation) {
	p.samples += row.SampleCount
	p.passed += row.PassedCount
	p.failed += row.FailedCount
	p.inconclusive += row.InconclusiveCount
	if row.CheckedAt != nil && (p.checkedAt == nil || row.CheckedAt.After(*p.checkedAt)) {
		checked := *row.CheckedAt
		p.checkedAt = &checked
	}
}

func (p channelMonitorProbeAvailability) apply(metric *ChannelMonitorV2Metric, health *ChannelMonitorV2Health, thresholds ChannelMonitorV2HealthThresholds) {
	metric.ProbeSampleCount, metric.ProbePassedCount = p.samples, p.passed
	metric.ProbeFailedCount, metric.ProbeInconclusiveCount = p.failed, p.inconclusive
	metric.ProbeCheckedAt = p.checkedAt
	// Task verdicts (including wrong answers, quota checks and HEAD pings) are
	// diagnostic metadata, not generation-request outcomes. Availability and
	// health come exclusively from deduplicated gateway/direct request facts.
	// In particular, legacy observations may have a declared group that differs
	// from the API key's actual route and must never create a false success.
	metric.ProbeAvailability = nil
	if !metric.HasSamples && metric.RequestCount == 0 {
		metric.AvailabilitySource = "unknown"
		*health = ChannelMonitorV2HealthForWithThresholds(ChannelMonitorV2Metric{}, thresholds)
	} else if metric.AvailabilitySource == "" || metric.AvailabilitySource == "unknown" {
		// Compatibility with repositories that omit the source discriminator.
		switch {
		case metric.BusinessRequestCount > 0 && metric.ProbeRequestCount > 0:
			metric.AvailabilitySource = "mixed"
		case metric.ProbeRequestCount > 0:
			metric.AvailabilitySource = "probe"
		default:
			metric.AvailabilitySource = "business"
		}
	}
}

type channelMonitorProbeDimension struct {
	platform string
	groupID  int64
	model    string
}

func probeAvailabilityDimensions(probes []channelMonitorAvailabilityProbe, by ChannelMonitorV2GroupBy) map[channelMonitorProbeDimension]channelMonitorProbeAvailability {
	dimensions := map[channelMonitorProbeDimension]channelMonitorProbeAvailability{}
	for _, probe := range probes {
		key := channelMonitorProbeDimension{platform: probe.observation.Platform}
		if by == ChannelMonitorV2GroupByPlatformModel || by == ChannelMonitorV2GroupByPlatformGroupModel {
			key.model = probe.model
		}
		groups := []int64{0}
		if by == ChannelMonitorV2GroupByPlatformGroup || by == ChannelMonitorV2GroupByPlatformGroupModel {
			// Never invent a group for an account-level or unbound probe.
			groups = probe.groups
		}
		for _, id := range groups {
			key.groupID = id
			value := dimensions[key]
			value.add(probe.observation)
			if name := probe.observation.Scope.GroupNames[id]; name != "" {
				value.groupName = name
			}
			dimensions[key] = value
		}
	}
	return dimensions
}

func mergeModelProbeAvailability(list *ChannelMonitorV2List[ChannelMonitorV2ModelRow], probes []channelMonitorAvailabilityProbe, thresholds ChannelMonitorV2HealthThresholds) {
	dimensions := probeAvailabilityDimensions(probes, ChannelMonitorV2GroupByPlatformModel)
	for i := range list.Items {
		row := &list.Items[i]
		key := channelMonitorProbeDimension{platform: row.Platform, model: row.Model}
		dimensions[key].apply(&row.Metrics, &row.Health, thresholds)
		delete(dimensions, key)
	}
	for key, probe := range dimensions {
		row := ChannelMonitorV2ModelRow{Platform: key.platform, Model: key.model, Health: ChannelMonitorV2HealthForWithThresholds(ChannelMonitorV2Metric{}, thresholds)}
		probe.apply(&row.Metrics, &row.Health, thresholds)
		list.Items = append(list.Items, row)
	}
	sort.SliceStable(list.Items, func(i, j int) bool {
		a, b := list.Items[i], list.Items[j]
		if a.Metrics.RequestCount != b.Metrics.RequestCount {
			return a.Metrics.RequestCount > b.Metrics.RequestCount
		}
		if a.Platform != b.Platform {
			return a.Platform < b.Platform
		}
		return a.Model < b.Model
	})
}

func mergeMatrixProbeAvailability(matrix *ChannelMonitorV2Matrix, probes []channelMonitorAvailabilityProbe, by ChannelMonitorV2GroupBy, thresholds ChannelMonitorV2HealthThresholds) {
	dimensions := probeAvailabilityDimensions(probes, by)
	for i := range matrix.Items {
		row := &matrix.Items[i]
		key := channelMonitorProbeDimension{platform: row.Platform, model: row.Model}
		if row.GroupID != nil {
			key.groupID = *row.GroupID
		}
		dimensions[key].apply(&row.Metrics, &row.Health, thresholds)
		delete(dimensions, key)
	}
	for key, probe := range dimensions {
		row := ChannelMonitorV2MatrixRow{Platform: key.platform, Model: key.model, GroupName: probe.groupName, Health: ChannelMonitorV2HealthForWithThresholds(ChannelMonitorV2Metric{}, thresholds), Buckets: []ChannelMonitorV2TrendPoint{}}
		if key.groupID > 0 {
			row.GroupID = cloneInt64Pointer(&key.groupID)
		}
		probe.apply(&row.Metrics, &row.Health, thresholds)
		matrix.Items = append(matrix.Items, row)
	}
	sort.SliceStable(matrix.Items, func(i, j int) bool {
		a, b := matrix.Items[i], matrix.Items[j]
		if a.Platform != b.Platform {
			return a.Platform < b.Platform
		}
		if a.GroupName != b.GroupName {
			return a.GroupName < b.GroupName
		}
		if a.GroupID != nil && b.GroupID != nil && *a.GroupID != *b.GroupID {
			return *a.GroupID < *b.GroupID
		}
		return a.Model < b.Model
	})
}
