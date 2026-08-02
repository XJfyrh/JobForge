-- Purpose: Add trace_context column for W3C TraceContext propagation across
-- process boundaries (API -> Scheduler -> Gateway -> Worker).
-- Lock risk: ADD COLUMN with nullable default is non-blocking (no table rewrite).
-- Data risk: None; existing rows get NULL trace_context (legacy trace_id still works).
-- Rollback: DROP COLUMN trace_context (see down migration).
-- Verification: SELECT column_name FROM information_schema.columns
--   WHERE table_name = 'jobs' AND column_name = 'trace_context';

alter table jobs
  add column if not exists trace_context text;

comment on column jobs.trace_context is
  'Serialized W3C TraceContext (traceparent header format) for cross-process OTel span propagation. Nullable for backward compatibility with W5 legacy trace_id.';
