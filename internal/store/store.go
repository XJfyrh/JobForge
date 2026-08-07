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
	// Enqueue persists a new job. If the idempotency key already exists for
	// the tenant with identical parameters, it returns the existing job with
	// deduplicated=true; a same-key submission with different parameters
	// fails with a CONFLICT domain error (ADR-0002).
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

	// ListAttempts returns the attempt timeline of a job scoped to a tenant,
	// ordered by attempt_no ascending (FR-002).
	ListAttempts(ctx context.Context, tenantID, jobID string) ([]AttemptRecord, error)
}

// ClaimParams holds the parameters for a claim operation.
type ClaimParams struct {
	// Queues lists the queues to claim from, in priority order: jobs from
	// earlier-declared queues are claimed before later ones, and within a
	// queue the usual priority/created_at ordering applies.
	Queues   []string
	WorkerID string
	Types    []string
	MaxJobs  int
	LeaseTTL time.Duration

	// TenantMaxInflight limits how many running jobs a tenant can have.
	// If <= 0, no limit is enforced.
	TenantMaxInflight int
}

// OutboxEvent represents one row of the outbox_events table. Events are
// written within job state transactions and published asynchronously by the
// outbox publisher (PRD v0.2 FR-610~612).
type OutboxEvent struct {
	EventID         int64
	AggregateID     string
	EventType       string
	Payload         []byte
	CreatedAt       time.Time
	PublishedAt     *time.Time
	PublishAttempts int
}

// OutboxStore defines the persistence operations consumed by the outbox
// publisher. All operations run outside job state transactions and never
// modify job core state.
type OutboxStore interface {
	// FetchUnpublished claims up to batch unpublished events ordered by
	// created_at. Concurrent publishers are safe via FOR UPDATE SKIP LOCKED.
	FetchUnpublished(ctx context.Context, batch int) ([]*OutboxEvent, error)

	// MarkPublished records successful publication. Returns true if this call
	// performed the transition (published_at was still NULL).
	MarkPublished(ctx context.Context, eventID int64) (bool, error)

	// MarkPublishFailed increments publish_attempts; the event remains
	// unpublished and eligible for retry.
	MarkPublishFailed(ctx context.Context, eventID int64) error

	// CountPending returns the number of unpublished events.
	CountPending(ctx context.Context) (int64, error)

	// CleanupPublished deletes published events older than the retention
	// period. Unpublished events are never removed. Returns rows deleted.
	CleanupPublished(ctx context.Context, retention time.Duration) (int64, error)
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

// QueueDepthRow is one gauge sample for jobforge_queue_depth (PRD 12.1):
// the number of pending jobs for a (tenant, queue, state) combination.
type QueueDepthRow struct {
	TenantID string
	Queue    string
	State    string
	Count    int64
}

// WorkerCountRow is one gauge sample for jobforge_workers_active (PRD 12.1):
// the number of registered workers for a (version, status) combination.
type WorkerCountRow struct {
	Version string
	Status  string
	Count   int64
}

// WorkerRow is one registered worker as surfaced by operational queries
// (stale worker detection). LastHeartbeatAt is nil for rows that never
// reported liveness.
type WorkerRow struct {
	WorkerID        string
	InstanceID      string
	Version         string
	Status          string
	LastHeartbeatAt *time.Time
	RegisteredAt    time.Time
}
