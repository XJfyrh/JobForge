-- Rollback: remove trace_context column added in 0006.
-- Lock risk: DROP COLUMN is non-blocking in PostgreSQL (marks column invisible).
-- Data risk: trace_context data is lost; legacy trace_id column is unaffected.

alter table jobs
  drop column if exists trace_context;
