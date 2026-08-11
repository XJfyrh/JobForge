-- Purpose: Add envelope-support columns to outbox_events (PRD v0.3 FR-703,
-- ADR-0006 §4). aggregate_version carries jobs.state_version at event write
-- time so consumers can detect duplicates/reordering per aggregate;
-- traceparent carries the job's serialized W3C TraceContext captured in the
-- same transaction as the state transition, so the publisher can build
-- envelope v1 without joining jobs at publish time.
-- Lock behavior: ALTER TABLE ADD COLUMN for a nullable column without a
-- default takes a brief ACCESS EXCLUSIVE lock and does not rewrite the table
-- (same pattern as migration 0009).
-- Data risk: None (new nullable columns; legacy rows keep NULL, which the
-- envelope renders as aggregate_version 0 = unknown and empty traceparent).
-- Rollback: Drop both columns (see down migration).
-- Verification: \d outbox_events

alter table outbox_events add column aggregate_version bigint;
alter table outbox_events add column traceparent text;

comment on column outbox_events.aggregate_version is
'jobs.state_version captured at event write time; NULL for legacy rows (unknown).';

comment on column outbox_events.traceparent is
'Serialized W3C TraceContext (traceparent header) of the originating job transaction;
NULL when absent.';
