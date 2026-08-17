-- Persistent business effects for demo.idempotent_effect (PRD v0.4 FR-801).
--
-- Lock behavior: this migration only creates a new table and does not lock or
-- rewrite jobs, outbox_events, or other existing application tables.
-- Data risk: forward migration is additive. Rollback deletes all demo effect
-- evidence; it does not modify job state or outbox history.
-- Verification:
--   SELECT to_regclass('public.demo_idempotent_effects');
--   SELECT job_id, result_ref, applied_at
--     FROM demo_idempotent_effects ORDER BY applied_at DESC LIMIT 10;
-- The table deliberately has no foreign key to jobs: it represents a separate
-- business-effect store and must never become a task-state fact source.

create table demo_idempotent_effects (
    job_id uuid primary key,
    result_ref text not null,
    applied_at timestamptz not null default now()
);
