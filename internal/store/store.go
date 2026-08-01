// Package store defines the persistence contracts consumed by the domain and
// transport layers. Following Go convention, interfaces are defined at the
// consumer side.
package store

import (
	"context"
	"time"

	"github.com/xjfyrh/jobforge/internal/domain"
)

// ListFilter specifies pagination and filtering options for job listing.
type ListFilter struct {
	TenantID string
	State    *domain.JobState
	Queue    *string
	Type     *string
	Cursor   *string // opaque cursor for keyset pagination
	Limit    int
}

// JobStore defines the persistence operations for jobs. The PostgreSQL
// implementation guarantees atomicity of state transitions within transactions.
type JobStore interface {
	// Enqueue persists a new job. If an idempotency key conflict occurs within
	// the same tenant, it returns the existing job with deduplicated=true.
	Enqueue(ctx context.Context, job *domain.Job) (deduplicated bool, err error)

	// GetByID retrieves a job by ID scoped to the given tenant.
	GetByID(ctx context.Context, tenantID, jobID string) (*domain.Job, error)

	// List retrieves jobs matching the filter with keyset pagination.
	// Returns the jobs and the next cursor (empty if no more results).
	List(ctx context.Context, filter ListFilter) ([]*domain.Job, string, error)

	// Claim atomically claims up to maxJobs ready jobs from the specified
	// queue for the given worker. Uses FOR UPDATE SKIP LOCKED to prevent
	// concurrent claims from conflicting.
	//
	// Invariant: within the claim transaction, lease_owner, lease_until,
	// attempt, fencing_token and state are all updated atomically.
	Claim(ctx context.Context, params ClaimParams) ([]*domain.Job, error)

	// Heartbeat extends the lease for a running job. Only the current owner
	// with the correct fencing token may extend.
	Heartbeat(ctx context.Context, jobID, workerID string, fencingToken int64, ttl time.Duration) error

	// Complete marks a running job as succeeded. Must match owner and token.
	// Writes job_attempts and outbox_events in the same transaction.
	Complete(ctx context.Context, jobID, workerID string, fencingToken int64, resultRef string, durationMs int64) error

	// Fail marks a running job as failed. Depending on retryable flag and
	// attempt count, transitions to retry_wait or dead.
	// Writes job_attempts and outbox_events in the same transaction.
	Fail(ctx context.Context, jobID, workerID string, fencingToken int64, errCode, errMsg string, retryable bool, durationMs int64) error

	// Cancel requests cancellation of a job. Waiting-state jobs transition
	// immediately; running jobs enter cancelling.
	Cancel(ctx context.Context, tenantID, jobID string) error

	// GetQueueDepth returns the number of pending jobs in a queue.
	// Pending states: scheduled, ready, retry_wait.
	GetQueueDepth(ctx context.Context, queue string) (int, error)
}

// ClaimParams holds the parameters for a claim operation.
type ClaimParams struct {
	Queue    string
	WorkerID string
	Types    []string
	MaxJobs  int
	LeaseTTL time.Duration

	// TenantMaxInflight limits how many running jobs a tenant can have.
	// If <= 0, no limit is enforced.
	TenantMaxInflight int
}

// AttemptRecord represents a single execution attempt for audit purposes.
type AttemptRecord struct {
	JobID        string
	AttemptNo    int
	WorkerID     string
	FencingToken int64
	StartedAt    time.Time
	FinishedAt   *time.Time
	Outcome      string // "succeeded", "failed", "lease_expired"
	ErrorCode    *string
	ErrorMessage *string
	DurationMs   *int64
	TraceID      *string
}
