-- Purpose: Revert migration 0009 (claimed_at column).
-- Data risk: Drops the claimed_at column and its data; the publisher fetch
-- falls back to plain FOR UPDATE SKIP LOCKED autocommit semantics.
-- Rollback: Re-apply the up migration.
-- Verification: \d outbox_events

alter table outbox_events drop column claimed_at;
