package service

import (
	"math"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

// AntigravityQuotaGroup mirrors an upstream pool, without guessing which models
// share a quota or combining percentages and resets from different buckets.
type AntigravityQuotaGroup struct {
	GroupID     string                   `json:"group_id"`
	DisplayName string                   `json:"display_name"`
	Description string                   `json:"description,omitempty"`
	Buckets     []AntigravityQuotaBucket `json:"buckets"`
}

type AntigravityQuotaBucket struct {
	BucketID        string   `json:"bucket_id"`
	DisplayName     string   `json:"display_name"`
	Description     string   `json:"description,omitempty"`
	Window          string   `json:"window"`
	Utilization     *float64 `json:"utilization,omitempty"`
	RemainingAmount *int64   `json:"remaining_amount,omitempty"`
	Disabled        bool     `json:"disabled"`
	ResetTime       string   `json:"reset_time,omitempty"`
}

func buildAntigravityQuotaGroups(summary *antigravity.RetrieveUserQuotaSummaryResponse) []AntigravityQuotaGroup {
	if summary == nil {
		return nil
	}
	groups := summary.Groups
	// Earlier versions expose the same buckets at the top level. Prefer grouped
	// data when present so a backwards-compatible duplicate is not rendered twice.
	if len(groups) == 0 && len(summary.Buckets) > 0 {
		groups = []antigravity.QuotaSummaryGroup{{Description: summary.Description, Buckets: summary.Buckets}}
	}
	result := make([]AntigravityQuotaGroup, 0, len(groups))
	for i, group := range groups {
		if len(group.Buckets) == 0 {
			continue
		}
		out := AntigravityQuotaGroup{
			GroupID: strconv.Itoa(i), DisplayName: group.DisplayName, Description: group.Description,
			Buckets: make([]AntigravityQuotaBucket, 0, len(group.Buckets)),
		}
		for _, bucket := range group.Buckets {
			b := AntigravityQuotaBucket{
				BucketID: bucket.BucketID, DisplayName: bucket.DisplayName, Description: bucket.Description,
				Window: bucket.Window, ResetTime: bucket.ResetTime, Disabled: bucket.Disabled,
			}
			if bucket.RemainingFraction != nil {
				fraction := *bucket.RemainingFraction
				if !math.IsNaN(fraction) && !math.IsInf(fraction, 0) && fraction >= 0 && fraction <= 1 {
					// Preserve precision; rounding is a presentation concern.
					used := (1 - fraction) * 100
					b.Utilization = &used
				}
			} else if bucket.RemainingAmount != nil {
				if amount, err := bucket.RemainingAmount.Int64(); err == nil {
					b.RemainingAmount = &amount
				}
			}
			out.Buckets = append(out.Buckets, b)
		}
		result = append(result, out)
	}
	return result
}

// Only explicit window identities are normalized for the monitor. A reset time
// alone says nothing about the duration of its quota window.
func antigravityMonitorWindow(window string) string {
	switch strings.ToLower(strings.TrimSpace(window)) {
	case "5h", "5hr", "5-hour", "5 hour", "5 hours", "five_hour", "five-hour", "18000s":
		return "5h"
	case "7d", "7-day", "7 day", "7 days", "weekly", "week", "seven_day", "604800s":
		return "7d"
	case "":
		return "quota"
	default:
		return window
	}
}
