//go:build unit

package service

import (
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func outboxLastUsedPayload() map[string]any {
	return map[string]any{"last_used": map[string]any{"101": float64(123)}}
}

func TestSchedulerSnapshotPollOutboxAdvancesWatermarkPastHandledEventsOnFailure(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	cache := &outboxCleanupCache{
		updateErr:   errors.New("cache update failed"),
		listBuckets: []SchedulerBucket{{Platform: PlatformOpenAI, Mode: SchedulerModeSingle}},
	}
	repo := &outboxCleanupRepo{
		events: []SchedulerOutboxEvent{
			{ID: 1, EventType: SchedulerOutboxEventAccountLastUsed, CreatedAt: old},
			{ID: 2, EventType: SchedulerOutboxEventAccountLastUsed, CreatedAt: old},
			{ID: 3, EventType: SchedulerOutboxEventAccountLastUsed, Payload: outboxLastUsedPayload(), CreatedAt: old},
			{ID: 4, EventType: SchedulerOutboxEventAccountLastUsed, CreatedAt: old},
		},
		rows:         []int64{1, 2, 3, 4},
		lockAcquired: true,
	}
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			Scheduling: config.GatewaySchedulingConfig{
				OutboxLagRebuildSeconds:  1,
				OutboxLagRebuildFailures: 1,
			},
		},
	}
	svc := NewSchedulerSnapshotService(cache, repo, &outboxCleanupAccountRepo{}, nil, cfg)

	svc.pollOutbox()

	require.Equal(t, int64(2), cache.watermark, "events handled before the failure must be committed")
	require.Equal(t, []int64{2}, cache.setWatermarks)
	require.Equal(t, []int64{3, 4}, repo.rows, "consumed rows up to the partial watermark are cleaned up")
	require.Equal(t, []int64{2}, repo.firstCreatedAfterID, "lag must be checked from the next pending (failing) event")
	require.Equal(t, 1, cache.listBucketCalls, "a stuck head-of-line event must still trigger the configured lag rebuild")

	// 下一轮从失败事件开始，不再重放已成功的 1、2；失败仍持续时不重复写水位，但 lag 检查照常执行。
	svc.pollOutbox()
	require.Equal(t, int64(2), cache.watermark)
	require.Equal(t, []int64{2}, cache.setWatermarks, "no progress means no watermark write")
	require.Equal(t, []int64{2, 2}, repo.firstCreatedAfterID)

	cache.updateErr = nil
	svc.pollOutbox()
	require.Equal(t, int64(4), cache.watermark)
	require.Equal(t, []int64{2, 4}, cache.setWatermarks)
	require.Empty(t, repo.rows)
}

func TestSchedulerSnapshotPollOutboxFirstEventFailureKeepsWatermarkButChecksLag(t *testing.T) {
	cache := &outboxCleanupCache{watermark: 4, updateErr: errors.New("cache update failed")}
	repo := &outboxCleanupRepo{
		events: []SchedulerOutboxEvent{
			{ID: 5, EventType: SchedulerOutboxEventAccountLastUsed, Payload: outboxLastUsedPayload(), CreatedAt: time.Now()},
			{ID: 6, EventType: SchedulerOutboxEventAccountLastUsed, CreatedAt: time.Now()},
		},
		rows:         []int64{4, 5, 6},
		lockAcquired: true,
	}
	svc := NewSchedulerSnapshotService(cache, repo, &outboxCleanupAccountRepo{}, nil, &config.Config{})

	svc.pollOutbox()

	require.Empty(t, cache.setWatermarks)
	require.Zero(t, repo.lockAttempts, "cleanup must not run without watermark progress")
	require.Equal(t, []int64{4, 5, 6}, repo.rows)
	require.Equal(t, []int64{4}, repo.firstCreatedAfterID, "lag check must run even when the first event fails")
}

func TestSchedulerSnapshotPollOutboxObservesStuckEventWithoutSkipping(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	groupID := int64(42)
	cache := &outboxCleanupCache{updateErr: errors.New("cache update failed")}
	repo := &outboxCleanupRepo{
		events: []SchedulerOutboxEvent{
			{ID: 9, EventType: SchedulerOutboxEventAccountLastUsed, GroupID: &groupID, Payload: outboxLastUsedPayload(), CreatedAt: now},
		},
		lockAcquired: true,
	}
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			Scheduling: config.GatewaySchedulingConfig{OutboxLagRebuildSeconds: 10},
		},
	}
	svc := NewSchedulerSnapshotService(cache, repo, &outboxCleanupAccountRepo{}, nil, cfg)
	svc.nowFn = func() time.Time { return now }

	svc.pollOutbox()
	id, logged := svc.outboxStuckEventSnapshot()
	require.Equal(t, int64(9), id)
	require.False(t, logged, "below the threshold the stuck event is only tracked")

	now = now.Add(10 * time.Second)
	svc.pollOutbox()
	id, logged = svc.outboxStuckEventSnapshot()
	require.Equal(t, int64(9), id)
	require.True(t, logged, "a single event failing past the threshold must raise one ERROR")
	require.Empty(t, cache.setWatermarks, "the stuck event must not be skipped")

	cache.updateErr = nil
	svc.pollOutbox()
	require.Equal(t, int64(9), cache.watermark)
	id, logged = svc.outboxStuckEventSnapshot()
	require.Zero(t, id)
	require.False(t, logged)
}
