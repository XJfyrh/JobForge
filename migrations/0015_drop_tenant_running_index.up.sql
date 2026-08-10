-- Purpose: Drop idx_jobs_tenant_running (migration 0012). Its only consumer
-- was claimTenantRunningCount, the per-candidate running-only count executed
-- inside the Claim transaction; the quota counter table (migration 0013,
-- ADR-0007) replaces that query entirely. Keeping the index would only add
-- write amplification. Separate migration from 0014 for rollback granularity:
-- the down migration restores the old index if the counter implementation is
-- rolled back first.
-- Lock behavior: DROP INDEX takes an ACCESS EXCLUSIVE lock on the index
-- only, briefly; job table reads/writes are not blocked beyond that.
-- Data risk: None (index only; no data change).
-- Rollback: Recreate the index (see down migration).
-- Verification: \di+ jobs_* (idx_jobs_tenant_running absent)

drop index if exists idx_jobs_tenant_running;
