package business

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func signedContractAction(t *testing.T) (SignedAction, ed25519.PrivateKey) {
	t.Helper()
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC).UnixMicro()
	id := "00000000-0000-4000-8000-000000000001"
	p := ResolutionParameters{TicketID: "ticket-1", Decision: "proposal", Action: "record_conclusion", Conclusion: "on_time",
		RequestedFields: []string{}, TargetTicketStatus: "escalated", Summary: "Synthetic conclusion", EvidenceRefs: []string{"business-evidence:fixture:ticket"},
		Claims: json.RawMessage(`[{"kind":"ticket_status","mode":"preserve_escalated","refs":[{"evidence_ref":"business-evidence:fixture:ticket","source_pointer":"/ticket/status"}]}]`)}
	hash, err := p.Hash()
	if err != nil {
		t.Fatal(err)
	}
	a := ActionAuthorization{SchemaVersion: 1, KeyID: "fixture-key", Operation: ResolutionOperation, TenantID: "tenant-north",
		BusinessRequestID: id, BusinessRequestCreatedAt: now - 1000000, OperationID: id, RunID: id, ApprovalID: id, ActorID: "approver-1",
		DecidedAt: now - 100000, ProposalHash: strings.Repeat("a", 64), ParametersHash: hash, SnapshotID: id, SnapshotHash: strings.Repeat("b", 64),
		AuthorizedAt: now, PermissionExpiresAt: now + time.Hour.Microseconds(), AuthorizationExpiresAt: now + time.Hour.Microseconds(), RunDeadline: now + 2*time.Hour.Microseconds(),
		VersionVector: VersionVector{SchemaVersion: 1, Ticket: TicketVersion{ID: "ticket-1", Revision: 1},
			Policy: PolicyRevision{Version: "policy-v1", Revision: 1, CorpusSHA256: strings.Repeat("c", 64)},
			Index:  IndexVersion{ID: id, ProfileHash: strings.Repeat("d", 64), ContentHash: strings.Repeat("e", 64)}}}
	action, err := SignAction(a, p, key)
	if err != nil {
		t.Fatal(err)
	}
	return action, key
}

func TestActionSignatureCoversEveryAuthorizationField(t *testing.T) {
	action, private := signedContractAction(t)
	public := private.Public().(ed25519.PublicKey)
	if err := action.Verify(public); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(action.Authorization)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	for field, value := range fields {
		t.Run(field, func(t *testing.T) {
			changed := action
			var altered map[string]json.RawMessage
			_ = json.Unmarshal(raw, &altered)
			switch {
			case field == "version_vector":
				v := action.Authorization.VersionVector
				v.Ticket.Revision++
				altered[field], _ = json.Marshal(v)
			case len(value) > 0 && value[0] == '"':
				var text string
				_ = json.Unmarshal(value, &text)
				if strings.HasSuffix(field, "_id") && validUUID(text) {
					text = "00000000-0000-4000-8000-000000000002"
				} else if strings.HasSuffix(field, "_hash") {
					text = strings.Repeat("f", 64)
				} else {
					text += "x"
				}
				altered[field], _ = json.Marshal(text)
			default:
				var number int64
				_ = json.Unmarshal(value, &number)
				if field == "authorized_at" || field == "business_request_created_at" || field == "authorization_expires_at" {
					number--
				} else {
					number++
				}
				altered[field], _ = json.Marshal(number)
			}
			encoded, _ := json.Marshal(altered)
			_ = json.Unmarshal(encoded, &changed.Authorization)
			if field != "schema_version" && field != "operation" {
				hash, err := changed.Authorization.Hash()
				if err != nil {
					t.Fatal("signature tamper vector is malformed", err)
				}
				digest, _ := hex.DecodeString(hash)
				signature, _ := base64.RawURLEncoding.DecodeString(action.Signature)
				if ed25519.Verify(public, digest, signature) {
					t.Fatal("legal field alteration retained Ed25519 signature")
				}
			}
			if changed.Verify(public) == nil {
				t.Fatal("altered authorization retained its signature")
			}
		})
	}
	changed := action
	changed.Parameters.Summary += "changed"
	changed.Authorization.ParametersHash, _ = changed.Parameters.Hash()
	if changed.Verify(public) == nil || action.Verify(ed25519.NewKeyFromSeed(bytesOfOne()).Public().(ed25519.PublicKey)) == nil {
		t.Fatal("altered parameter or untrusted key accepted")
	}
}

func TestActionRevisionSafeIntegerBoundary(t *testing.T) {
	action, _ := signedContractAction(t)
	action.Authorization.VersionVector.Policy.Revision = actionSafeInteger
	if _, err := action.Authorization.Hash(); err != nil {
		t.Fatal("safe boundary rejected", err)
	}
	action.Authorization.VersionVector.Policy.Revision++
	if _, err := action.Authorization.Hash(); err == nil {
		t.Fatal("unsafe policy revision accepted")
	}
}

func TestActionReceiptMinimumRetentionBoundary(t *testing.T) {
	action, _ := signedContractAction(t)
	a := action.Authorization
	hash, _ := a.Hash()
	r := ActionReceipt{SchemaVersion: 1, TenantID: a.TenantID, OperationID: a.OperationID,
		BusinessRequestID: a.BusinessRequestID, AuthorizationHash: hash, ProposalHash: a.ProposalHash,
		ParametersHash: a.ParametersHash, ApprovalID: a.ApprovalID, TicketID: action.Parameters.TicketID,
		BeforeRevision: 1, AfterRevision: 2, TicketStatus: action.Parameters.TargetTicketStatus,
		AppliedAt: a.AuthorizedAt + 1}
	r.RetainUntil = r.AppliedAt + 30*24*time.Hour.Microseconds()
	r.ReceiptHash = r.Hash()
	if err := r.Validate(action); err != nil {
		t.Fatal("exact 30 day minimum rejected", err)
	}
	r.RetainUntil--
	r.ReceiptHash = r.Hash()
	if err := r.Validate(action); err != ErrActionConflict {
		t.Fatal("shorter retention accepted", err)
	}
}

func bytesOfOne() []byte {
	value := make([]byte, ed25519.SeedSize)
	value[0] = 1
	return value
}

func TestActionDecodeAndPreservedStatus(t *testing.T) {
	action, key := signedContractAction(t)
	raw, _ := json.Marshal(action)
	decoded, err := DecodeSignedAction(raw)
	if err != nil || decoded.Verify(key.Public().(ed25519.PublicKey)) != nil {
		t.Fatalf("closed action round trip: %v", err)
	}
	for _, body := range []string{
		strings.Replace(string(raw), `"schema_version":1`, `"schema_version":1,"unknown":1`, 1),
		strings.Replace(string(raw), `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1),
		strings.Replace(string(raw), `"exists":false`, `"exists":null`, 1),
		strings.Replace(string(raw), `"exists":false,`, "", 1),
		string(raw) + "{}",
		strings.Replace(string(raw), `"claims":[`, `"claims":null,"ignored":[`, 1),
		strings.Replace(string(raw), `"kind":"ticket_status"`, `"kind":"ticket_status","kind":"ticket_status"`, 1),
		strings.Replace(string(raw), `"kind":"ticket_status"`, `"kind":"ticket_status","unknown":"ignored"`, 1),
		strings.Replace(string(raw), `"mode":"preserve_escalated"`, `"mode":null`, 1),
		strings.Replace(string(raw), `"mode":"preserve_escalated"`, `"mode":"unregistered"`, 1),
	} {
		if _, err := DecodeSignedAction([]byte(body)); err == nil {
			t.Fatal("ambiguous or incomplete signed action accepted")
		}
	}
	for _, status := range []string{"open", "awaiting_information", "escalated", "informational_only"} {
		p := action.Parameters
		p.TargetTicketStatus = status
		if _, err := p.Hash(); err != nil {
			t.Fatalf("existing captured status %s rejected: %v", status, err)
		}
	}
}
