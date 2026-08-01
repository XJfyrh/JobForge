package postgres

// SQL queries for the PostgreSQL job store. Each query is annotated with its
// invariants, locking behavior and expected transaction context.

// enqueueInsert inserts a new job. The ON CONFLICT clause handles idempotency:
// if the same (tenant_id, idempotency_key) already exists, we do nothing and
// then fetch the existing row.
const enqueueInsert = `
insert into jobs (
    id, tenant_id, queue, type, payload, priority, state,
    run_at, attempt, max_attempts, timeout_seconds,
    idempotency_key, fencing_token, trace_id, state_version,
    retry_of_job_id, created_at, updated_at
) values (
    $1, $2, $3, $4, $5, $6, $7,
    $8, $9, $10, $11,
    $12, $13, $14, $15,
    $16, $17, $18
)
on conflict (tenant_id, idempotency_key) where idempotency_key is not null
do nothing
`

// enqueueSelectByID fetches a job by ID after insert (or conflict).
const enqueueSelectByID = `
select id, tenant_id, queue, type, payload, priority, state,
       run_at, attempt, max_attempts, timeout_seconds,
       idempotency_key, lease_owner, lease_until, fencing_token,
       cancel_requested_at, trace_id, state_version, retry_of_job_id,
       created_at, updated_at,
       (xmax = 0) as inserted
from jobs
where id = $1
`

// getByID fetches a single job scoped to a tenant.
const getByID = `
select id, tenant_id, queue, type, payload, priority, state,
       run_at, attempt, max_attempts, timeout_seconds,
       idempotency_key, lease_owner, lease_until, fencing_token,
       cancel_requested_at, trace_id, state_version, retry_of_job_id,
       created_at, updated_at
from jobs
where id = $1 and tenant_id = $2
`

// claimSelect locks up to N ready jobs for a queue using SKIP LOCKED.
// This prevents concurrent Workers from claiming the same job.
//
// Invariant: FOR UPDATE SKIP LOCKED ensures each row is locked by at most one
// transaction. The partial index on (queue, state, run_at, priority DESC,
// created_at) WHERE state='ready' supports this query efficiently.
const claimSelect = `
select id, tenant_id, queue, type, payload, priority, state,
       run_at, attempt, max_attempts, timeout_seconds,
       idempotency_key, lease_owner, lease_until, fencing_token,
       cancel_requested_at, trace_id, state_version, retry_of_job_id,
       created_at, updated_at
from jobs
where queue = $1
  and state = 'ready'
  and run_at <= now()
  and ($2::text[] is null or type = any($2))
order by priority desc, created_at asc
limit $3
for update skip locked
`

// claimUpdate updates the claimed job's lease fields atomically.
// Called within the same transaction as claimSelect.
const claimUpdate = `
update jobs
set state = 'running',
    lease_owner = $2,
    lease_until = $3,
    attempt = attempt + 1,
    fencing_token = fencing_token + 1,
    state_version = state_version + 1,
    updated_at = now()
where id = $1
returning id, tenant_id, queue, type, payload, priority, state,
          run_at, attempt, max_attempts, timeout_seconds,
          idempotency_key, lease_owner, lease_until, fencing_token,
          cancel_requested_at, trace_id, state_version, retry_of_job_id,
          created_at, updated_at
`

// claimInsertAttempt records the start of a new attempt.
const claimInsertAttempt = `
insert into job_attempts (job_id, attempt_no, worker_id, fencing_token, started_at, trace_id)
values ($1, $2, $3, $4, now(), $5)
`

// heartbeatUpdate extends the lease. Matches owner + token + active state.
// Returns 0 rows if the lease is stale (owner/token mismatch or state changed).
const heartbeatUpdate = `
update jobs
set lease_until = $4,
    updated_at = now()
where id = $1
  and lease_owner = $2
  and fencing_token = $3
  and state in ('running', 'cancelling')
`

// completeUpdate transitions running → succeeded.
// Matches owner + token + state. Rejects cancelling state (cancel wins race).
const completeUpdate = `
update jobs
set state = 'succeeded',
    state_version = state_version + 1,
    updated_at = now()
where id = $1
  and lease_owner = $2
  and fencing_token = $3
  and state = 'running'
`

// completeRejectCancelling checks if the job is in cancelling state (cancel won
// the race). Used to return CANCEL_REQUESTED instead of STALE_LEASE.
const completeRejectCancelling = `
select state from jobs
where id = $1 and lease_owner = $2 and fencing_token = $3 and state = 'cancelling'
`

// failUpdateRetry transitions running → retry_wait with backoff.
const failUpdateRetry = `
update jobs
set state = 'retry_wait',
    run_at = $4,
    lease_owner = null,
    lease_until = null,
    state_version = state_version + 1,
    updated_at = now()
where id = $1
  and lease_owner = $2
  and fencing_token = $3
  and state = 'running'
`

// failUpdateDead transitions running → dead.
const failUpdateDead = `
update jobs
set state = 'dead',
    state_version = state_version + 1,
    updated_at = now()
where id = $1
  and lease_owner = $2
  and fencing_token = $3
  and state = 'running'
`

// failUpdateCancelling transitions cancelling → cancelled on fail.
// In cancelling state, fail does not trigger retry.
const failUpdateCancelling = `
update jobs
set state = 'cancelled',
    state_version = state_version + 1,
    updated_at = now()
where id = $1
  and lease_owner = $2
  and fencing_token = $3
  and state = 'cancelling'
`

// updateAttemptOutcome records the attempt result.
const updateAttemptOutcome = `
update job_attempts
set finished_at = now(),
    outcome = $3,
    error_code = $4,
    error_message = $5,
    duration_ms = $6
where job_id = $1 and attempt_no = $2
`

// insertOutboxEvent writes a state-change event to the outbox within the same
// transaction as the state transition. P0 only persists; P1 publishes.
const insertOutboxEvent = `
insert into outbox_events (aggregate_id, event_type, payload)
values ($1, $2, $3)
`

// cancelWaiting transitions a waiting-state job directly to cancelled.
const cancelWaiting = `
update jobs
set state = 'cancelled',
    state_version = state_version + 1,
    cancel_requested_at = now(),
    updated_at = now()
where id = $1
  and tenant_id = $2
  and state in ('scheduled', 'ready', 'retry_wait')
`

// cancelRunning transitions a running job to cancelling.
const cancelRunning = `
update jobs
set state = 'cancelling',
    state_version = state_version + 1,
    cancel_requested_at = now(),
    updated_at = now()
where id = $1
  and tenant_id = $2
  and state = 'running'
`

// checkTerminal checks if a job is already in a terminal state.
const checkTerminal = `
select state from jobs
where id = $1 and tenant_id = $2 and state in ('succeeded', 'dead', 'cancelled')
`

// listJobs retrieves jobs with keyset pagination ordered by created_at DESC.
const listJobs = `
select id, tenant_id, queue, type, payload, priority, state,
       run_at, attempt, max_attempts, timeout_seconds,
       idempotency_key, lease_owner, lease_until, fencing_token,
       cancel_requested_at, trace_id, state_version, retry_of_job_id,
       created_at, updated_at
from jobs
where tenant_id = $1
  and ($2::text is null or state = $2)
  and ($3::text is null or queue = $3)
  and ($4::text is null or type = $4)
  and ($5::timestamptz is null or created_at < $5)
order by created_at desc
limit $6
`

// getQueueDepth counts pending jobs in a queue for backpressure (FR-303).
// Pending states: scheduled, ready, retry_wait.
const getQueueDepth = `
select count(*) from jobs
where queue = $1
  and state in ('scheduled', 'ready', 'retry_wait')
`
