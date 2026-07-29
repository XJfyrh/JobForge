-- Purpose: Create workers table for Worker registration and health tracking.
-- Lock behavior: CREATE TABLE acquires exclusive lock on new table only.
-- Data risk: None (new table).
-- Rollback: Drop the table.
-- Verification: \d workers
-- Note: W1 only creates the table. Worker registration logic is in W2.

create table workers (
    worker_id text primary key,
    instance_id text not null,
    supported_types text [] not null default '{}',
    queues text [] not null default '{}',
    capacity integer not null default 1,
    inflight integer not null default 0,
    last_heartbeat_at timestamptz,
    status text not null default 'active'
    check (status in ('active', 'draining', 'offline')),
    started_at timestamptz not null default now(),
    version text not null default ''
);

-- Index for finding active workers by queue.
create index idx_workers_queue on workers using gin (queues)
where status = 'active';
