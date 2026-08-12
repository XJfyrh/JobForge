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
	// attempt, fencing_token and state are all updated atomically. When a
	// tenant quota is active, each claimed job's slot is reserved on the
	// derived counter in the same transaction (PRD v0.3 FR-721, ADR-0007).
	Claim(ctx context.Context, params ClaimParams) (*ClaimResult, error)

	// Heartbeat extends the lease for a running/cancelling job. Only the
	// current owner with the correct fencing token may extend. The result is
	// derived from the same PostgreSQL clock sample used by the update so the
	// Gateway never estimates lease or cancel latency with its host clock.
	Heartbeat(ctx context.Context, jobID, workerID string, fencingToken int64, ttl time.Duration) (*HeartbeatResult, error)

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

// HeartbeatResult is the database-authored outcome of one successful lease
// renewal. CancelSignalLatency is populated only when CancelRequested is true.
type HeartbeatResult struct {
	LeaseUntil          time.Time
	CancelRequested     bool
	CancelSignalLatency time.Duration
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

	// TenantMaxInflight limits how many inflight (running + cancelling) jobs
	// a tenant can have. If <= 0, no limit is enforced.
	TenantMaxInflight int

	// QuotaPrefilter enables the candidate pre-filter that excludes full
	// tenants before the row-lock window (PRD v0.3 FR-725/726, ADR-0007 §4).
	// Disabling it only costs fairness performance: the in-transaction atomic
	// reservation still enforces the hard cap.
	QuotaPrefilter bool
}

// ClaimResult carries the outcome of a claim operation.
type ClaimResult struct {
	// Jobs are the successfully claimed jobs.
	Jobs []*domain.Job

	// QuotaConflicts counts candidates skipped because their tenant's atomic
	// slot reservation hit the hard cap (pre-filter staleness). Feeds
	// jobforge_quota_reservation_conflicts_total.
	QuotaConflicts int

	// MaxObservedInflight is the highest counter value returned by a
	// successful in-transaction slot reservation (post-increment). It lets
	// tests assert the hard cap held inside every claim transaction (AT-21);
	// 0 when no quota reservation ran.
	MaxObservedInflight int
}

// OutboxEvent represents one row of the outbox_events table. Events are
// written within job state transactions and published asynchronously by the
// outbox publisher (PRD v0.2 FR-610~612).
//
// AggregateVersion and Traceparent (migration 0016) capture envelope v1
// fields at event write time (PRD v0.3 FR-703, ADR-0006 §4). Both are NULL
// for legacy rows written before 0016: the envelope renders a nil
// AggregateVersion as 0 (unknown) and a nil Traceparent as empty.
type OutboxEvent struct {
	EventID          int64
	AggregateID      string
	EventType        string
	Payload          []byte
	CreatedAt        time.Time
	PublishedAt      *time.Time
	PublishAttempts  int
	AggregateVersion *int64
	Traceparent      *string
}

// OutboxStore defines the persistence operations consumed by the outbox
// publisher. All operations run outside job state transactions and never
// modify job core state.
type OutboxStore interface {
	// FetchUnpublished atomically claims up to batch unpublished events
	// ordered by created_at: a single UPDATE...FOR UPDATE SKIP LOCKED
	// statement stamps claimed_at, so concurrent publishers never claim the
	// same event. Events claimed but not published within the claim TTL are
	// reclaimable (crashed-publisher recovery).
	FetchUnpublished(ctx context.Context, batch int) ([]*OutboxEvent, error)

	// MarkPublished records successful publication. Returns true if this call
	// performed the transition (published_at was still NULL).
	MarkPublished(ctx context.Context, eventID int64) (bool, error)

	// MarkPublishedBatch records successful publication for a whole batch in
	// one statement (publisher throughput, NFR-302). Returns how many rows
	// this call transitioned.
	MarkPublishedBatch(ctx context.Context, eventIDs []int64) (int64, error)

	// MarkPublishFailed increments publish_attempts; the event remains
	// unpublished and eligible for retry.
	MarkPublishFailed(ctx context.Context, eventID int64) error

	// ResetClaim releases the atomic claim (claimed_at back to NULL) so an
	// unpublished event becomes immediately reclaimable again: used after a
	// failed publish or when a claimed event was left unprocessed.
	ResetClaim(ctx context.Context, eventID int64) error

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

// QuotaDriftRow is one tenant whose derived inflight counter disagrees with
// the jobs aggregation (PRD v0.3 FR-724). Counter is the tenant_quota_counters
// value (0 when the row is missing), Actual the jobs running+cancelling count.
type QuotaDriftRow struct {
	TenantID string
	Counter  int64
	Actual   int64
}
