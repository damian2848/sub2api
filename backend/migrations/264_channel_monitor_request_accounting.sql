-- Channel statistics count actual business and probe generation requests. Source
-- remains a dimension; user/billing tables are deliberately untouched. Existing
-- rows are business facts until the bounded worker replaces each covered range.
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS api_success BOOLEAN;
COMMENT ON COLUMN usage_logs.api_success IS
    'Observed API completion independent of customer price; NULL for legacy logs with only historical billing evidence.';

ALTER TABLE channel_monitor_v2_metrics_1m
    ADD COLUMN IF NOT EXISTS source VARCHAR(16) NOT NULL DEFAULT 'business'
        CHECK (source IN ('business', 'probe'));
ALTER TABLE channel_monitor_v2_metrics_1m DROP CONSTRAINT IF EXISTS channel_monitor_v2_metrics_1m_pkey;
ALTER TABLE channel_monitor_v2_metrics_1m ADD PRIMARY KEY (bucket_start, platform, group_id, model, source);

ALTER TABLE channel_monitor_v2_metrics_rollup
    ADD COLUMN IF NOT EXISTS source VARCHAR(16) NOT NULL DEFAULT 'business'
        CHECK (source IN ('business', 'probe'));
ALTER TABLE channel_monitor_v2_metrics_rollup DROP CONSTRAINT IF EXISTS channel_monitor_v2_metrics_rollup_pkey;
ALTER TABLE channel_monitor_v2_metrics_rollup ADD PRIMARY KEY (bucket_seconds, bucket_start, platform, group_id, model, source);

ALTER TABLE channel_monitor_v2_error_metrics_1m
    ADD COLUMN IF NOT EXISTS source VARCHAR(16) NOT NULL DEFAULT 'business'
        CHECK (source IN ('business', 'probe'));
ALTER TABLE channel_monitor_v2_error_metrics_1m DROP CONSTRAINT IF EXISTS channel_monitor_v2_error_metrics_1m_pkey;
ALTER TABLE channel_monitor_v2_error_metrics_1m ADD PRIMARY KEY (bucket_start, platform, group_id, model, error_category, taxonomy_version, source);

ALTER TABLE channel_monitor_v2_error_metrics_rollup
    ADD COLUMN IF NOT EXISTS source VARCHAR(16) NOT NULL DEFAULT 'business'
        CHECK (source IN ('business', 'probe'));
ALTER TABLE channel_monitor_v2_error_metrics_rollup DROP CONSTRAINT IF EXISTS channel_monitor_v2_error_metrics_rollup_pkey;
ALTER TABLE channel_monitor_v2_error_metrics_rollup ADD PRIMARY KEY (bucket_seconds, bucket_start, platform, group_id, model, error_category, taxonomy_version, source);

ALTER TABLE channel_monitor_v2_latency_histograms_1m
    ADD COLUMN IF NOT EXISTS source VARCHAR(16) NOT NULL DEFAULT 'business'
        CHECK (source IN ('business', 'probe'));
ALTER TABLE channel_monitor_v2_latency_histograms_1m DROP CONSTRAINT IF EXISTS channel_monitor_v2_latency_histograms_1m_pkey;
ALTER TABLE channel_monitor_v2_latency_histograms_1m ADD PRIMARY KEY (bucket_start, platform, group_id, model, user_id, metric, upper_bound_ms, source);

ALTER TABLE channel_monitor_v2_latency_histograms_rollup
    ADD COLUMN IF NOT EXISTS source VARCHAR(16) NOT NULL DEFAULT 'business'
        CHECK (source IN ('business', 'probe'));
ALTER TABLE channel_monitor_v2_latency_histograms_rollup DROP CONSTRAINT IF EXISTS channel_monitor_v2_latency_histograms_rollup_pkey;
ALTER TABLE channel_monitor_v2_latency_histograms_rollup ADD PRIMARY KEY (bucket_seconds, bucket_start, platform, group_id, model, user_id, metric, upper_bound_ms, source);

ALTER TABLE channel_monitor_v2_metrics_1m
    ADD COLUMN IF NOT EXISTS usage_incomplete_requests BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS cost_incomplete_requests BIGINT NOT NULL DEFAULT 0;
ALTER TABLE channel_monitor_v2_metrics_rollup
    ADD COLUMN IF NOT EXISTS usage_incomplete_requests BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS cost_incomplete_requests BIGINT NOT NULL DEFAULT 0;

ALTER TABLE channel_monitor_v2_watermarks
    ADD COLUMN IF NOT EXISTS accounting_version INTEGER NOT NULL DEFAULT 1;
-- Do not scan or truncate request history in a migration. Coverage is invalidated
-- atomically, so reads only expose recomputed ranges under the new semantics.
-- Leave the marker at 1 until the new worker commits its first version-2 range;
-- an older worker still running during a rolling upgrade cannot claim new coverage.
UPDATE channel_monitor_v2_watermarks
SET accounting_version = 1, usage_coverage_start = NULL, error_coverage_start = NULL,
    data_through = NULL, last_successful_at = NULL, backfill_cursor = NULL,
    updated_at = NOW()
WHERE accounting_version < 2;
COMMENT ON TABLE channel_monitor_v2_metrics_1m IS
    'Actual business and probe API generation request facts, preserving their source; never task-summary observations.';
