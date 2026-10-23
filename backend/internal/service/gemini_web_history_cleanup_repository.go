package service

import (
	"context"
	"errors"
	"time"
)

// GeminiWebCleanupWindow identifies one upstream quota cycle. ResetAt must be
// supplied by Google; estimated reset times must never authorize deletion.
type GeminiWebCleanupWindow struct {
	ID      string    `json:"id"`
	ResetAt time.Time `json:"reset_at"`
}

type GeminiWebHistoryCleanupJob struct {
	ID            int64
	AccountID     int64
	Identity      string
	Windows       []GeminiWebCleanupWindow
	Snapshot      []string
	SnapshotReady bool
	Attempts      int
	LeaseToken    string
	LeaseUntil    time.Time
}

var ErrGeminiWebHistoryMaintenance = errors.New("gemini web account history cleanup is in progress; retry shortly")
var ErrGeminiWebHistoryLeaseLost = errors.New("gemini web history cleanup lease lost")

// Implemented by the SQL account repository. All job writes are fenced by the
// lease token. Request admission and cleanup claims share an account lock.
type GeminiWebHistoryCleanupRepository interface {
	EnqueueGeminiWebHistoryCleanup(context.Context, int64, string, []GeminiWebCleanupWindow) error
	ClaimGeminiWebHistoryCleanup(context.Context, time.Duration) (*GeminiWebHistoryCleanupJob, error)
	RenewGeminiWebHistoryCleanup(context.Context, *GeminiWebHistoryCleanupJob, time.Duration) error
	GeminiWebHistoryHasActiveRequests(context.Context, *GeminiWebHistoryCleanupJob) (bool, error)
	SaveGeminiWebHistorySnapshot(context.Context, *GeminiWebHistoryCleanupJob, []string) error
	FinishGeminiWebHistoryCleanup(context.Context, *GeminiWebHistoryCleanupJob) error
	RetryGeminiWebHistoryCleanup(context.Context, *GeminiWebHistoryCleanupJob, string, time.Time) error
	CancelGeminiWebHistoryCleanup(context.Context, *GeminiWebHistoryCleanupJob, string) error
	BeginGeminiWebHistoryRequest(context.Context, int64, time.Duration) (string, error)
	EndGeminiWebHistoryRequest(context.Context, int64, string) error
}
