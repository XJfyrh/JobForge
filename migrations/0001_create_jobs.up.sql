-- Purpose: Create the core jobs table for the JobForge task queue.
-- Lock behavior: CREATE TABLE acquires exclusive lock on the new table only.
-- Data risk: None (new table, no existing data).
-- Rollback: Drop the table (see down migration).
-- Verification: \d jobs; verify indexes with \di+ jobs_*.

create table jobs (
    id uuid primary key,
    tenant_id text not null,
    queue text not null,
    type text not null,
    payload jsonb not null default '{}',
    priority smallint not null default 0,
    state text not null default 'ready'
    check (state in (
        'scheduled', 'ready', 'running', 'cancelling',
        'retry_wait', 'succeeded', 'dead', 'cancelled'
    )),
    run_at timestamptz not null default now(),
    attempt integer not null default 0,
    max_attempts integer not null default 3
    check (max_attempts between 1 and 10),
    timeout_seconds integer not null default 300,
    idempotency_key text,
    lease_owner text,
    lease_until timestamptz,
    fencing_token bigint not null default 0,
    cancel_requested_at timestamptz,
    trace_id text,
    state_version bigint not null default 1,
    retry_of_job_id uuid references jobs (id),
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now()
);

-- Partial index for claim queries: only ready jobs need to be scanned.
-- Supports: SELECT ... WHERE queue=$1 AND state='ready' AND run_at <= now()
-- ORDER BY priority DESC, created_at ASC LIMIT $n FOR UPDATE SKIP LOCKED
create index idx_jobs_claim on jobs (queue, priority desc, created_at asc)
where state = 'ready';

-- Partial index for lease expiry scans by the Scheduler.
-- Supports: SELECT ... WHERE state IN ('running','cancelling') AND lease_until < now()
create index idx_jobs_lease_expiry on jobs (lease_until)
where state in ('running', 'cancelling');

-- Index for tenant-scoped queries ordered by creation time.
create index idx_jobs_tenant_created on jobs (tenant_id, created_at desc);

-- Unique constraint for submission idempotency.
-- Only applies when idempotency_key is not null.
create unique index idx_jobs_idempotency on jobs (tenant_id, idempotency_key)
where idempotency_key is not null;
