package handler

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// lockedOpsRepo records error logs from concurrent ops error log workers.
type lockedOpsRepo struct {
	service.OpsRepository
	mu      sync.Mutex
	entries []*service.OpsInsertErrorLogInput
}

func (r *lockedOpsRepo) InsertErrorLog(_ context.Context, entry *service.OpsInsertErrorLogInput) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, entry)
	return 0, nil
}

func (r *lockedOpsRepo) BatchInsertErrorLogs(_ context.Context, entries []*service.OpsInsertErrorLogInput) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, entries...)
	return int64(len(entries)), nil
}

func (r *lockedOpsRepo) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

func TestStopOpsErrorLogWorkers_FlushesQueuedAndBatchedEntries(t *testing.T) {
	resetOpsErrorLoggerStateForTest(t)
	t.Cleanup(func() { resetOpsErrorLoggerStateForTest(t) })

	repo := &lockedOpsRepo{}
	ops := service.NewOpsService(repo, &ingressRejectSettingRepo{}, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	const n = 100
	for i := 0; i < n; i++ {
		enqueueOpsErrorLog(ops, &service.OpsInsertErrorLogInput{
			RequestID:  fmt.Sprintf("req-%d", i),
			StatusCode: 502,
			ErrorPhase: "upstream",
			ErrorType:  "upstream_error",
			CreatedAt:  time.Now(),
		})
	}
	require.Equal(t, int64(n), opsErrorLogEnqueued.Load(), "all entries should be queued")

	start := time.Now()
	require.True(t, StopOpsErrorLogWorkers(), "drain must finish before timeout")
	require.Less(t, time.Since(start), opsErrorLogDrainTimeout, "drain must not hit the timeout")
	require.Equal(t, n, repo.count(), "stop must flush every queued and batched entry")
	require.Equal(t, int64(n), opsErrorLogProcessed.Load())

	// After stop, new entries are dropped instead of panicking on the closed queue.
	enqueueOpsErrorLog(ops, &service.OpsInsertErrorLogInput{RequestID: "late", StatusCode: 502, CreatedAt: time.Now()})
	require.Equal(t, int64(n), opsErrorLogEnqueued.Load())
	require.True(t, StopOpsErrorLogWorkers(), "repeated stop returns the first result")
}

func TestStopOpsErrorLogWorkers_NoWorkersStartedReturnsTrue(t *testing.T) {
	resetOpsErrorLoggerStateForTest(t)
	t.Cleanup(func() { resetOpsErrorLoggerStateForTest(t) })

	require.True(t, StopOpsErrorLogWorkers())
}
