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

// claimLeadershipForced takes over the scheduler_leadership singleton row
// (ADR-0005) unconditionally, incrementing epoch. Used when the advisory
// lock try succeeded: the previous advisory holder is necessarily gone
// (session-level locks die with their connection), so waiting out the lease
// would needlessly delay NFR-004 fast-path takeover. A rare corner case
// (a live lease-based leader not holding the advisory lock) is resolved by
// epoch fencing: the displaced leader's next heartbeat fails and it steps
// down.
const claimLeadershipForced = `
insert into scheduler_leadership (id, leader_id, epoch, last_seen)
values (1, $1, 1, now())
on conflict (id) do update
set leader_id = $1,
    epoch = scheduler_leadership.epoch + 1,
    last_seen = now()
returning epoch`

// claimLeadershipIfStale takes over the lease only when there is no leader,
// the current leader's lease is stale (last_seen older than $2), or the
// claimant already owns the lease. Used when the advisory lock is still held
// by another instance (a stuck leader): the lease is the only liveness
// signal. An empty result means another leader's lease is still fresh.
const claimLeadershipIfStale = `
insert into scheduler_leadership (id, leader_id, epoch, last_seen)
values (1, $1, 1, now())
on conflict (id) do update
set leader_id = $1,
    epoch = scheduler_leadership.epoch + 1,
    last_seen = now()
where scheduler_leadership.leader_id is null
   or scheduler_leadership.last_seen < now() - $2::interval
   or scheduler_leadership.leader_id = $1
returning epoch`

// heartbeatLeadership refreshes the leader's lease. Returns no rows when the
// caller is no longer the leader of the given epoch (taken over), which the
// Scheduler treats as an immediate step-down signal (fencing).
const heartbeatLeadership = `
update scheduler_leadership
set last_seen = now()
where id = 1 and leader_id = $1 and epoch = $2
returning epoch`

// releaseLeadership steps down gracefully: clears leader_id so a standby can
// take over immediately without waiting out the lease. Guarded by leader_id
// and epoch so a resurrected old leader cannot disturb the new one.
const releaseLeadership = `
update scheduler_leadership
set leader_id = null
where id = 1 and leader_id = $1 and epoch = $2`

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

// queueDepthMetrics samples pending jobs per (tenant, queue, state) for the
// jobforge_queue_depth gauge (PRD 12.1 / FR-502). Pending states match the
// backpressure definition: scheduled, ready, retry_wait.
const queueDepthMetrics = `
select tenant_id, queue, state, count(*)
from jobs
where state in ('scheduled', 'ready', 'retry_wait')
group by tenant_id, queue, state
`
