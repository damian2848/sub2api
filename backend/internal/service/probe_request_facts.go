package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// ProbeRequestFact is a non-billing upstream attempt. GroupID is the actual
// routed group, never an account's membership list or the task's display label.
// Only the final generation attempt represents a logical channel outcome.
// Quality verdicts are intentionally absent: a wrong answer can be API success.
type ProbeRequestFact struct {
	RunID, LogicalRequestID, AttemptID                              string
	AttemptNumber                                                   int
	IsFinal                                                         bool
	Role, Platform                                                  string
	GroupID                                                         *int64
	AccountID                                                       int64
	Protocol, RequestedModel, UpstreamModel                         string
	StartedAt, FinishedAt                                           time.Time
	DurationMs                                                      int64
	FirstTokenMs                                                    *int64
	HTTPStatus                                                      *int
	APISuccess                                                      bool
	ErrorKind                                                       *string
	InputTokens, OutputTokens, CacheReadTokens, CacheCreationTokens *int64
	CacheCreation5mTokens, CacheCreation1hTokens, ReasoningTokens   *int64
	UsageComplete                                                   bool
	UpstreamCostUSD                                                 *float64
	CostComplete                                                    bool
}

// RecordProbeRequestFacts must persist a logical request atomically and
// idempotently. It records upstream cost snapshots, never customer charges.
type ProbeRequestFactRecorder interface {
	RecordProbeRequestFacts(context.Context, []ProbeRequestFact) error
}

// A logical channel outcome is stored once; the other rows are physical
// attempts retained for upstream consumption and account diagnostics.
func finalizeProbeFacts(facts []ProbeRequestFact) {
	for i := range facts {
		facts[i].AttemptNumber = i + 1
		facts[i].IsFinal = i == len(facts)-1
	}
}
func saveProbeFacts(recorder ProbeRequestFactRecorder, facts []ProbeRequestFact) {
	if recorder == nil || len(facts) == 0 {
		return
	}
	// Persist network/timeout failures even when the run context has expired.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := recorder.RecordProbeRequestFacts(ctx, facts); err != nil {
		logger.LegacyPrintf("service.probe_request_facts", "save logical_request=%s failed: %v", facts[0].LogicalRequestID, err)
	}
}

type probeRequestFactRecorderContextKey struct{}
