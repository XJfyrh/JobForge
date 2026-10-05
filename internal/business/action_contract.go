package business

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/xjfyrh/jobforge/internal/jsonstrict"
)

const (
	// ResolutionOperation is the only registered business write.
	ResolutionOperation = "apply_ticket_resolution"
	// ActionRequestMaxBytes bounds the signed proposal, not ordinary tool output.
	ActionRequestMaxBytes = 32 * 1024
	// ActionReceiptRetention is a minimum, not a read permission expiry.
	ActionReceiptRetention       = 30 * 24 * time.Hour
	actionSafeInteger      int64 = 9007199254740991
)

var (
	// ErrActionConflict preserves the first operation and rejects changed facts.
	ErrActionConflict = errors.New("business action conflict")
	// ErrAuthorizationExpired never authorizes a first write after permission expiry.
	ErrAuthorizationExpired = errors.New("business action authorization expired")
	// ErrActionForbidden rejects untrusted action identities or signatures.
	ErrActionForbidden = errors.New("business action forbidden")
)

// ResolutionParameters is reconstructed from the immutable approved proposal.
// Claims stay protected JSON; no field can select another operation or endpoint.
type ResolutionParameters struct {
	TicketID           string          `json:"ticket_id"`
	Decision           string          `json:"decision"`
	Action             string          `json:"action"`
	Conclusion         string          `json:"conclusion"`
	RequestedFields    []string        `json:"requested_fields"`
	TargetTicketStatus string          `json:"target_ticket_status"`
	Summary            string          `json:"summary"`
	Claims             json.RawMessage `json:"claims"`
	EvidenceRefs       []string        `json:"evidence_refs"`
}

// ActionAuthorization is immutable. Times are UTC Unix microseconds and all
// dependent versions are covered by the Ed25519 signed digest.
type ActionAuthorization struct {
	SchemaVersion            int           `json:"schema_version"`
	KeyID                    string        `json:"key_id"`
	Operation                string        `json:"operation"`
	TenantID                 string        `json:"tenant_id"`
	BusinessRequestID        string        `json:"business_request_id"`
	BusinessRequestCreatedAt int64         `json:"business_request_created_at"`
	OperationID              string        `json:"operation_id"`
	RunID                    string        `json:"run_id"`
	ApprovalID               string        `json:"approval_id"`
	ActorID                  string        `json:"actor_id"`
	DecidedAt                int64         `json:"decided_at"`
	ProposalHash             string        `json:"proposal_hash"`
	ParametersHash           string        `json:"parameters_hash"`
	SnapshotID               string        `json:"snapshot_id"`
	SnapshotHash             string        `json:"snapshot_hash"`
	VersionVector            VersionVector `json:"version_vector"`
	AuthorizedAt             int64         `json:"authorized_at"`
	PermissionExpiresAt      int64         `json:"permission_expires_at"`
	AuthorizationExpiresAt   int64         `json:"authorization_expires_at"`
	RunDeadline              int64         `json:"run_deadline"`
}

// SignedAction is an approved content identity, not a physical send permit.
type SignedAction struct {
	Authorization ActionAuthorization  `json:"authorization"`
	Parameters    ResolutionParameters `json:"parameters"`
	Signature     string               `json:"signature"`
}

// ActionReceipt records the first atomic ticket update and its immutable source.
type ActionReceipt struct {
	SchemaVersion     int    `json:"schema_version"`
	TenantID          string `json:"tenant_id"`
	OperationID       string `json:"operation_id"`
	BusinessRequestID string `json:"business_request_id"`
	AuthorizationHash string `json:"authorization_hash"`
	ProposalHash      string `json:"proposal_hash"`
	ParametersHash    string `json:"parameters_hash"`
	ApprovalID        string `json:"approval_id"`
	TicketID          string `json:"ticket_id"`
	BeforeRevision    int64  `json:"before_revision"`
	AfterRevision     int64  `json:"after_revision"`
	TicketStatus      string `json:"ticket_status"`
	AppliedAt         int64  `json:"applied_at"`
	RetainUntil       int64  `json:"retain_until"`
	ReceiptHash       string `json:"receipt_hash"`
}

// ActionFingerprint shares the Run's explicit domain-separated field encoding.
func ActionFingerprint(domain string, fields ...string) string {
	h := sha256.New()
	for _, field := range append([]string{domain}, fields...) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(field))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func actionJSON(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, ErrInvalidArgument
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object any
	if decoder.Decode(&object) != nil {
		return nil, ErrInvalidArgument
	}
	return json.Marshal(object)
}

// Hash freezes exact parameters, including all evidence and array ordering.
func (p ResolutionParameters) Hash() (string, error) {
	if !validID(p.TicketID) || p.Decision != "proposal" || !validText(p.Summary, 8192) || p.Summary == "" ||
		p.RequestedFields == nil || p.EvidenceRefs == nil || len(p.EvidenceRefs) < 1 || len(p.EvidenceRefs) > 32 || len(p.RequestedFields) > 5 {
		return "", ErrInvalidArgument
	}
	allowedStatus := map[string]string{"request_information": "awaiting_information", "escalate": "escalated"}
	if p.Action == "record_conclusion" {
		if p.TargetTicketStatus != "open" && p.TargetTicketStatus != "informational_only" && p.TargetTicketStatus != "escalated" && p.TargetTicketStatus != "awaiting_information" {
			return "", ErrInvalidArgument
		}
	} else if allowedStatus[p.Action] == "" || p.TargetTicketStatus != allowedStatus[p.Action] {
		return "", ErrInvalidArgument
	}
	if p.Conclusion != "on_time" && p.Conclusion != "delayed" && p.Conclusion != "disputed" && p.Conclusion != "insufficient" && p.Conclusion != "conflicting" {
		return "", ErrInvalidArgument
	}
	if !validResolutionClaims(p) {
		return "", ErrInvalidArgument
	}
	raw, err := actionJSON(p)
	if err != nil || len(raw) > 16384 {
		return "", ErrInvalidArgument
	}
	return ActionFingerprint("jobforge.business.parameters.v1", "1", string(raw)), nil
}

// Hash covers every immutable authorization fact; Signature is outside it.
func (a ActionAuthorization) Hash() (string, error) {
	if a.SchemaVersion != 1 || a.Operation != ResolutionOperation || !validID(a.KeyID) || !validID(a.TenantID) || !validID(a.ActorID) ||
		!validUUID(a.BusinessRequestID) || !validUUID(a.OperationID) || !validUUID(a.RunID) || !validUUID(a.ApprovalID) || !validUUID(a.SnapshotID) ||
		!validHash(a.ParametersHash) || !validHash(a.ProposalHash) || !validHash(a.SnapshotHash) {
		return "", ErrInvalidArgument
	}
	for _, value := range []int64{a.BusinessRequestCreatedAt, a.DecidedAt, a.AuthorizedAt, a.PermissionExpiresAt, a.AuthorizationExpiresAt, a.RunDeadline} {
		if value < 1 || value > actionSafeInteger {
			return "", ErrInvalidArgument
		}
	}
	if a.DecidedAt < a.BusinessRequestCreatedAt || a.AuthorizedAt < a.DecidedAt || a.AuthorizationExpiresAt <= a.AuthorizedAt ||
		a.AuthorizationExpiresAt > a.PermissionExpiresAt || a.PermissionExpiresAt > a.RunDeadline {
		return "", ErrInvalidArgument
	}
	v := a.VersionVector
	if v.SchemaVersion != 1 || !validID(v.Ticket.ID) || v.Ticket.Revision < 1 || v.Ticket.Revision >= actionSafeInteger ||
		v.Order.Exists != (v.Order.Revision != nil) || v.Delivery.Exists != (v.Delivery.AggregateRevision != nil) ||
		(v.Order.Exists && v.Order.ID == nil) || (v.Delivery.Exists && v.Delivery.ID == nil) ||
		(!v.Order.Exists && v.Delivery.ID != nil) ||
		!validID(v.Policy.Version) || v.Policy.Revision < 1 || v.Policy.Revision > actionSafeInteger || !validHash(v.Policy.CorpusSHA256) ||
		!validUUID(v.Index.ID) || !validHash(v.Index.ProfileHash) || !validHash(v.Index.ContentHash) {
		return "", ErrInvalidArgument
	}
	for _, id := range []*string{v.Order.ID, v.Delivery.ID} {
		if id != nil && !validID(*id) {
			return "", ErrInvalidArgument
		}
	}
	for _, revision := range []*int64{v.Order.Revision, v.Delivery.AggregateRevision} {
		if revision != nil && (*revision < 1 || *revision > actionSafeInteger) {
			return "", ErrInvalidArgument
		}
	}
	vector, err := actionJSON(v)
	if err != nil {
		return "", err
	}
	vectorHash := ActionFingerprint("jobforge.business.version-vector.v1", "1", string(vector))
	integer := func(n int64) string { return strconv.FormatInt(n, 10) }
	return ActionFingerprint("jobforge.business.authorization.v1", "1", a.KeyID, a.Operation, a.TenantID,
		a.BusinessRequestID, integer(a.BusinessRequestCreatedAt), a.OperationID, a.RunID, a.ApprovalID, a.ActorID, integer(a.DecidedAt),
		a.ProposalHash, a.ParametersHash, a.SnapshotID, a.SnapshotHash, vectorHash, integer(a.AuthorizedAt),
		integer(a.PermissionExpiresAt), integer(a.AuthorizationExpiresAt), integer(a.RunDeadline)), nil
}

// SignAction is called only after the control transaction checks approval/lease.
func SignAction(a ActionAuthorization, p ResolutionParameters, key ed25519.PrivateKey) (SignedAction, error) {
	hash, err := p.Hash()
	if err != nil || hash != a.ParametersHash || a.VersionVector.Ticket.ID != p.TicketID || len(key) != ed25519.PrivateKeySize {
		return SignedAction{}, ErrInvalidArgument
	}
	hash, err = a.Hash()
	if err != nil {
		return SignedAction{}, err
	}
	digest, _ := hex.DecodeString(hash)
	return SignedAction{Authorization: a, Parameters: p, Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, digest))}, nil
}

// Verify requires a deployment-trusted public key, never one in request data.
func (s SignedAction) Verify(key ed25519.PublicKey) error {
	p, err := s.Parameters.Hash()
	if err != nil || p != s.Authorization.ParametersHash || s.Parameters.TicketID != s.Authorization.VersionVector.Ticket.ID {
		return ErrActionForbidden
	}
	hash, err := s.Authorization.Hash()
	signature, decodeErr := base64.RawURLEncoding.DecodeString(s.Signature)
	if err != nil || decodeErr != nil || len(key) != ed25519.PublicKeySize || len(signature) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(signature) != s.Signature {
		return ErrActionForbidden
	}
	digest, _ := hex.DecodeString(hash)
	if !ed25519.Verify(key, digest, signature) {
		return ErrActionForbidden
	}
	return nil
}

// DecodeSignedAction rejects missing and unknown typed fields before hashing.
// Typed reconstruction also detects explicit nulls and non-integer numbers.
func DecodeSignedAction(raw []byte) (SignedAction, error) {
	var action SignedAction
	if len(raw) > ActionRequestMaxBytes || jsonstrict.Decode(raw, &action) != nil {
		return action, ErrInvalidArgument
	}
	var fields any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&fields) != nil {
		return action, ErrInvalidArgument
	}
	actual, _ := json.Marshal(fields)
	expected, err := actionJSON(action)
	if err != nil || !bytes.Equal(actual, expected) {
		return action, ErrInvalidArgument
	}
	if _, err := action.Authorization.Hash(); err != nil {
		return action, err
	}
	if _, err := action.Parameters.Hash(); err != nil {
		return action, err
	}
	return action, nil
}

// PublicKeyHash is a non-secret deployment/profile binding.
func PublicKeyHash(key ed25519.PublicKey) string {
	hash := sha256.Sum256(key)
	return hex.EncodeToString(hash[:])
}

// Hash covers the complete first receipt except its own digest.
func (r ActionReceipt) Hash() string {
	raw, _ := json.Marshal(r)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	delete(fields, "receipt_hash")
	canonical, _ := actionJSON(fields)
	return ActionFingerprint("jobforge.business.receipt.v1", "1", string(canonical))
}

// Validate binds an actual business response to its original approved content.
func (r ActionReceipt) Validate(action SignedAction) error {
	a := action.Authorization
	hash, err := a.Hash()
	if err != nil || r.SchemaVersion != 1 || r.TenantID != a.TenantID || r.OperationID != a.OperationID ||
		r.BusinessRequestID != a.BusinessRequestID || r.AuthorizationHash != hash || r.ProposalHash != a.ProposalHash ||
		r.ParametersHash != a.ParametersHash || r.ApprovalID != a.ApprovalID || r.TicketID != action.Parameters.TicketID ||
		r.TicketStatus != action.Parameters.TargetTicketStatus ||
		r.BeforeRevision != a.VersionVector.Ticket.Revision || r.AfterRevision != r.BeforeRevision+1 ||
		r.AppliedAt < a.AuthorizedAt || r.AppliedAt >= a.AuthorizationExpiresAt || r.RetainUntil > actionSafeInteger ||
		r.RetainUntil < r.AppliedAt+ActionReceiptRetention.Microseconds() || r.ReceiptHash != r.Hash() {
		return ErrActionConflict
	}
	return nil
}
