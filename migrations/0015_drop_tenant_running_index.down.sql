-- Rollback for migration 0015: restore the running-only quota index with the
-- same definition as migration 0012.
create index if not exists idx_jobs_tenant_running on jobs (tenant_id)
where state = 'running';
