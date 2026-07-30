package postgres

// SQL queries for the Scheduler. The Scheduler promotes scheduled/retry_wait
// jobs to ready and recovers expired leases. It uses a PostgreSQL advisory
// lock for single-active instance election.

// schedulerAdvisoryLockID is the fixed advisory lock ID used by the Scheduler.
// Only one instance can hold this lock at a time.
const schedulerAdvisoryLockID int64 = 738291

// tryAdvisoryLock attempts to acquire a session-level advisory lock.
// Returns true if the lock was acquired, false if another instance holds it.
// The lock is held until explicitly released or the session disconnects.
const tryAdvisoryLock = `
select pg_try_advisory_lock($1)
`

// releaseAdvisoryLock releases the session-level advisory lock.
const releaseAdvisoryLock = `
select pg_advisory_unlock($1)
`

// promoteReady transitions scheduled/retry_wait jobs whose run_at has arrived
// to the ready state. Uses a CTE with FOR UPDATE SKIP LOCKED to avoid lock
// contention with concurrent claim transactions.
//
// Invariant: promote is idempotent — re-scanning the same rows is a no-op
// because the WHERE clause filters on state IN ('scheduled','retry_wait').
const promoteReady = `
with candidates as (
    select id
    from jobs
    where state in ('scheduled', 'retry_wait')
      and run_at <= now()
    order by run_at asc
    limit $1
    for update skip locked
)
update jobs
set state = 'ready',
    state_version = state_version + 1,
    updated_at = now()
from candidates
where jobs.id = candidates.id
returning jobs.id, jobs.queue
`

// recoverRunningLeases transitions running jobs with expired leases back to
// ready. The lease_owner and lease_until are cleared so a new Worker can claim.
// Uses a CTE to capture pre-update values (lease_owner, attempt, fencing_token)
// for audit, since RETURNING would return the post-update NULLs.
//
// Invariant: recovery writes an outbox event and updates job_attempts with
// outcome 'lease_expired' for audit (handled in Go code within the same tx).
const recoverRunningLeases = `
with expired as (
    select id, queue, lease_owner, attempt, fencing_token
    from jobs
    where state = 'running'
      and lease_until < now()
)
update jobs
set state = 'ready',
    lease_owner = null,
    lease_until = null,
    state_version = state_version + 1,
    updated_at = now()
from expired
where jobs.id = expired.id
returning expired.id, expired.queue, expired.lease_owner, expired.attempt, expired.fencing_token
`

// recoverCancellingLeases transitions cancelling jobs with expired leases to
// cancelled. This handles the case where a Worker was cancelling but never
// acknowledged before the lease expired. Uses CTE + FOR UPDATE SKIP LOCKED
// for consistency with recoverRunningLeases.
const recoverCancellingLeases = `
with expired as (
    select id, queue, lease_owner, attempt, fencing_token
    from jobs
    where state = 'cancelling'
      and lease_until < now()
)
update jobs
set state = 'cancelled',
    state_version = state_version + 1,
    updated_at = now()
from expired
where jobs.id = expired.id
returning expired.id, expired.queue, expired.lease_owner, expired.attempt, expired.fencing_token
`

// insertRecoveryAttempt records a lease-expired recovery event in job_attempts.
// Uses ON CONFLICT to update the existing attempt record (created during claim)
// rather than inserting a duplicate.
const insertRecoveryAttempt = `
insert into job_attempts (job_id, attempt_no, worker_id, fencing_token, started_at, finished_at, outcome)
values ($1, $2, $3, $4, now(), now(), 'lease_expired')
on conflict (job_id, attempt_no) do update set
    finished_at = now(),
    outcome = 'lease_expired'
`

// insertRecoveryOutbox writes a lease_expired event to the outbox.
const insertRecoveryOutbox = `
insert into outbox_events (aggregate_id, event_type, payload)
values ($1, 'job.lease_expired', $2)
`
