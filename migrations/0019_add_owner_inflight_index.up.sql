-- Purpose: Accelerate the Gateway Poll owner inflight count introduced by
-- PRD v0.5 FR-905. The query filters one lease_owner and the two inflight
-- states while holding the corresponding workers row lock; this partial
-- index prevents unrelated inflight jobs from extending that transaction.
-- Lock behavior: CREATE INDEX runs inside the transactional migrator and
-- takes a SHARE lock on jobs, blocking writes (not reads) while it builds.
-- Large production tables should schedule the migration during a bounded
-- maintenance window because this migrator cannot use CREATE INDEX CONCURRENTLY.
-- Data risk: None (new index only; no rows or task states are changed).
-- Rollback: Drop the index; Poll correctness remains jobs-derived but returns
-- to the pre-0019 scan cost (see down migration).
-- Verification: EXPLAIN (ANALYZE, BUFFERS) the owner inflight count and verify
-- idx_jobs_owner_inflight serves the scan.

create index if not exists idx_jobs_owner_inflight on jobs (lease_owner)
where lease_owner is not null
and state in ('running', 'cancelling');
