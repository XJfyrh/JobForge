-- Purpose: Add request_hash to jobs for idempotency key conflict detection
-- (ADR-0002 CONFLICT). The hash canonicalizes the submit request (queue,
-- type, payload, priority, run_at, max_attempts, timeout_seconds) so Enqueue
-- can distinguish identical resubmissions (deduplicate, return the existing
-- job) from same-key-different-parameter conflicts (rejected with CONFLICT /
-- HTTP 409).
-- Lock behavior: ALTER TABLE ADD COLUMN for a nullable column without a
-- default takes a brief ACCESS EXCLUSIVE lock and does not rewrite the table.
-- Data risk: None (new nullable column; rows created before this migration
-- keep NULL and preserve legacy deduplicate semantics).
-- Rollback: Drop the column (see down migration).
-- Verification: \d jobs

alter table jobs add column request_hash text;

comment on column jobs.request_hash is 'sha256 of canonical submit request; NULL = legacy.';
