-- Purpose: Create outbox_events table for transactional event publishing.
-- Lock behavior: CREATE TABLE acquires exclusive lock on new table only.
-- Data risk: None (new table).
-- Rollback: Drop the table.
-- Verification: \d outbox_events
-- Note: P0 only persists events. P1 publishes to JetStream via Outbox Publisher.

create table outbox_events (
    event_id bigint generated always as identity primary key,
    aggregate_id uuid not null,
    event_type text not null,
    payload jsonb not null default '{}',
    created_at timestamptz not null default now(),
    published_at timestamptz,
    publish_attempts integer not null default 0
);

-- Index for the outbox publisher to find unpublished events.
create index idx_outbox_unpublished on outbox_events (created_at)
where published_at is null;

-- Index for querying events by aggregate (job).
create index idx_outbox_aggregate on outbox_events (aggregate_id, created_at);
