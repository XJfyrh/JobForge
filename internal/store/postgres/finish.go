package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store"
)

// Complete keeps the existing store contract: an already accepted call is
// reported as terminal. Gateways use CompleteAttempt for an idempotent ACK.
func (s *JobStore) Complete(ctx context.Context, jobID, workerID string, token int64, ref string, durationMs int64) error {
	result, err := s.CompleteAttempt(ctx, jobID, workerID, token, ref, durationMs)
	if err == nil && !result.Changed {
		return domain.NewError(domain.CodeAlreadyTerminal, domain.ErrAlreadyTerminal, "completion already accepted")
	}
	return err
}

// Fail keeps the existing store contract. Gateways use FailAttempt to obtain
// an ACK and the transaction's outcome without a racy follow-up state query.
func (s *JobStore) Fail(ctx context.Context, jobID, workerID string, token int64, code, message string, retryable bool, durationMs int64) error {
	result, err := s.FailAttempt(ctx, jobID, workerID, token, code, message, retryable, durationMs)
	if err == nil && !result.Changed {
		return staleFinish()
	}
	return err
}

// CompleteAttempt commits the first result and success atomically with attempt,
// quota and outbox. A duplicate never replaces the first accepted result.
func (s *JobStore) CompleteAttempt(ctx context.Context, jobID, workerID string, token int64, ref string, durationMs int64) (*store.AttemptResult, error) {
	if err := domain.ValidateResultRef(ref); err != nil {
		return nil, err
	}
	return s.finishAttempt(ctx, jobID, workerID, token, finishRequest{
		complete: true, ref: ref, durationMs: durationMs,
	})
}

// FailAttempt commits the domain failure decision and its reporting metadata.
// Only actual retry_wait/dead transitions are eligible for retry/DLQ metrics.
func (s *JobStore) FailAttempt(ctx context.Context, jobID, workerID string, token int64, code, message string, retryable bool, durationMs int64) (*store.AttemptResult, error) {
	return s.finishAttempt(ctx, jobID, workerID, token, finishRequest{
		code: code, message: message, retryable: retryable, durationMs: durationMs,
	})
}

type finishRequest struct {
	complete   bool
	ref        string
	code       string
	message    string
	retryable  bool
	durationMs int64
}

// The jobs lock serializes finish, cancel and recovery. Metadata comes from
// that same locked row. No model/network/telemetry call occurs in this tx.
func (s *JobStore) finishAttempt(ctx context.Context, jobID, workerID string, token int64, req finishRequest) (*store.AttemptResult, error) {
	if req.durationMs < 0 {
		return nil, domain.NewError(domain.CodeInvalidArgument, domain.ErrInvalidArgument, "duration must be non-negative")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin finish tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var job domain.Job
	var now time.Time
	err = tx.QueryRow(ctx, finishSelect, jobID).Scan(
		&job.ID, &job.TenantID, &job.Queue, &job.Type, &job.State,
		&job.Attempt, &job.MaxAttempts, &job.LeaseOwner, &job.LeaseUntil, &job.FencingToken,
		&job.RunAt, &job.StateVersion, &job.TraceContext, &job.ResultRef, &now,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, staleFinish()
	}
	if err != nil {
		return nil, fmt.Errorf("lock finish job: %w", err)
	}
	// Compare the current token even on terminal/waiting rows. A historical
	// attempt's successful RPC must never ACK a lease that has been replaced.
	if job.FencingToken != token || token <= 0 {
		return nil, staleFinish()
	}
	if job.State != domain.StateRunning && job.State != domain.StateCancelling {
		return duplicateFinish(ctx, tx, &job, workerID, token, req.complete)
	}
	if job.LeaseOwner == nil || *job.LeaseOwner != workerID {
		return nil, staleFinish()
	}
	result := &store.AttemptResult{Changed: true, Queue: job.Queue, Type: job.Type, DurationMs: req.durationMs}
	var errCode, errMessage *string
	if req.complete {
		err = job.CompleteWithResult(workerID, token, req.ref, now)
		result.Outcome = "succeeded"
	} else {
		jitter := time.Duration(rand.Int64N(int64(time.Second)))
		next := now.Add(domain.Backoff(job.Attempt, time.Second, 5*time.Minute, jitter)).Truncate(time.Microsecond)
		err = job.Fail(workerID, token, req.retryable, next, now)
		errCode, errMessage = &req.code, &req.message
		switch job.State {
		case domain.StateRetryWait:
			result.Outcome = "failed_retry"
		case domain.StateDead:
			result.Outcome = "failed_dead"
		case domain.StateCancelled:
			result.Outcome = "cancelled"
		}
	}
	if err != nil {
		return nil, err
	}
	result.State, result.RunAt = job.State, job.RunAt
	tag, err := tx.Exec(ctx, finishUpdate,
		job.ID, job.Attempt, workerID, token, job.State, job.RunAt,
		job.LeaseOwner, job.LeaseUntil, job.ResultRef, result.Outcome,
		errCode, errMessage, req.durationMs,
	)
	if err != nil {
		return nil, fmt.Errorf("update finish job: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return nil, staleFinish()
	}
	if _, err := tx.Exec(ctx, quotaRelease, job.TenantID); err != nil {
		return nil, fmt.Errorf("release tenant quota: %w", err)
	}
	if err := writeOutbox(ctx, tx, job.ID, "job."+result.Outcome, job.StateVersion, job.TraceContext); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit finish tx: %w", err)
	}
	return result, nil
}

func duplicateFinish(ctx context.Context, tx pgx.Tx, job *domain.Job, workerID string, token int64, complete bool) (*store.AttemptResult, error) {
	result := &store.AttemptResult{Queue: job.Queue, Type: job.Type, RunAt: job.RunAt}
	err := tx.QueryRow(ctx, `select outcome, coalesce(duration_ms, 0) from job_attempts
where job_id = $1 and attempt_no = $2 and worker_id = $3 and fencing_token = $4
and finished_at is not null`, job.ID, job.Attempt, workerID, token).Scan(&result.Outcome, &result.DurationMs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, staleFinish()
	}
	if err != nil {
		return nil, fmt.Errorf("read accepted attempt: %w", err)
	}
	switch {
	case complete && result.Outcome == "succeeded":
		result.State = domain.StateSucceeded
	case !complete && result.Outcome == "failed_retry":
		result.State = domain.StateRetryWait
	case !complete && result.Outcome == "failed_dead":
		result.State = domain.StateDead
	case !complete && result.Outcome == "cancelled":
		result.State = domain.StateCancelled
	default:
		// Scheduler recovery writes lease_expired, never an RPC ACK outcome.
		return nil, staleFinish()
	}
	return result, nil
}

func staleFinish() error {
	return domain.NewError(domain.CodeStaleLease, domain.ErrStaleLease, "report rejected: lease or accepted attempt does not match")
}

const finishSelect = `
select id, tenant_id, queue, type, state, attempt, max_attempts,
       lease_owner, lease_until, fencing_token, run_at, state_version, trace_context,
       result_ref, clock_timestamp()
from jobs where id = $1 for update
`

// The locked job and its current unfinished attempt change together. The CTE
// removes a round trip without moving the state decision out of the domain.
const finishUpdate = `
with finished as (
    update job_attempts set finished_at = now(), outcome = $10,
        error_code = $11, error_message = $12, duration_ms = $13
    where job_id = $1 and attempt_no = $2 and worker_id = $3
        and fencing_token = $4 and finished_at is null
    returning job_id
)
update jobs set state = $5, run_at = $6, lease_owner = $7, lease_until = $8,
    result_ref = $9, state_version = state_version + 1, updated_at = now()
where id = $1 and exists (select 1 from finished)
`
