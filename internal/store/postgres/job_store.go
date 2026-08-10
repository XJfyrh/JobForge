// Package postgres implements the store.JobStore interface using PostgreSQL.
// All state transitions are atomic within database transactions. The claim
// operation uses FOR UPDATE SKIP LOCKED to prevent concurrent Workers from
// claiming the same job.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store"
)

// JobStore implements store.JobStore using PostgreSQL.
type JobStore struct {
	pool *pgxpool.Pool
}

// NewJobStore creates a new PostgreSQL-backed job store.
func NewJobStore(pool *pgxpool.Pool) *JobStore {
	return &JobStore{pool: pool}
}

// Ensure interface compliance at compile time.
var _ store.JobStore = (*JobStore)(nil)

// Ping performs a lightweight connectivity check against PostgreSQL.
// It satisfies the http.Pinger interface for health-readiness probes.
func (s *JobStore) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// Enqueue persists a new job. If the idempotency key already exists for the
// tenant, the existing row is compared against the incoming request hash
// (ADR-0002 CONFLICT):
//   - identical parameters (or a legacy row without a stored hash) →
//     deduplicated=true, and job is replaced with the existing row so the
//     caller receives the real job_id and current state;
//   - different parameters → a CONFLICT domain error carrying the existing
//     job id.
//
// After a successful insert of a ready job, sends pg_notify to wake up
// Gateway Poll listeners (ADR-0003).
func (s *JobStore) Enqueue(ctx context.Context, job *domain.Job) (bool, error) {
	_, err := s.pool.Exec(ctx, enqueueInsert,
		job.ID, job.TenantID, job.Queue, job.Type, job.Payload,
		job.Priority, string(job.State), job.RunAt, job.Attempt,
		job.MaxAttempts, job.TimeoutSeconds, job.IdempotencyKey,
		job.FencingToken, job.TraceID, job.TraceContext, job.StateVersion,
		job.RetryOfJobID, job.CreatedAt, job.UpdatedAt, job.RequestHash,
	)
	if err != nil {
		return false, fmt.Errorf("enqueue insert: %w", err)
	}

	// Fetch the job to determine if it was inserted or deduplicated.
	var inserted bool
	err = s.pool.QueryRow(ctx, enqueueSelectByID, job.ID).Scan(
		&job.ID, &job.TenantID, &job.Queue, &job.Type, &job.Payload,
		&job.Priority, &job.State, &job.RunAt, &job.Attempt,
		&job.MaxAttempts, &job.TimeoutSeconds, &job.IdempotencyKey,
		&job.LeaseOwner, &job.LeaseUntil, &job.FencingToken,
		&job.CancelRequestedAt, &job.TraceID, &job.TraceContext, &job.StateVersion,
		&job.RetryOfJobID, &job.CreatedAt, &job.UpdatedAt, &inserted,
	)
	if err == nil {
		// Notify listeners if a new ready job was inserted.
		if inserted && job.State == domain.StateReady {
			_, _ = s.pool.Exec(ctx, "select pg_notify('jobforge_job_ready', $1)", job.Queue)
		}
		return !inserted, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("enqueue select: %w", err)
	}

	// Conflict path: the insert did nothing because the idempotency key is
	// already taken. Fetch the existing row to compare request hashes.
	if job.IdempotencyKey == nil {
		return false, fmt.Errorf("enqueue: insert skipped but no idempotency key present")
	}
	existing, err := s.selectByIdempotencyKey(ctx, job.TenantID, *job.IdempotencyKey)
	if err != nil {
		return false, fmt.Errorf("enqueue select by idempotency key: %w", err)
	}

	// Same-key submissions with different parameters are rejected per
	// ADR-0002 (CONFLICT). Legacy rows created before migration 0008 have no
	// stored hash and keep the previous deduplicate behavior.
	if existing.RequestHash != "" && existing.RequestHash != job.RequestHash {
		return false, domain.NewError(domain.CodeConflict, domain.ErrConflict,
			"idempotency key conflict: existing job %s was submitted with different parameters", existing.ID)
	}

	// Identical resubmission: report the existing job so the caller receives
	// the real job_id and current state instead of the discarded request.
	*job = *existing
	return true, nil
}

// selectByIdempotencyKey fetches the job owning (tenant_id, idempotency_key).
// The stored request_hash (nullable for legacy rows) is surfaced via
// Job.RequestHash; an empty value means "hash unknown".
func (s *JobStore) selectByIdempotencyKey(ctx context.Context, tenantID, key string) (*domain.Job, error) {
	var (
		job  domain.Job
		hash *string
	)
	err := s.pool.QueryRow(ctx, enqueueSelectByKey, tenantID, key).Scan(
		&job.ID, &job.TenantID, &job.Queue, &job.Type, &job.Payload,
		&job.Priority, &job.State, &job.RunAt, &job.Attempt,
		&job.MaxAttempts, &job.TimeoutSeconds, &job.IdempotencyKey,
		&job.LeaseOwner, &job.LeaseUntil, &job.FencingToken,
		&job.CancelRequestedAt, &job.TraceID, &job.TraceContext, &job.StateVersion,
		&job.RetryOfJobID, &job.CreatedAt, &job.UpdatedAt, &hash,
	)
	if err != nil {
		return nil, err
	}
	if hash != nil {
		job.RequestHash = *hash
	}
	return &job, nil
}

// GetByID retrieves a job by ID scoped to the given tenant.
func (s *JobStore) GetByID(ctx context.Context, tenantID, jobID string) (*domain.Job, error) {
	job, err := scanJob(s.pool.QueryRow(ctx, getByID, jobID, tenantID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.NewError(domain.CodeNotFound, domain.ErrNotFound,
				"job %s not found for tenant %s", jobID, tenantID)
		}
		return nil, fmt.Errorf("get job: %w", err)
	}
	return job, nil
}

// List retrieves jobs with keyset pagination.
func (s *JobStore) List(ctx context.Context, filter store.ListFilter) ([]*domain.Job, string, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	var stateStr *string
	if filter.State != nil {
		v := string(*filter.State)
		stateStr = &v
	}

	var cursorTime *time.Time
	if filter.Cursor != nil && *filter.Cursor != "" {
		t, err := time.Parse(time.RFC3339Nano, *filter.Cursor)
		if err != nil {
			return nil, "", domain.NewError(domain.CodeInvalidArgument, domain.ErrInvalidArgument,
				"invalid cursor format")
		}
		cursorTime = &t
	}

	rows, err := s.pool.Query(ctx, listJobs,
		filter.TenantID, stateStr, filter.Queue, filter.Type, cursorTime, limit+1,
	)
	if err != nil {
		return nil, "", fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()

	var jobs []*domain.Job
	for rows.Next() {
		job, err := scanJobFromRows(rows)
		if err != nil {
			return nil, "", fmt.Errorf("scan job: %w", err)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("rows iteration: %w", err)
	}

	var nextCursor string
	if len(jobs) > limit {
		jobs = jobs[:limit]
		nextCursor = jobs[len(jobs)-1].CreatedAt.Format(time.RFC3339Nano)
	}

	return jobs, nextCursor, nil
}

// Claim atomically claims up to maxJobs ready jobs for a worker.
//
// Transaction invariant: SELECT FOR UPDATE SKIP LOCKED + UPDATE + INSERT
// job_attempts all happen in a single transaction. This guarantees that
// lease_owner, lease_until, attempt, fencing_token and state are updated
// atomically, and no two Workers can claim the same job.
//
// Tenant quota (PRD v0.3 FR-720~726, ADR-0007): when TenantMaxInflight > 0,
// candidates of full tenants are pre-filtered before the row-lock window;
// per-tenant slot reservations run as the LAST step of the claim transaction
// (conditional batch upserts, tenants in ascending order) so the counter row
// lock is held for a single round trip instead of the whole claim. Losing
// the reservation race (a concurrent claim filled the slots after the
// unlocked counter snapshot) rolls the transaction back and retries with a
// fresh snapshot.
func (s *JobStore) Claim(ctx context.Context, params store.ClaimParams) (*store.ClaimResult, error) {
	// Reservation races are rare (the pre-filter already excludes full
	// tenants); a bounded retry keeps the store self-contained while the
	// gateway long-poll provides the outer loop.
	const maxQuotaAttempts = 3
	for attempt := 0; ; attempt++ {
		res, retriable, err := s.claimOnce(ctx, params)
		if err != nil {
			return nil, err
		}
		if !retriable || attempt+1 >= maxQuotaAttempts {
			return res, nil
		}
	}
}

// claimOnce executes one claim transaction. retriable=true means a quota
// reservation lost a race; the transaction was rolled back and the caller
// may retry.
func (s *JobStore) claimOnce(ctx context.Context, params store.ClaimParams) (*store.ClaimResult, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin claim tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Select ready jobs with row-level locking. Queues are claimed in
	// declaration order (see claimSelect). With an active quota and enabled
	// pre-filter, claimSelectQuota additionally excludes full tenants before
	// the LIMIT window so other tenants' candidates backfill it (FR-725).
	var rows pgx.Rows
	if params.TenantMaxInflight > 0 && params.QuotaPrefilter {
		rows, err = tx.Query(ctx, claimSelectQuota, params.Queues, params.Types, params.MaxJobs, params.TenantMaxInflight)
	} else {
		rows, err = tx.Query(ctx, claimSelect, params.Queues, params.Types, params.MaxJobs)
	}
	if err != nil {
		return nil, false, fmt.Errorf("claim select: %w", err)
	}

	var candidates []*domain.Job
	for rows.Next() {
		job, err := scanJobFromRows(rows)
		if err != nil {
			rows.Close()
			return nil, false, fmt.Errorf("scan claim candidate: %w", err)
		}
		candidates = append(candidates, job)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("claim rows: %w", err)
	}

	if len(candidates) == 0 {
		return &store.ClaimResult{}, false, nil
	}

	leaseUntil := time.Now().Add(params.LeaseTTL)
	claimed := make([]*domain.Job, 0, len(candidates))
	conflicts := 0
	maxObservedInflight := 0

	// Planned claims per tenant (quota active): an unlocked counter snapshot
	// decides how many candidates of each tenant can be claimed; the binding
	// hard-cap check stays in the conditional reservation below.
	planned := make(map[string]struct{}, len(candidates))
	type tenantPlan struct {
		tenant string
		count  int
	}
	var plans []tenantPlan
	if params.TenantMaxInflight > 0 {
		byTenant := make([]*domain.Job, len(candidates))
		copy(byTenant, candidates)
		sort.SliceStable(byTenant, func(i, j int) bool {
			return byTenant[i].TenantID < byTenant[j].TenantID
		})

		tenants := make([]string, 0)
		for i := 0; i < len(byTenant); {
			tenant := byTenant[i].TenantID
			tenants = append(tenants, tenant)
			for i < len(byTenant) && byTenant[i].TenantID == tenant {
				i++
			}
		}
		counters, err := snapshotQuotaCounters(ctx, tx, tenants)
		if err != nil {
			return nil, false, err
		}

		for i := 0; i < len(byTenant); {
			tenant := byTenant[i].TenantID
			end := i
			for end < len(byTenant) && byTenant[end].TenantID == tenant {
				end++
			}
			group := byTenant[i:end]
			i = end

			available := params.TenantMaxInflight - counters[tenant]
			take := len(group)
			if take > available {
				take = available
			}
			if take <= 0 {
				// Snapshot already at the cap: skip the whole group.
				conflicts += len(group)
				continue
			}
			conflicts += len(group) - take
			for _, candidate := range group[:take] {
				planned[candidate.ID] = struct{}{}
			}
			plans = append(plans, tenantPlan{tenant: tenant, count: take})
		}
	} else {
		for _, candidate := range candidates {
			planned[candidate.ID] = struct{}{}
		}
	}

	// Claim phase: lease update + attempt record, in candidate order, only
	// for planned candidates.
	for _, candidate := range candidates {
		if _, ok := planned[candidate.ID]; !ok {
			continue
		}

		// Update lease fields atomically.
		job, err := scanJob(tx.QueryRow(ctx, claimUpdate, candidate.ID, params.WorkerID, leaseUntil))
		if err != nil {
			return nil, false, fmt.Errorf("claim update: %w", err)
		}

		// Record the attempt start.
		_, err = tx.Exec(ctx, claimInsertAttempt,
			job.ID, job.Attempt, params.WorkerID, job.FencingToken, job.TraceID)
		if err != nil {
			return nil, false, fmt.Errorf("claim insert attempt: %w", err)
		}

		claimed = append(claimed, job)
	}

	// Reservation phase LAST (quota active): the conditional batch upserts
	// are the binding hard-cap check. Running them after the claim updates
	// keeps the counter row lock held for a single round trip before commit
	// instead of the whole claim (ADR-0007 §2/§3). Tenants are reserved in
	// ascending tenant_id order; a lost race rolls back everything and the
	// caller retries with a fresh snapshot.
	for _, plan := range plans {
		var inflight int
		err := tx.QueryRow(ctx, quotaReserveBatch, plan.tenant, plan.count, params.TenantMaxInflight).Scan(&inflight)
		if errors.Is(err, pgx.ErrNoRows) {
			return &store.ClaimResult{}, true, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("reserve tenant quota: %w", err)
		}
		if inflight > maxObservedInflight {
			maxObservedInflight = inflight
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit claim tx: %w", err)
	}

	return &store.ClaimResult{
		Jobs:                claimed,
		QuotaConflicts:      conflicts,
		MaxObservedInflight: maxObservedInflight,
	}, false, nil
}

// snapshotQuotaCounters reads the derived counters for the given tenants
// without locking them. Missing rows report zero. The snapshot may be stale;
// the conditional reservation later in the transaction enforces the hard cap.
func snapshotQuotaCounters(ctx context.Context, tx pgx.Tx, tenants []string) (map[string]int, error) {
	counters := make(map[string]int, len(tenants))
	rows, err := tx.Query(ctx, quotaCounterSnapshot, tenants)
	if err != nil {
		return nil, fmt.Errorf("snapshot quota counters: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var tenant string
		var inflight int
		if err := rows.Scan(&tenant, &inflight); err != nil {
			return nil, fmt.Errorf("scan quota counter: %w", err)
		}
		counters[tenant] = inflight
	}
	return counters, rows.Err()
}

// Heartbeat extends the lease for a running/cancelling job.
func (s *JobStore) Heartbeat(ctx context.Context, jobID, workerID string, fencingToken int64, ttl time.Duration) error {
	leaseUntil := time.Now().Add(ttl)
	tag, err := s.pool.Exec(ctx, heartbeatUpdate, jobID, workerID, fencingToken, leaseUntil)
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewError(domain.CodeStaleLease, domain.ErrStaleLease,
			"heartbeat rejected: owner/token mismatch or job not in active state")
	}
	return nil
}

// Complete transitions a running job to succeeded within a transaction that
// also updates the attempt record, releases the tenant's quota slot and
// writes an outbox event.
func (s *JobStore) Complete(ctx context.Context, jobID, workerID string, fencingToken int64, _ string, durationMs int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin complete tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var tenantID string
	err = tx.QueryRow(ctx, completeUpdate, jobID, workerID, fencingToken).Scan(&tenantID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("complete update: %w", err)
		}
		// Determine specific rejection reason.
		var state string
		err := tx.QueryRow(ctx, completeRejectCancelling, jobID, workerID, fencingToken).Scan(&state)
		if err == nil {
			return domain.NewError(domain.CodeCancelRequested, domain.ErrCancelRequested,
				"job is cancelling: complete rejected")
		}
		return domain.NewError(domain.CodeStaleLease, domain.ErrStaleLease,
			"complete rejected: owner/token mismatch or job not running")
	}

	// Release the tenant's inflight slot in the same transaction (ADR-0007 §6).
	if _, err := tx.Exec(ctx, quotaRelease, tenantID); err != nil {
		return fmt.Errorf("release tenant quota: %w", err)
	}

	// Update attempt outcome.
	if err := updateAttempt(ctx, tx, jobID, workerID, "succeeded", nil, nil, &durationMs); err != nil {
		return err
	}

	// Write outbox event.
	if err := writeOutbox(ctx, tx, jobID, "job.succeeded"); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// Fail transitions a running job to retry_wait or dead, or a cancelling job
// to cancelled. All within a single transaction with quota release, attempt
// and outbox writes.
func (s *JobStore) Fail(ctx context.Context, jobID, workerID string, fencingToken int64, errCode, errMsg string, retryable bool, durationMs int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin fail tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Try cancelling → cancelled first.
	var tenantID string
	var outcome string
	err = tx.QueryRow(ctx, failUpdateCancelling, jobID, workerID, fencingToken).Scan(&tenantID)
	switch {
	case err == nil:
		outcome = "cancelled"
	case errors.Is(err, pgx.ErrNoRows):
		// Determine retry vs dead.
		// Fetch current attempt count to decide.
		var attempt, maxAttempts int
		err := tx.QueryRow(ctx, "select attempt, max_attempts from jobs where id = $1 and lease_owner = $2 and fencing_token = $3 and state = 'running'",
			jobID, workerID, fencingToken).Scan(&attempt, &maxAttempts)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeStaleLease, domain.ErrStaleLease,
					"fail rejected: owner/token mismatch or job not running")
			}
			return fmt.Errorf("fail check: %w", err)
		}

		if retryable && attempt < maxAttempts {
			// Calculate backoff: base=1s, max=5min, full jitter (0–1s random).
			jitter := time.Duration(rand.Int64N(int64(time.Second)))
			backoff := domain.Backoff(attempt, time.Second, 5*time.Minute, jitter)
			nextRetry := time.Now().Add(backoff)
			err = tx.QueryRow(ctx, failUpdateRetry, jobID, workerID, fencingToken, nextRetry).Scan(&tenantID)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return domain.NewError(domain.CodeStaleLease, domain.ErrStaleLease,
						"fail rejected: concurrent state change")
				}
				return fmt.Errorf("fail retry: %w", err)
			}
			outcome = "failed_retry"
		} else {
			err = tx.QueryRow(ctx, failUpdateDead, jobID, workerID, fencingToken).Scan(&tenantID)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return domain.NewError(domain.CodeStaleLease, domain.ErrStaleLease,
						"fail rejected: concurrent state change")
				}
				return fmt.Errorf("fail dead: %w", err)
			}
			outcome = "failed_dead"
		}
	default:
		return fmt.Errorf("fail cancelling: %w", err)
	}

	// Release the tenant's inflight slot in the same transaction: all three
	// transitions above leave the inflight states (ADR-0007 §6).
	if _, err := tx.Exec(ctx, quotaRelease, tenantID); err != nil {
		return fmt.Errorf("release tenant quota: %w", err)
	}

	// Update attempt outcome.
	if err := updateAttempt(ctx, tx, jobID, workerID, outcome, &errCode, &errMsg, &durationMs); err != nil {
		return err
	}

	// Write outbox event.
	eventType := "job." + outcome
	if err := writeOutbox(ctx, tx, jobID, eventType); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// Cancel requests cancellation. Waiting-state jobs go directly to cancelled;
// running jobs enter cancelling.
func (s *JobStore) Cancel(ctx context.Context, tenantID, jobID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin cancel tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Try waiting-state cancel first.
	tag, err := tx.Exec(ctx, cancelWaiting, jobID, tenantID)
	if err != nil {
		return fmt.Errorf("cancel waiting: %w", err)
	}
	if tag.RowsAffected() > 0 {
		if err := writeOutbox(ctx, tx, jobID, "job.cancelled"); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	// Try running → cancelling.
	tag, err = tx.Exec(ctx, cancelRunning, jobID, tenantID)
	if err != nil {
		return fmt.Errorf("cancel running: %w", err)
	}
	if tag.RowsAffected() > 0 {
		if err := writeOutbox(ctx, tx, jobID, "job.cancelling"); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	// Check if already terminal.
	var state string
	err = tx.QueryRow(ctx, checkTerminal, jobID, tenantID).Scan(&state)
	if err == nil {
		return domain.NewError(domain.CodeAlreadyTerminal, domain.ErrAlreadyTerminal,
			"job already in terminal state %q", state)
	}

	return domain.NewError(domain.CodeNotFound, domain.ErrNotFound,
		"job %s not found for tenant %s", jobID, tenantID)
}

// updateAttempt records the outcome of an attempt within the transaction.
func updateAttempt(ctx context.Context, tx pgx.Tx, jobID, _, outcome string, errCode, errMsg *string, durationMs *int64) error {
	// Get the current attempt number for this job.
	var attemptNo int
	err := tx.QueryRow(ctx, "select attempt from jobs where id = $1", jobID).Scan(&attemptNo)
	if err != nil {
		return fmt.Errorf("get attempt no: %w", err)
	}

	_, err = tx.Exec(ctx, updateAttemptOutcome, jobID, attemptNo, outcome, errCode, errMsg, durationMs)
	if err != nil {
		return fmt.Errorf("update attempt: %w", err)
	}
	return nil
}

// writeOutbox inserts an outbox event within the transaction.
func writeOutbox(ctx context.Context, tx pgx.Tx, aggregateID, eventType string) error {
	payload, _ := json.Marshal(map[string]string{
		"job_id":     aggregateID,
		"event_type": eventType,
	})
	_, err := tx.Exec(ctx, insertOutboxEvent, aggregateID, eventType, payload)
	if err != nil {
		return fmt.Errorf("write outbox: %w", err)
	}
	return nil
}

// scanJob scans a single job row from a Row.
func scanJob(row pgx.Row) (*domain.Job, error) {
	var j domain.Job
	var state string
	err := row.Scan(
		&j.ID, &j.TenantID, &j.Queue, &j.Type, &j.Payload,
		&j.Priority, &state, &j.RunAt, &j.Attempt,
		&j.MaxAttempts, &j.TimeoutSeconds, &j.IdempotencyKey,
		&j.LeaseOwner, &j.LeaseUntil, &j.FencingToken,
		&j.CancelRequestedAt, &j.TraceID, &j.TraceContext, &j.StateVersion,
		&j.RetryOfJobID, &j.CreatedAt, &j.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	j.State = domain.JobState(state)
	return &j, nil
}

// scanJobFromRows scans a job from a Rows iterator.
func scanJobFromRows(rows pgx.Rows) (*domain.Job, error) {
	var j domain.Job
	var state string
	err := rows.Scan(
		&j.ID, &j.TenantID, &j.Queue, &j.Type, &j.Payload,
		&j.Priority, &state, &j.RunAt, &j.Attempt,
		&j.MaxAttempts, &j.TimeoutSeconds, &j.IdempotencyKey,
		&j.LeaseOwner, &j.LeaseUntil, &j.FencingToken,
		&j.CancelRequestedAt, &j.TraceID, &j.TraceContext, &j.StateVersion,
		&j.RetryOfJobID, &j.CreatedAt, &j.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	j.State = domain.JobState(state)
	return &j, nil
}

// GetQueueDepth returns the number of pending jobs in a queue.
// Used for queue backpressure (FR-303).
func (s *JobStore) GetQueueDepth(ctx context.Context, queue string) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, getQueueDepth, queue).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get queue depth: %w", err)
	}
	return count, nil
}

// ListAttempts returns the attempt timeline of a job scoped to the given
// tenant, ordered by attempt_no ascending (FR-002). Returns NOT_FOUND when
// the job does not exist for the tenant.
func (s *JobStore) ListAttempts(ctx context.Context, tenantID, jobID string) ([]store.AttemptRecord, error) {
	// Tenant scoping: reject unknown / foreign jobs first.
	if _, err := s.GetByID(ctx, tenantID, jobID); err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, listAttempts, jobID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list attempts: %w", err)
	}
	defer rows.Close()

	attempts := make([]store.AttemptRecord, 0)
	for rows.Next() {
		var a store.AttemptRecord
		a.JobID = jobID
		var outcome *string
		if err := rows.Scan(
			&a.AttemptNo, &a.WorkerID, &a.FencingToken, &a.StartedAt, &a.FinishedAt,
			&outcome, &a.ErrorCode, &a.ErrorMessage, &a.DurationMs, &a.TraceID,
		); err != nil {
			return nil, fmt.Errorf("scan attempt: %w", err)
		}
		if outcome != nil {
			a.Outcome = *outcome
		}
		attempts = append(attempts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("attempts rows: %w", err)
	}
	return attempts, nil
}
