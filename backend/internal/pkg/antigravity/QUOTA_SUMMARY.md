# Antigravity quota windows

The account usage view uses `POST /v1internal:retrieveUserQuotaSummary` with
`{"project":"<project_id>"}` and the account's OAuth bearer token. This endpoint
supplies the independent quota groups and their rolling windows. It is separate
from `fetchAvailableModels`, whose model-level fractions do not identify a
5-hour or weekly window.

Account usage must call `RetrieveUserQuotaSummaryFromURL` with the same fixed
endpoint selected by `resolveAntigravityForwardBaseURL(account)` for generation:
the configured override takes priority, paid plans default to daily, and other
plans default to prod. The model catalog must use `FetchAvailableModelsFromURL`
with that same endpoint. Read-only comparisons confirmed that prod and daily can
return different remaining fractions and reset times for the same account;
querying prod while generation uses daily can therefore show a misleading full
quota. A successful response from another endpoint is not an equivalent retry.

The fixed summary query does not follow redirects or fall back across endpoints.
The default `RetrieveUserQuotaSummary` wrapper retains the existing `BaseURLs`
order for callers without an account-selected endpoint. Neither summary path
updates `DefaultURLAvailability`: a quota lookup must not change forwarding
priority or clear a generation endpoint's unavailable marker.

The contract was checked against the embedded protobuf descriptors in the
official Antigravity 2.24.0 application on 2026-10-20:

- `google/internal/cloud/code/v1internal/quota_summary.proto` defines the request
  and response.
- `prediction_service.proto` maps `PredictionService/RetrieveUserQuotaSummary`
  to the REST POST endpoint above with HTTP body `*`.
- `groups[]` contains `displayName`, `description`, and `buckets[]`.
- Buckets contain `bucketId`, `displayName`, `description`, `window`, `disabled`,
  and RFC3339 `resetTime`.
- The protobuf `remaining` oneof is flattened in REST JSON:
  `remainingFraction` (float) **or** `remainingAmount` (int64, normally a JSON
  string). Missing fractional usage is unknown, not zero remaining.
- The deprecated top-level `buckets[]` remains supported when groups are absent.

Group names, bucket IDs, and window labels are upstream strings, not a fixed
model-family enum. Preserve every group, including a separate Argon pool when
provided. Never merge buckets by model name or pair a percentage with another
bucket's reset time. The test values for Gemini, GPT/Claude, and Argon are
synthetic fixtures using this verified schema, not captured account quotas.

The normalized API adds `antigravity_quota_groups`, `antigravity_quota_source`,
and `antigravity_quota_error`. Legacy per-model data remains available, explicitly
marked as having no known period if the summary is unavailable. The monitor
cannot report healthy quota from that fallback or from a summary with no usable
numeric windows.

Manual account/batch refreshes and the per-account query button pass `force=true`
through both browser and service caches. Concurrent refreshes share an upstream
request; each caller receives an independent snapshot. `updated_at` remains the
actual query time on cache hits, and older browser responses cannot replace a
newer snapshot. Summary failures use the shorter error cache lifetime.

Relevant checks:

```sh
cd backend
go test -tags=unit ./internal/pkg/antigravity
go test -race -tags=unit ./internal/service -run 'Test(Antigravity|FetchAntigravityQuota|BuildUsageInfo|FetchQuota|MonitorQuotaUsesAntigravity|UsageQuotaTiers|QuotaFetcher|UsageFailureInfo)'
```

Frontend regression tests cover grouped windows, missing quotas, legacy models,
force refresh, stale responses, and account changes in `AccountUsageCell.spec.ts`
and `AccountsView.usageRefresh.spec.ts`.

Client regression tests give prod and daily distinct quota fractions and verify
that a selected daily endpoint wins even when prod is first in `BaseURLs`. They
also cover failures and redirects without switching pools, and ensure quota
queries leave forwarding availability and priority unchanged.
