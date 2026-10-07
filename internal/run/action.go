package run

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xjfyrh/jobforge/internal/business"
)

const (
	// SupportApprovalExecutorVersion adds the Go-owned action step to S3.
	SupportApprovalExecutorVersion = "linux-v2-approval-runtime-1"
	// TicketResolutionApprovalPolicy is opt-in through approval-capable schemas.
	TicketResolutionApprovalPolicy = "ticket_resolution_v1"
	// MaxActionCalls is separate from the unchanged model/tool physical ledger.
	MaxActionCalls = 8
)

// ApprovalEnabled checks the complete immutable capability, not a pending state.
func (p Profile) ApprovalEnabled() bool {
	d, err := DecodeSupportDefinition(p.Definition)
	if err != nil || (d.SchemaVersion != 4 && d.SchemaVersion != 6) || p.ExecutorVersion != SupportApprovalExecutorVersion {
		return false
	}
	hash, err := SupportProfileHash(p)
	return err == nil && hash == p.Hash
}

// ApprovalRequest never accepts edited proposal content or an actor identity.
type ApprovalRequest struct {
	SchemaVersion int    `json:"schema_version"`
	Decision      string `json:"decision"`
	ProposalHash  string `json:"proposal_hash"`
}

// Validate accepts only the original proposal hash and a closed decision.
func (r ApprovalRequest) Validate() error {
	if r.SchemaVersion != 1 || (r.Decision != "approve" && r.Decision != "reject") || !ValidHash(r.ProposalHash) {
		return ErrInvalidArgument
	}
	return nil
}

// Hash binds the authenticated stable actor into idempotency.
func (r ApprovalRequest) Hash(tenant, id, actor string) string {
	return Fingerprint("jobforge.run.approval.v1", "1", tenant, id, actor, r.Decision, r.ProposalHash)
}

// ApprovalView preserves the first decision independently of later Run state.
type ApprovalView struct {
	RunID               string     `json:"run_id"`
	Status              string     `json:"status"`
	ProposalHash        string     `json:"proposal_hash"`
	ProposalRef         string     `json:"proposal_ref"`
	Proposal            *Proposal  `json:"proposal"`
	PermissionExpiresAt time.Time  `json:"permission_expires_at"`
	ApprovalID          *string    `json:"approval_id"`
	ActorID             *string    `json:"actor_id"`
	DecidedAt           *time.Time `json:"decided_at"`
	Available           bool       `json:"available"`
}

// ApprovalResponse returns the first decision and current execution view.
type ApprovalResponse struct {
	Approval ApprovalView `json:"approval"`
	Run      Run          `json:"run"`
	Reused   bool         `json:"reused"`
}

// DecideApproval changes only the Run domain and next registered cursor.
func DecideApproval(r *Run, a *Authority, request ApprovalRequest, proposalCommitHash, decisionHash, nextID string, now time.Time) error {
	if r.State != AwaitingApproval || r.CancelRequestedAt != nil {
		return ErrInvalidTransition
	}
	if r.PermissionExpiresAt == nil || !r.PermissionExpiresAt.After(now) || !r.RunDeadline.After(now) {
		return ErrApprovalExpired
	}
	if request.Decision == "reject" {
		outcome := "rejected"
		r.State, r.Outcome = Succeeded, &outcome
	} else {
		r.State = Ready
		a.NextStepID, a.NextStepKind = nextID, business.ResolutionOperation
		a.NextInputHash = Fingerprint("jobforge.run.action-input.v1", r.ProfileHash, r.SnapshotHash, proposalCommitHash, decisionHash)
	}
	r.UpdatedAt = now
	return nil
}

// ResolutionFromProposal reconstructs exactly the accepted eight-field proposal.
func ResolutionFromProposal(ticket string, proposal *Proposal) (business.ResolutionParameters, error) {
	if proposal == nil || proposal.SupportProposalFields == nil {
		return business.ResolutionParameters{}, ErrInvalidArgument
	}
	claims, err := json.Marshal(proposal.Claims)
	if err != nil {
		return business.ResolutionParameters{}, ErrInvalidArgument
	}
	p := business.ResolutionParameters{TicketID: ticket, Decision: proposal.Decision, Action: proposal.Action,
		Conclusion: proposal.Conclusion, RequestedFields: proposal.RequestedFields, TargetTicketStatus: proposal.TargetTicketStatus,
		Summary: proposal.Summary, Claims: claims, EvidenceRefs: proposal.EvidenceRefs}
	if _, err := p.Hash(); err != nil {
		return p, ErrInvalidArgument
	}
	return p, nil
}

// EffectView can advance without modifying terminal execution facts.
type EffectView struct {
	State             string                  `json:"state"`
	OperationID       *string                 `json:"operation_id"`
	AuthorizationHash *string                 `json:"authorization_hash"`
	EffectVersion     int64                   `json:"effect_version"`
	ReceiptRef        *string                 `json:"receipt_ref"`
	Receipt           *business.ActionReceipt `json:"receipt"`
	Source            *string                 `json:"source"`
	ObservedAt        *time.Time              `json:"observed_at"`
}

// ActionBinding is the protected immutable action plus independent effect.
type ActionBinding struct {
	Action *business.SignedAction `json:"action"`
	Effect EffectView             `json:"effect"`
}

// ActionCall records physical permission without provider usage or money fields.
type ActionCall struct {
	ID                string       `json:"physical_call_id"`
	Lease             Lease        `json:"-"`
	Step              StepIdentity `json:"-"`
	OperationID       string       `json:"operation_id"`
	AuthorizationHash string       `json:"authorization_hash"`
	Kind              string       `json:"kind"`
	Status            string       `json:"status"`
	ReservedAt        time.Time    `json:"reserved_at"`
	DispatchExpiresAt time.Time    `json:"dispatch_expires_at"`
	Deadline          time.Time    `json:"call_deadline"`
	TransportOutcome  *string      `json:"transport_outcome"`
	ObservationHash   *string      `json:"observation_hash"`
	ObservedAt        *time.Time   `json:"observed_at"`
	ProviderMetering  string       `json:"provider_metering"`
}

// ActionCallsResponse contains at most eight original-Run physical permits.
type ActionCallsResponse struct {
	Items []ActionCall `json:"items"`
}

// ReserveActionCallRequest binds a physical UUID to fenced action execution.
type ReserveActionCallRequest struct {
	Lease             Lease
	Step              StepIdentity
	ID                string
	Kind              string
	OperationID       string
	AuthorizationHash string
}

// ReserveActionCallResponse permits dispatch only when NewlyReserved is true.
type ReserveActionCallResponse struct {
	Call          ActionCall
	NewlyReserved bool
}

// ObserveActionCallRequest records original transport facts without changing Run state.
type ObserveActionCallRequest struct {
	Lease            Lease
	ID               string
	TransportOutcome string
	ObservationHash  string
}

// CompleteActionRequest advances only current authority with a validated receipt.
type CompleteActionRequest struct {
	Lease          Lease
	Step           StepIdentity
	Receipt        business.ActionReceipt
	PhysicalCallID string
}

// ActionStore keeps public action queries separate from model admission.
type ActionStore interface {
	Approval(context.Context, string, string) (ApprovalView, error)
	DecideApproval(context.Context, string, string, string, string, ApprovalRequest) (ApprovalResponse, error)
	Effect(context.Context, string, string) (EffectView, error)
	ActionCalls(context.Context, string, string) (ActionCallsResponse, error)
}

// Approval returns tenant-authorized original proposal and decision facts.
func (s *Service) Approval(ctx context.Context, tenant, id string) (ApprovalView, error) {
	store, ok := s.store.(ActionStore)
	if !ok {
		return ApprovalView{}, ErrProfileUnavailable
	}
	return store.Approval(ctx, tenant, id)
}

// DecideApproval delegates atomic approval and idempotency to the store.
func (s *Service) DecideApproval(ctx context.Context, tenant, id, actor, key string, request ApprovalRequest) (ApprovalResponse, error) {
	store, ok := s.store.(ActionStore)
	if !ok {
		return ApprovalResponse{}, ErrProfileUnavailable
	}
	return store.DecideApproval(ctx, tenant, id, actor, key, request)
}

// Effect reads independently observed business facts.
func (s *Service) Effect(ctx context.Context, tenant, id string) (EffectView, error) {
	store, ok := s.store.(ActionStore)
	if !ok {
		return EffectView{}, ErrProfileUnavailable
	}
	return store.Effect(ctx, tenant, id)
}

// ActionCalls reads physical action permits without provider metering.
func (s *Service) ActionCalls(ctx context.Context, tenant, id string) (ActionCallsResponse, error) {
	store, ok := s.store.(ActionStore)
	if !ok {
		return ActionCallsResponse{}, ErrProfileUnavailable
	}
	return store.ActionCalls(ctx, tenant, id)
}
