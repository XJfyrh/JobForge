-- Purpose: Serve the inflight-caliber queries introduced with the quota
-- counter redesign (PRD v0.3 FR-721, ADR-0007 §5): per-tenant reconcile
-- aggregates, the AT-21 sampling assertion and counter rebuilds all filter on
-- state IN ('running','cancelling'). This partial index supersedes
-- idx_jobs_tenant_running (migration 0012), whose sole consumer
-- (claimTenantRunningCount, running-only caliber) is removed by the counter
-- table implementation; the old index is dropped in migration 0015.
-- Lock behavior: CREATE INDEX CONCURRENTLY is not usable inside a migration
-- transaction; a plain CREATE INDEX takes a SHARE lock on jobs, blocking
-- writes (not reads) while the index builds. At MVP scale this is acceptable;
-- very large production tables should build the index out-of-band.
-- Data risk: None (new index only; no data change).
-- Rollback: Drop the index (see down migration).
-- Verification: \di+ jobs_*

create index if not exists idx_jobs_tenant_inflight on jobs (tenant_id)
where state in ('running', 'cancelling');
