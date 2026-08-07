-- Purpose: Add claimed_at to outbox_events for atomic publisher claiming.
-- The publisher's fetch is a single-statement atomic claim (UPDATE ... FOR
-- UPDATE SKIP LOCKED sets claimed_at), so concurrent publishers cannot pick
-- up the same event. Claims that stay unpublished past the claim TTL
-- (publisher crashed after claiming) become eligible for reclaim.
-- Lock behavior: ALTER TABLE ADD COLUMN for a nullable column without a
-- default takes a brief ACCESS EXCLUSIVE lock and does not rewrite the table.
-- Data risk: None (new nullable column; existing rows keep NULL, which
-- means unclaimed under the fetch condition).
-- Rollback: Drop the column (see down migration).
-- Verification: \d outbox_events

alter table outbox_events add column claimed_at timestamptz;

comment on column outbox_events.claimed_at is 'atomic publisher claim time; NULL = unclaimed.';
