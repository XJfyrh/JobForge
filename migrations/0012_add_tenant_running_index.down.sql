-- Purpose: Revert migration 0012 (tenant running quota partial index).
-- Data risk: None; the quota count falls back to a sequential scan.
-- Rollback: Re-apply the up migration.
-- Verification: \di+ jobs_*

drop index if exists idx_jobs_tenant_running;
