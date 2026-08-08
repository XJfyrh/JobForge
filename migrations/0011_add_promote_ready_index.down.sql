-- Purpose: Revert migration 0011 (promote partial index).
-- Data risk: None; the promote scan falls back to a sequential scan.
-- Rollback: Re-apply the up migration.
-- Verification: \di+ jobs_*

drop index if exists idx_jobs_promote_ready;
