// Package run defines the durable Agent execution contracts. Only Runs are
// scheduled; steps, tool invocations, and physical calls share their execution
// authority and never acquire independent leases.
package run

import (
	"encoding/json"
	"time"
)

// State is a persisted execution state, independent of the business outcome.
type State string

// Ready and the following constants form the closed Run state set.
const (
	Ready            State = "ready"
	Running          State = "running"
	Stopping         State = "stopping"
	RetryWait        State = "retry_wait"
	AwaitingApproval State = "awaiting_approval"
	Succeeded        State = "succeeded"
	Failed           State = "failed"
	Cancelled        State = "cancelled"
)

// Terminal reports immutable execution states.
func (s State) Terminal() bool { return s == Succeeded || s == Failed || s == Cancelled }

// Valid reports the closed set of persisted states.
func (s State) Valid() bool {
	return s == Ready || s == Running || s == Stopping || s == RetryWait || s == AwaitingApproval || s.Terminal()
}

// ErrorCode is a stable, content-free boundary error.
type ErrorCode string

// ErrInvalidArgument and the following codes are stable transport classifications.
const (
	ErrInvalidArgument       ErrorCode = "INVALID_ARGUMENT"
	ErrUnauthorized          ErrorCode = "UNAUTHORIZED"
	ErrForbidden             ErrorCode = "FORBIDDEN"
	ErrNotFound              ErrorCode = "NOT_FOUND"
	ErrConflict              ErrorCode = "CONFLICT"
	ErrAlreadyTerminal       ErrorCode = "ALREADY_TERMINAL"
	ErrInvalidTransition     ErrorCode = "INVALID_TRANSITION"
	ErrStaleLease            ErrorCode = "STALE_LEASE"
	ErrCancelRequested       ErrorCode = "CANCEL_REQUESTED"
	ErrStopRequested         ErrorCode = "STOP_REQUESTED"
	ErrStepConflict          ErrorCode = "STEP_CONFLICT"
	ErrCallConflict          ErrorCode = "CALL_CONFLICT"
	ErrBudgetExhausted       ErrorCode = "BUDGET_EXHAUSTED"
	ErrProfileUnavailable    ErrorCode = "PROFILE_UNAVAILABLE"
	ErrCallSettlementExpired ErrorCode = "CALL_SETTLEMENT_EXPIRED"
	ErrCheckpointTooLarge    ErrorCode = "CHECKPOINT_TOO_LARGE"
	ErrModelProtocol         ErrorCode = "MODEL_PROTOCOL_ERROR"
	ErrQueueOverloaded       ErrorCode = "QUEUE_OVERLOADED"
	ErrDependencyUnavailable ErrorCode = "DEPENDENCY_UNAVAILABLE"
	ErrInternal              ErrorCode = "INTERNAL"
)

func (e ErrorCode) Error() string { return string(e) }

// Failure belongs to a Run response; a failed Run is still a successful query.
type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Run contains queryable execution facts. Worker authority is kept separately
// in Lease and must never be accepted from the public submit payload.
type Run struct {
	ID                  string          `json:"run_id"`
	TenantID            string          `json:"tenant_id"`
	BusinessRequestID   string          `json:"business_request_id"`
	BusinessRequestKey  string          `json:"business_request_key"`
	TicketID            string          `json:"ticket_id"`
	RetryOfRunID        *string         `json:"retry_of_run_id"`
	ProfileID           string          `json:"profile_id"`
	ProfileHash         string          `json:"profile_hash"`
	BudgetBatchID       string          `json:"budget_batch_id"`
	SnapshotID          string          `json:"snapshot_id"`
	SnapshotHash        string          `json:"snapshot_hash"`
	VersionVector       json.RawMessage `json:"version_vector"`
	State               State           `json:"state"`
	Outcome             *string         `json:"outcome"`
	Error               *Failure        `json:"error"`
	AttemptNo           int64           `json:"attempt_no"`
	RecoveryCount       int64           `json:"recovery_count"`
	CursorVersion       int64           `json:"cursor_version"`
	RunTimeoutSeconds   int64           `json:"run_timeout_seconds"`
	RunDeadline         time.Time       `json:"run_deadline"`
	AttemptDeadline     *time.Time      `json:"attempt_deadline"`
	LeaseUntil          *time.Time      `json:"lease_until"`
	NextAttemptAt       *time.Time      `json:"next_attempt_at"`
	PermissionExpiresAt *time.Time      `json:"permission_expires_at"`
	ProposalRef         *string         `json:"proposal_ref"`
	StopReason          *string         `json:"stop_reason"`
	CancelRequestedAt   *time.Time      `json:"cancel_requested_at"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
	Budget              BudgetView      `json:"budget"`
}

// Usage counts authorized attempts, not only successful network responses.
// Tokens and money in a query represent exposure: known upper cost plus holds.
type Usage struct {
	Chat                int64 `json:"chat"`
	LogicalTools        int64 `json:"logical_tools"`
	QueryEmbedding      int64 `json:"query_embedding"`
	ProfileMetadataHTTP int64 `json:"profile_metadata_http"`
	BusinessToolHTTP    int64 `json:"business_tool_http"`
	PhysicalHTTP        int64 `json:"physical_http"`
	ProtocolCorrections int64 `json:"protocol_corrections"`
	Tokens              int64 `json:"tokens"`
	CostMicroyuan       int64 `json:"cost_microyuan"`
}

// Account is one durable family, tenant, or batch budget.
type Account struct {
	Scope              string `json:"scope"`
	ID                 string `json:"id"`
	Limits             Usage  `json:"limits"`
	Used               Usage  `json:"used"`
	Frozen             bool   `json:"frozen"`
	KnownTokens        int64  `json:"known_tokens"`
	KnownCostMicroyuan int64  `json:"known_cost_microyuan"`
	HeldTokens         int64  `json:"held_tokens"`
	HeldCostMicroyuan  int64  `json:"held_cost_microyuan"`
}

// BudgetView keeps family inheritance visible instead of presenting a new
// retry ID as a fresh budget. Querying it never consumes model quota.
type BudgetView struct {
	Family   Account `json:"family"`
	Tenant   Account `json:"tenant"`
	Batch    Account `json:"batch"`
	RunUsage Usage   `json:"run_usage"`
}

// SubmitRequest contains only user-selectable, registered resources.
type SubmitRequest struct {
	SchemaVersion      int    `json:"schema_version"`
	TicketID           string `json:"ticket_id"`
	BusinessRequestKey string `json:"business_request_key"`
	ProfileID          string `json:"profile_id"`
	BudgetBatchID      string `json:"budget_batch_id"`
	RunTimeoutSeconds  int64  `json:"run_timeout_seconds"`
}

// RetryRequest creates a new execution in the same business and budget family.
type RetryRequest struct {
	SchemaVersion     int   `json:"schema_version"`
	RunTimeoutSeconds int64 `json:"run_timeout_seconds"`
}

// SubmitResponse also represents an idempotently accepted retry.
type SubmitResponse struct {
	Run    Run  `json:"run"`
	Reused bool `json:"reused"`
}

// CancelResponse identifies the accepted command and the current Run view.
type CancelResponse struct {
	OperationID string `json:"operation_id"`
	Run         Run    `json:"run"`
	Reused      bool   `json:"reused"`
}

// Result never confuses a proposal with an applied business effect.
type Result struct {
	Available bool    `json:"available"`
	Kind      *string `json:"kind"`
	Ref       *string `json:"ref"`
}

// Step is protected checkpoint data, not a log or an independently leased job.
type Step struct {
	ID            string          `json:"step_id"`
	Sequence      int64           `json:"sequence"`
	Kind          string          `json:"kind"`
	InputHash     string          `json:"input_hash"`
	ProfileHash   string          `json:"profile_hash"`
	SnapshotHash  string          `json:"snapshot_hash"`
	CommitHash    string          `json:"commit_hash"`
	OutputRef     string          `json:"output_ref"`
	Output        json.RawMessage `json:"output"`
	CursorVersion int64           `json:"cursor_version"`
	CreatedAt     time.Time       `json:"created_at"`
}

// Event exposes content-free, Run-local ordered history.
type Event struct {
	Sequence      int64     `json:"sequence"`
	Type          string    `json:"event_type"`
	State         State     `json:"state"`
	AttemptNo     int64     `json:"attempt_no"`
	CursorVersion int64     `json:"cursor_version"`
	CreatedAt     time.Time `json:"created_at"`
}

// ListFilter uses stable keyset pagination inside the authenticated tenant.
type ListFilter struct {
	State  State
	Cursor string
	Limit  int
}

// Page is the public Run listing envelope.
type Page struct {
	Items      []Run   `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// StepPage contains a bounded portion of protected checkpoints.
type StepPage struct {
	Items     []Step `json:"items"`
	NextAfter *int64 `json:"next_after"`
}

// EventPage contains metadata only.
type EventPage struct {
	Items     []Event `json:"items"`
	NextAfter *int64  `json:"next_after"`
}

// Lease is internal execution authority bound to an authenticated principal.
type Lease struct {
	TenantID     string
	RunID        string
	WorkerID     string
	SessionID    string
	AttemptNo    int64
	FencingToken int64
}

// Authority is the persisted, non-public part of a Run's current execution.
type Authority struct {
	WorkerID        string
	SessionID       string
	FencingToken    int64
	ActiveCallID    *string
	CheckpointBytes int64
	EventSequence   int64
	NextStepID      string
	NextStepKind    string
	NextInputHash   string
}

// SnapshotBinding is supplied only by the trusted business adapter, outside a
// control transaction. VersionVector is validated there against the schema.
type SnapshotBinding struct {
	TenantID         string
	TicketID         string
	ID               string
	ContentHash      string
	VersionVector    json.RawMessage
	Ticket           json.RawMessage
	IndexID          string
	IndexProfileHash string
}
