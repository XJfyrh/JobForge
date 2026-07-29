-- Purpose: Create job_attempts table for execution audit trail.
-- Lock behavior: CREATE TABLE acquires exclusive lock on new table only.
-- Data risk: None (new table).
-- Rollback: Drop the table.
-- Verification: \d job_attempts

create table job_attempts (
    id bigint generated always as identity primary key,
    job_id uuid not null references jobs (id),
    attempt_no integer not null,
    worker_id text not null,
    fencing_token bigint not null,
    started_at timestamptz not null default now(),
    finished_at timestamptz,
    outcome text
    check (outcome in (
        'succeeded', 'failed_retry', 'failed_dead',
        'cancelled', 'lease_expired'
    )),
    error_code text,
    error_message text,
    duration_ms bigint,
    trace_id text
);

-- Index for fetching attempt timeline of a specific job.
create index idx_job_attempts_job on job_attempts (job_id, attempt_no);

-- Unique constraint: one record per job per attempt number.
create unique index idx_job_attempts_unique on job_attempts (job_id, attempt_no);
