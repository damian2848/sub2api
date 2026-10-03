//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestChannelMonitorV2MaxChunkForDepth(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	// Within last day → tightest ceiling (2h).
	require.Equal(t, channelMonitorV2MaxChunkNear1d, channelMonitorV2MaxChunkForDepth(now, now.Add(-2*time.Hour)))
	// Between 1d and 7d → 4h.
	require.Equal(t, channelMonitorV2MaxChunkNear7d, channelMonitorV2MaxChunkForDepth(now, now.Add(-2*24*time.Hour)))
	// Older than 7d → 6h (never 24h default).
	require.Equal(t, channelMonitorV2MaxChunkFar, channelMonitorV2MaxChunkForDepth(now, now.Add(-10*24*time.Hour)))
	require.Less(t, channelMonitorV2MaxChunkFar, 24*time.Hour)
	require.Equal(t, time.Hour, channelMonitorV2BackfillChunkInit)
	require.Equal(t, 15*time.Minute, channelMonitorV2MinBackfillChunk)
}

func TestChannelMonitorV2AggregatorAdaptiveChunk(t *testing.T) {
	s := NewChannelMonitorV2Aggregator(nil, nil, nil)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	cursor := now.Add(-3 * time.Hour)

	// Failure shrinks chunk and sets backoff floor.
	s.backfillChunk = 2 * time.Hour
	s.recordBackfillFailure(now, cursor)
	require.Equal(t, time.Hour, s.backfillChunk)
	require.Equal(t, time.Minute, s.nextWaitFloor)
	require.Equal(t, 1, s.backfillFailures)

	// Repeated failure halves again and raises floor.
	s.recordBackfillFailure(now, cursor)
	require.Equal(t, 30*time.Minute, s.backfillChunk)
	require.Equal(t, 2*time.Minute, s.nextWaitFloor)

	// Fast success grows within depth ceiling and clears backoff.
	s.recordBackfillSuccess(cursor.Add(-30*time.Minute), 5*time.Second, now)
	require.Equal(t, 0, s.backfillFailures)
	require.Equal(t, time.Duration(0), s.nextWaitFloor)
	require.Greater(t, s.backfillChunk, 30*time.Minute)
	require.LessOrEqual(t, s.backfillChunk, channelMonitorV2MaxChunkForDepth(now, cursor.Add(-30*time.Minute)))
}

type channelMonitorV2VersionedCursorRepo struct {
	channelMonitorV2RepoStub
	watermark *ChannelMonitorV2AggregationWatermark
}

func (r *channelMonitorV2VersionedCursorRepo) GetAggregationWatermark(context.Context) (*ChannelMonitorV2AggregationWatermark, error) {
	return r.watermark, nil
}

func TestChannelMonitorV2AccountingUpgradeRestartsBoundedBootstrap(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for _, version := range []int{1, ChannelMonitorV2AccountingVersion} {
		repo := &channelMonitorV2VersionedCursorRepo{watermark: &ChannelMonitorV2AggregationWatermark{
			AccountingVersion: version, HasData: true, DataThrough: now,
			BackfillCursor: now.Add(-channelMonitorV2RetentionMax),
		}}
		aggregator := NewChannelMonitorV2Aggregator(repo, nil, nil)
		require.NoError(t, aggregator.ensureCursor(context.Background(), now))
		require.True(t, aggregator.cursorLoaded)
		if version == ChannelMonitorV2AccountingVersion {
			require.True(t, aggregator.hasAggregated)
			require.Equal(t, repo.watermark.BackfillCursor, aggregator.backfillAt)
		} else {
			require.False(t, aggregator.hasAggregated, "business-only coverage cannot masquerade as unified data")
			require.True(t, aggregator.backfillAt.IsZero())
			require.Equal(t, 2*time.Hour, channelMonitorV2BootstrapFirst, "upgrade starts recent then fills history in bounded chunks")
			require.LessOrEqual(t, aggregator.backfillChunk, channelMonitorV2MaxChunkNear1d)
		}
	}
}

func TestChannelMonitorV2HistoricalChunksNeverExpandToWholeDay(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for _, hour := range []int{1, 11, 23} {
		end := time.Date(2026, 9, 20, hour, 0, 0, 0, time.UTC)
		start := channelMonitorV2BackfillStart(now, end, 48*time.Hour)
		require.Equal(t, channelMonitorV2MaxChunkFar, end.Sub(start))
		start = channelMonitorV2BackfillStart(now, end, channelMonitorV2MinBackfillChunk)
		require.Equal(t, channelMonitorV2MinBackfillChunk, end.Sub(start), "failure backoff remains effective after crossing day boundaries")
	}
	end := now.Add(-channelMonitorV2RetentionMax).Add(time.Hour)
	require.Equal(t, now.Add(-channelMonitorV2RetentionMax), channelMonitorV2BackfillStart(now, end, channelMonitorV2MaxChunkFar))
}
