-- Rollback for migration 0014: drop the inflight partial index.
drop index if exists idx_jobs_tenant_inflight;
