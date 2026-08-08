-- Purpose: Accelerate the Scheduler's promote scan. promoteReady selects
-- WHERE state IN ('scheduled','retry_wait') AND run_at <= now() ORDER BY
-- run_at ASC LIMIT n FOR UPDATE SKIP LOCKED; run_at leads the partial index
-- so the scan is ordered and stops at the limit without a sort, covering
-- both states in a single index.
-- Lock behavior: CREATE INDEX CONCURRENTLY is not usable inside a migration
-- transaction; a plain CREATE INDEX takes a SHARE lock on jobs, blocking
-- writes (not reads) while the index builds. At MVP scale this is acceptable;
-- very large production tables should build the index out-of-band.
-- Data risk: None (new index only; no data change).
-- Rollback: Drop the index (see down migration).
-- Verification: \di+ jobs_*

create index if not exists idx_jobs_promote_ready on jobs (run_at)
where state in ('scheduled', 'retry_wait');
