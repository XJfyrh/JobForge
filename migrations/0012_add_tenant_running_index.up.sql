-- Purpose: Accelerate the tenant quota count executed inside the Claim
-- transaction: SELECT count(*) FROM jobs WHERE tenant_id = $1 AND state =
-- 'running' (FR-302). Without an index this is a sequential scan per claimed
-- candidate, extending the row-lock hold time of the claim transaction; the
-- partial index makes it an index-only scan bounded by the tenant's running
-- jobs.
-- Lock behavior: CREATE INDEX CONCURRENTLY is not usable inside a migration
-- transaction; a plain CREATE INDEX takes a SHARE lock on jobs, blocking
-- writes (not reads) while the index builds. At MVP scale this is acceptable;
-- very large production tables should build the index out-of-band.
-- Data risk: None (new index only; no data change).
-- Rollback: Drop the index (see down migration).
-- Verification: \di+ jobs_*

create index if not exists idx_jobs_tenant_running on jobs (tenant_id)
where state = 'running';
