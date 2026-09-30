# gpt56 fork: sync upstream v0.2.11

Checked on 2026-10-01 (Asia/Shanghai).

## Source and scope

- Official repository: https://github.com/Wei-Shaw/sub2api
- Official latest release at verification: v0.2.11, published 2026-09-30 15:06:51 +08:00.
- Official main: 42bc7f6cffe24bcb471608e48e66b4a0afa1f882.
- Previous imported baseline: v0.2.9 (8532ec28b56188d3c845ed97070e6ee5136bd9bd); the fork previously imported it in PR ranxi2001/sub2api#179.
- Local starting point: production at de79d8ed5; this update is prepared on sync/upstream-v0.2.11.
- Integration uses the incremental v0.2.9-to-main patch. A direct merge from the historical Git merge base would reapply many cherry-picked changes.
- The fork's VERSION remains 2.9.8 until a separate release is prepared. No release tag or production deployment is part of this source sync.

## Included changes

- GPT-6.1 Sol, Claude Sonnet 5.5, updated Codex plans, remote model catalogs and Astra Ultrafast capability/pricing support.
- Claude reset-credit status and confirmed manual redemption, with idempotency and organization-level locking.
- Redis in-flight balance reservations and billing-task lifetime accounting to reduce concurrent overspending.
- API key creation limits, risk-control user allowlists, usage ranking by spending, and Claude Code-only client filtering/fallback.
- Composite WebSocket routing, account model ownership, stream usage accounting, and single-pass tool-name rewriting fixes.

## Preserved fork behavior

- BPS settings, native multi-agent capability restrictions, priority scheduling, account eligibility rechecks, and per-user denied models.
- Pelican APIs, request capture, credential operations, background-role controls, and owner-controlled updates/releases.
- Explicit-session identity isolation and strict identity mode. Allowlisted users bypass the local cyber gates consistently for HTTP and every WebSocket turn; upstream denials still propagate and are audited.
- Existing database migrations, dependency lockfiles and release workflows are unchanged.

## Defaults to review before deployment

- api_key_create.max_active_per_user defaults to 200; api_key_create.max_per_user_per_hour defaults to 60. Zero disables the corresponding limit.
- billing.inflight_reservation.enabled defaults to true. Low-balance users can be rejected earlier during concurrent requests; deployment can explicitly disable or tune this setting.
- This update adds no SQL migration. Deployment and rollback still need to follow the existing release process.

## Validation

- Frontend production build, TypeScript checking, locale completeness and ESLint passed with pnpm 9.15.9.
- Full frontend Vitest run passed: 396 files, 3,302 tests.
- Full backend unit suite passed with Go 1.27.0: go test -p 2 -tags=unit ./....
- Race detection passed for in-flight reservations, Claude reset redemption and cyber allowlist/session compatibility across service, handler and repository packages.
- Repository integration suites passed against isolated PostgreSQL 15 and Redis 8.4 containers: API key creation counters and usage-log queries, 49 subtests.
- Embedded web/server tests and a native macOS arm64 executable build passed.
- Wire regenerated the dependency graph successfully; migration files and dependency lockfiles are unchanged.
- A scheduler profit test now uses a 1e-12 tolerance for floating-point arithmetic instead of exact equality. Production score/profit calculations are unchanged.
- No production smoke test or release publishing was performed. The standalone golangci-lint executable was not available locally; Go test's vet checks were exercised.
The frontend critical CI set now includes Claude reset credits, Codex key configuration, model whitelist mappings, plan badges, dashboard ranking and Codex API tests.
