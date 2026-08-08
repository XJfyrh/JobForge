-- Purpose: Create the scheduler_leadership singleton row backing the
-- Scheduler leadership lease (ADR-0005). The advisory lock remains the
-- fast-path mutual exclusion; this row is the source of truth for who the
-- leader is: leader heartbeats refresh last_seen, standbys take over when
-- it goes stale, and epoch fences resurrected old leaders.
-- Lock behavior: CREATE TABLE + single-row INSERT on a new table only.
-- Data risk: None (new table, seeded with the singleton row).
-- Rollback: Drop the table (see down migration).
-- Verification: \d scheduler_leadership

create table scheduler_leadership (
    id smallint primary key check (id = 1),
    leader_id text,
    epoch bigint not null default 0,
    last_seen timestamptz not null default now()
);

comment on table scheduler_leadership is 'leadership lease (ADR-0005); NULL = no leader.';

-- Seed the singleton row; leadership claims upsert against it.
insert into scheduler_leadership (id) values (1);
