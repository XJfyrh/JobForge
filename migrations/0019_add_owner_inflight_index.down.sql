-- Rollback for migration 0019: remove only the Gateway Poll performance index.
drop index if exists idx_jobs_owner_inflight;
