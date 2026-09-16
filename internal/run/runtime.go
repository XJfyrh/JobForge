package run

import (
	"encoding/json"
	"time"
)

// Pricing is a conservative, immutable rational tariff. Values describe an
// estimate under the declared tariff, never an independently verified invoice.
type Pricing struct {
	Hash               string `json:"hash"`
	Denominator        int64  `json:"denominator"`
	InputMissMicroyuan int64  `json:"input_miss_microyuan"`
	InputHitMicroyuan  int64  `json:"input_hit_microyuan"`
	OutputMicroyuan    int64  `json:"output_microyuan"`
}

// Profile freezes the host-registered workflow and physical resource bounds.
// Deployment code supplies it; public requests may only select its immutable ID.
type Profile struct {
	ID                    string          `json:"profile_id"`
	Hash                  string          `json:"profile_hash"`
	Strategy              string          `json:"strategy"`
	ExecutorVersion       string          `json:"executor_version"`
	ProviderAuditPolicy   string          `json:"provider_audit_policy,omitempty"`
	ExpectedResponseModel string          `json:"expected_response_model,omitempty"`
	Executable            bool            `json:"-"`
	MaxInputTokens        int64           `json:"max_input_tokens"`
	MaxOutputTokens       int64           `json:"max_output_tokens"`
	FamilyTokenLimit      int64           `json:"family_token_limit"`
	FamilyCostMicroyuan   int64           `json:"family_cost_microyuan"`
	Pricing               Pricing         `json:"pricing"`
	Definition            json.RawMessage `json:"definition"`
}

// WorkerConfig is administrator-controlled; registration cannot widen it.
type WorkerConfig struct {
	ID         string
	Tenants    []string
	ProfileIDs []string
	Capacity   int
}

// Session binds one authenticated principal to one process startup.
type Session struct {
	WorkerID  string
	ID        string
	StartupID string
	Version   string
	CreatedAt time.Time
	SeenAt    time.Time
	ExpiresAt time.Time
	// AuthorityObservedAt is the fresh database clock used for this response;
	// it is not persisted and does not itself grant or renew session authority.
	AuthorityObservedAt time.Time
}

// Checkpoint is the protected, persisted input to a current or recovered worker.
type Checkpoint struct {
	Run       Run
	Authority Authority
	Snapshot  SnapshotBinding
	Steps     []Step
}

// ClaimedRun combines one fenced lease with its frozen resources and cursor.
type ClaimedRun struct {
	Lease      Lease
	Checkpoint Checkpoint
	// AuthorityObservedAt is the database clock used to grant this lease after
	// acquiring its Run, budget and capacity locks.
	AuthorityObservedAt time.Time
}

// BudgetSpec is accepted only by the administrator setup path.
type BudgetSpec struct {
	ID         string
	Scope      string
	Key        string
	ValidFrom  time.Time
	ValidUntil time.Time
	Limits     Usage
}

// BusinessRequest binds all retries to the same three persistent accounts.
type BusinessRequest struct {
	ID              string
	TenantID        string
	Key             string
	RequestHash     string
	RootRunID       string
	FamilyAccountID string
	TenantAccountID string
	BatchAccountID  string
	CreatedAt       time.Time
	RetryUntil      time.Time
}

// Admission is a trusted service result after external capture and validation.
// Store admission must still recheck idempotency and database time under locks.
type Admission struct {
	TenantID        string
	OperationKey    string
	RequestHash     string
	SourceRunID     string
	Submit          SubmitRequest
	Retry           RetryRequest
	Profile         Profile
	Snapshot        SnapshotBinding
	RunID           string
	BusinessID      string
	FamilyAccountID string
	OperationID     string
	FirstStepID     string
}

// Operation is an accepted idempotent public command, never a work queue.
type Operation struct {
	ID          string
	TenantID    string
	Kind        string
	SourceRunID string
	Scope       string
	Key         string
	RequestHash string
	ResultRunID string
	CreatedAt   time.Time
}
