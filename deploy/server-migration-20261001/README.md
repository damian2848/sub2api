# Gpt56 Production Migration

This directory archives the migration and cleanup performed on 2026-10-01. The scripts contain deployment-specific paths, checkpoints and container identities; they are not a repeatable installer. Review the current server state before using them, and never rerun seeding, fencing, promotion or cleanup as a routine deployment command. Runtime receipts, credentials and backups are intentionally excluded from Git.

Migration completed on 2026-10-01. Production runs independently on `root@23.132.132.111:40301`, under `/opt/sub2api-gpt56`.

Initial migration release: `0.2.11`, commit `7bb09518a1167295ae0a79382f27d08911404b3e`, image `sub2api-gpt56:0.2.11-7bb09518a`. The running binary was subsequently updated to `0.2.12`, commit `8b2e7d5cc88e605a3642a3bb24b29ab4eea97f85`, before container cleanup. Cleanup preserved that running production build; its image tag still carries the original migration version.

## Production

- `sub2api.gpt56.site` and `image2api.gpt56.site` resolve to the new server and serve the migrated Sub2API instance over HTTPS. `gpt56.site` now returns a 308 redirect to `sub2api.gpt56.site`, preserving paths and query parameters; it does not directly proxy a container. The source had no running independent Image2API service.
- Application: `gpt56-production-app`, loopback `18080`.
- PostgreSQL: `gpt56-postgres-standby`, loopback `15432`. Its historical container name remains, but it is now the writable primary, outside recovery.
- Redis: `gpt56-redis-standby`, loopback `16379`. It is now the primary; the on-disk configuration no longer contains `replicaof` or `masterauth`.
- Video adapter and upstream relay: `gpt56-production-grok-video`, ports `18081` and `18082`.
- Async video console: `gpt56-production-video-console`, port `4178`.
- Production Compose: `production/compose.json`, profile `cutover`.
- Active proxy configuration: `/etc/nginx/sites-available/gpt56-migration.conf`. Video routes, admin injection, WebSockets, streaming, 256 MB requests and headers containing underscores are preserved.
- TLS: `/etc/letsencrypt/live/gpt56-migration/`; certificate covers all three names and initially expires 2026-12-30. `gpt56-tls-renew.timer` checks renewal twice daily and reloads Nginx.

The retired `/opt/sub2api` bootstrap deployment and isolated preview containers were removed during the subsequent cleanup. Their bind-mounted data and private cleanup backups were preserved. Active KaiyunCode deployments remain separate.

## Data Handover

The old applications and helper writers were stopped before promotion. Source PostgreSQL was fenced read-only, and Redis writes were paused for the final replication checkpoint. The new PostgreSQL replayed beyond final source LSN `5/5F2C3450`. Redis caught up beyond offset `26446313`.

Final snapshot: 2 users, 197 account rows including deleted records, 20 API keys, 316 schema migrations and 275,511 usage logs. User, account, key and setting fingerprints matched before production startup. Existing credentials, JWT and TOTP keys were retained.

Maintenance began at `06:59:05 UTC`; the new server began serving at `07:01:30 UTC`, approximately 145 seconds later. A temporary bridge also returned 502 before its SSH forwarding and certificate handling were fixed. This was not a zero-downtime migration. The old DNS entrypoint's separate certificate-chain error was subsequently fixed and its forwarded health check verified as 200.

The old `43.128.10.245` HTTPS entrypoint now forwards to the new server for clients with cached DNS. The new server trusts forwarded client addresses only from that old entrypoint, and does not depend on the old databases or application.

Old app/helper containers and Redis remain stopped with restart disabled. Old PostgreSQL remains a read-only archive. The migration replication slot was removed, its source login disabled and temporary authorized SSH keys removed. One-way file synchronization and both temporary SSH tunnel services are disabled.

## Checks And Operation

```bash
python3 /opt/sub2api-gpt56/verify_target.py
systemctl status gpt56-sync-monitor.timer gpt56-tls-renew.timer gpt56-backup.timer
docker compose -f /opt/sub2api-gpt56/production/compose.json --profile cutover ps
docker compose -f /opt/sub2api-gpt56/live/compose.json ps
```

`gpt56-sync-monitor.timer` now verifies production every minute despite its historical name. It writes `production/health.json` and systemd status; it does not send external notifications. Health/readiness, primary database roles, container health and independence from the old server are checked. The full verification also checks existing admin authentication, frontend, domains and video console. Existing API key model listing was separately verified as 200; no billable generation was made for verification.

The old `verify_staging.py`, `verify_source.py`, initial controller and seeding scripts are historical migration tools. Do not rerun them against the promoted production. Promotion is intentionally not repeatable.

The database containers have restart policies and persist their primary roles. Production services also restart automatically. The Compose `cutover` profile must be included when managing those services manually.

## Backups And Recovery

- Pre-migration backup on source: `/root/sub2api-backups/server-migration-20261001`.
- Verified transferred backup on destination: `/opt/sub2api-gpt56/source-artifacts`.
- New production backups: `/opt/sub2api-gpt56/backups/`, generated daily at approximately 03:30 Asia/Shanghai, retaining seven complete runs.
- Each production backup contains a verified PostgreSQL custom-format dump, Redis RDB, application files, private deployment configuration and TLS files with SHA256 checksums. Database and cache snapshots are taken independently; Redis is a cache, not a transactional database backup.
- Cutover proof: `cutover-receipt.json`. Pre-cutover preview proof: `migration-receipt.json`.

Backups contain credentials and must remain restricted to root. Preserve the source and verified artifacts until operational acceptance. Once the new server receives writes, returning to the old database requires synchronization or restoring a current backup; changing DNS alone would lose new data. The old Nginx relay may be retired once DNS cache traffic has ceased.

## Container Cleanup

On 2026-10-01, removed the three stopped `gpt56-preview-*` containers, stopped `kaiyuncode-migration-rehearsal`, and the unused bootstrap `sub2api`, `sub2api-postgres`, `sub2api-redis` containers. The bootstrap database had no usage since the migration. A verified database dump, Redis snapshot, container configuration and Compose files were archived at `/opt/sub2api-gpt56/cleanup/20261001T104237Z`; original bind mounts and images remain. The empty `gpt56-preview_isolated` and `sub2api_sub2api-network` networks were removed.

The old bootstrap Nginx route to port 8080 was replaced with a default HTTP 404 and TLS rejection for unknown hosts, while retaining the ACME webroot. Domain production routes, KaiyunCode routes and their certificates were preserved. The five production Sub2API container IDs were checked unchanged before and after cleanup.

The user confirmed that the old source server `43.128.10.245` is powered off. Its disks retain the archived data and containers; no remote container deletion was performed on that powered-off server. The new production operates independently.
