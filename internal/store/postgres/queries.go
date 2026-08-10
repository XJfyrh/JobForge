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
    idempotency_key, fencing_token, trace_id, trace_context, state_version,
    retry_of_job_id, created_at, updated_at, request_hash
) values (
    $1, $2, $3, $4, $5, $6, $7,
    $8, $9, $10, $11,
    $12, $13, $14, $15, $16,
    $17, $18, $19, $20
)
on conflict (tenant_id, idempotency_key) where idempotency_key is not null
do nothing
`

// enqueueSelectByKey fetches the existing job owning (tenant_id,
// idempotency_key) together with its stored request_hash. Used by Enqueue on
// conflict to distinguish identical resubmissions from same-key-different-
// parameter conflicts (ADR-0002 CONFLICT).
const enqueueSelectByKey = `
select id, tenant_id, queue, type, payload, priority, state,
       run_at, attempt, max_attempts, timeout_seconds,
       idempotency_key, lease_owner, lease_until, fencing_token,
       cancel_requested_at, trace_id, trace_context, state_version, retry_of_job_id,
       created_at, updated_at, request_hash
from jobs
where tenant_id = $1 and idempotency_key = $2
`

// enqueueSelectByID fetches a job by ID after insert (or conflict).
const enqueueSelectByID = `
select id, tenant_id, queue, type, payload, priority, state,
       run_at, attempt, max_attempts, timeout_seconds,
       idempotency_key, lease_owner, lease_until, fencing_token,
       cancel_requested_at, trace_id, trace_context, state_version, retry_of_job_id,
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
       cancel_requested_at, trace_id, trace_context, state_version, retry_of_job_id,
       created_at, updated_at
from jobs
where id = $1 and tenant_id = $2
`

// claimSelect locks up to N ready jobs from the declared queues using SKIP
// LOCKED. This prevents concurrent Workers from claiming the same job.
//
// Queues are honored in declaration order: array_position sorts earlier-
// declared queues first, then the usual priority/created_at ordering applies
// within each queue. The partial index idx_jobs_claim (queue, priority desc,
// created_at asc) WHERE state='ready' still serves the queue = any($1)
// predicate via per-queue index scans; the declaration-order sort runs only
// over the index-filtered candidates.
//
// Invariant: FOR UPDATE SKIP LOCKED ensures each row is locked by at most one
// transaction.
const claimSelect = `
select id, tenant_id, queue, type, payload, priority, state,
       run_at, attempt, max_attempts, timeout_seconds,
       idempotency_key, lease_owner, lease_until, fencing_token,
       cancel_requested_at, trace_id, trace_context, state_version, retry_of_job_id,
       created_at, updated_at
from jobs
where queue = any($1::text[])
  and state = 'ready'
  and run_at <= now()
  and ($2::text[] is null or type = any($2))
order by array_position($1::text[], queue) asc, priority desc, created_at asc
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
          cancel_requested_at, trace_id, trace_context, state_version, retry_of_job_id,
          created_at, updated_at
`

// claimSelectQuota selects claim candidates with the tenant quota pre-filter
// active (PRD v0.3 FR-721/725, ADR-0007 §4): candidates of tenants whose
// derived counter already reached the limit are excluded BEFORE the LIMIT
// window, so jobs of tenants with free slots backfill the window instead of
// being starved behind a full tenant's backlog. The pre-filter read is an
// unlocked READ COMMITTED snapshot: staleness only wastes candidate slots
// (reservation below enforces the hard cap) or delays a claim by one round.
// $4 is the tenant inflight limit (> 0). FOR UPDATE OF j keeps the row locks
// on the jobs table only.
const claimSelectQuota = `
select j.id, j.tenant_id, j.queue, j.type, j.payload, j.priority, j.state,
       j.run_at, j.attempt, j.max_attempts, j.timeout_seconds,
       j.idempotency_key, j.lease_owner, j.lease_until, j.fencing_token,
       j.cancel_requested_at, j.trace_id, j.trace_context, j.state_version, j.retry_of_job_id,
       j.created_at, j.updated_at
from jobs j
where j.queue = any($1::text[])
  and j.state = 'ready'
  and j.run_at <= now()
  and ($2::text[] is null or j.type = any($2))
  and not exists (
      select 1 from tenant_quota_counters c
      where c.tenant_id = j.tenant_id
        and c.inflight >= $4
  )
order by array_position($1::text[], j.queue) asc, j.priority desc, j.created_at asc
limit $3
for update of j skip locked
`

// quotaCounterSnapshot reads the derived counters for the candidate tenants
// without locking them (ADR-0007 §4 stale-window semantics): the snapshot
// only sizes the per-tenant claim batches, the conditional reservation below
// enforces the hard cap. Missing rows report zero at the caller.
const quotaCounterSnapshot = `
select tenant_id, inflight from tenant_quota_counters where tenant_id = any($1::text[])
`

// quotaReserveBatch atomically reserves n slots for one tenant as the LAST
// step of the claim transaction (PRD v0.3 FR-721 batch caliber, ADR-0007 §2).
// The condition inflight + n <= limit makes the whole batch fail atomically
// (no row returned, counter untouched) when it would cross the cap; the
// caller then rolls back and retries with a fresh snapshot. A successful
// batch is refunded only by transaction rollback: every reserved candidate is
// claimed in the same transaction, so no unused slots exist at commit. The
// counter row is created lazily (inflight = n) on first use; the caller only
// reserves n <= limit, so the insert branch never crosses the cap.
const quotaReserveBatch = `
insert into tenant_quota_counters (tenant_id, inflight, updated_at)
values ($1, $2, now())
on conflict (tenant_id) do update
set inflight = tenant_quota_counters.inflight + $2,
    updated_at = now()
where tenant_quota_counters.inflight + $2 <= $3
returning inflight
`

// quotaRelease decrements the tenant's inflight counter when a job leaves the
// inflight states (running/cancelling) within the same transaction as the
// state transition (PRD v0.3 FR-722). The inflight > 0 guard prevents going
// negative on pre-existing drift; reconcile repairs drift from jobs.
const quotaRelease = `
update tenant_quota_counters
set inflight = inflight - 1,
    updated_at = now()
where tenant_id = $1 and inflight > 0
`

// quotaReleaseN decrements the tenant's inflight counter by $2, clamped at
// zero. Used by lease recovery, which releases several jobs of the same
// tenant in one statement (per-tenant aggregate).
const quotaReleaseN = `
update tenant_quota_counters
set inflight = greatest(inflight - $2, 0),
    updated_at = now()
where tenant_id = $1
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

// completeUpdate transitions running → succeeded. Returns the tenant_id so
// the same transaction can release the tenant's quota slot (ADR-0007 §6).
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
returning tenant_id
`

// completeRejectCancelling checks if the job is in cancelling state (cancel won
// the race). Used to return CANCEL_REQUESTED instead of STALE_LEASE.
const completeRejectCancelling = `
select state from jobs
where id = $1 and lease_owner = $2 and fencing_token = $3 and state = 'cancelling'
`

// failUpdateRetry transitions running → retry_wait with backoff. Returns the
// tenant_id for the same-transaction quota release (ADR-0007 §6).
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
returning tenant_id
`

// failUpdateDead transitions running → dead. Returns the tenant_id for the
// same-transaction quota release (ADR-0007 §6).
const failUpdateDead = `
update jobs
set state = 'dead',
    state_version = state_version + 1,
    updated_at = now()
where id = $1
  and lease_owner = $2
  and fencing_token = $3
  and state = 'running'
returning tenant_id
`

// failUpdateCancelling transitions cancelling → cancelled on fail.
// In cancelling state, fail does not trigger retry. Returns the tenant_id:
// cancelling jobs keep occupying a quota slot until this terminal transition
// releases it (PRD v0.3 FR-723, ADR-0007 §6).
const failUpdateCancelling = `
update jobs
set state = 'cancelled',
    state_version = state_version + 1,
    updated_at = now()
where id = $1
  and lease_owner = $2
  and fencing_token = $3
  and state = 'cancelling'
returning tenant_id
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
       cancel_requested_at, trace_id, trace_context, state_version, retry_of_job_id,
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

// listAttempts returns the attempt timeline of a job (FR-002). The join on
// jobs scopes the query to the owning tenant; other tenants' requests see no
// rows and are mapped to NOT_FOUND by the caller.
const listAttempts = `
select a.attempt_no, a.worker_id, a.fencing_token, a.started_at, a.finished_at,
       a.outcome, a.error_code, a.error_message, a.duration_ms, a.trace_id
from job_attempts a
join jobs j on j.id = a.job_id and j.tenant_id = $2
where a.job_id = $1
order by a.attempt_no asc
`
