package run

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func checkpointRequest(t *testing.T, step StepIdentity, result StepResult) CommitStepRequest {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	_, canonical, err := CanonicalStepResult(raw, step.Kind)
	if err != nil {
		t.Fatal(err)
	}
	return CommitStepRequest{Step: step, ResultJSON: raw, CommitHash: CommitHash(step, canonical)}
}

func TestCheckpointJSONRejectsAmbiguousProtectedContent(t *testing.T) {
	valid := `{"schema_version":1,"tool_invocation_id":"","physical_call_id":"","evidence_refs":[],"content":null,"proposal":null,"correction_required":false}`
	for name, raw := range map[string]string{
		"empty": "", "whitespace": " \t\n", "array": "[]", "trailing": valid + `{}`,
		"duplicate":              strings.Replace(valid, `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1),
		"alias":                  strings.Replace(valid, `"schema_version"`, `"Schema_Version"`, 1),
		"unknown":                strings.Replace(valid, `"content":null`, `"unknown":null`, 1),
		"null-array":             strings.Replace(valid, `"evidence_refs":[]`, `"evidence_refs":null`, 1),
		"null-element":           strings.Replace(valid, `"evidence_refs":[]`, `"evidence_refs":[null]`, 1),
		"invalid-unicode":        strings.Replace(valid, `"content":null`, `"content":{"value":"\ud800"}`, 1),
		"nul":                    strings.Replace(valid, `"content":null`, `"content":{"value":"\u0000"}`, 1),
		"nested-duplicate":       strings.Replace(valid, `"content":null`, `"content":{"a":1,"a":2}`, 1),
		"missing-proposal-field": strings.Replace(valid, `"proposal":null`, `"proposal":{"decision":"no_action","summary":"s","evidence_refs":[]}`, 1),
		"proposal-alias":         strings.Replace(valid, `"proposal":null`, `"proposal":{"Decision":"no_action","summary":"s","evidence_refs":[],"action":""}`, 1),
		"proposal-null":          strings.Replace(valid, `"proposal":null`, `"proposal":{"decision":"no_action","summary":"s","evidence_refs":[],"action":null}`, 1),
		"oversized":              strings.Repeat(" ", 16385),
		"invalid-utf8":           string([]byte{'{', '"', 0xff, '"', ':', '1', '}'}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := CanonicalStepResult([]byte(raw), "model_proposal"); err == nil {
				t.Fatal("ambiguous JSON accepted")
			}
		})
	}
	if _, _, err := CanonicalStepResult([]byte(valid), "model_proposal"); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointCanonicalNumbersPreserveExactValueAcrossJSONB(t *testing.T) {
	first := []byte(`{"z":1e3,"fraction":1.2300,"negative":-0.0,"precise":9007199254740991,"small":1e-12}`)
	second := []byte(`{"fraction":1.2300,"negative":0.0,"precise":9007199254740991,"small":0.000000000001,"z":1000}`)
	a, err := CanonicalCheckpointJSON(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := CanonicalCheckpointJSON(second)
	if err != nil || !bytes.Equal(a, b) || !bytes.Contains(a, []byte("9007199254740991")) {
		t.Fatalf("numeric identity changed: %s != %s (%v)", a, b, err)
	}
}

func TestCheckpointEvidenceAndProposalGraph(t *testing.T) {
	snapshot := SnapshotBinding{ID: uuid.NewString(), IndexID: uuid.NewString(), Ticket: json.RawMessage(`{"ticket_id":"fixture"}`)}
	profile := Profile{ID: "fixture-v1", Hash: Fingerprint("fixture"), Strategy: BoundedReadonlyStrategy}
	step := StepIdentity{ID: uuid.NewString(), Sequence: 1, Kind: "read_ticket", ProfileID: profile.ID, ProfileHash: profile.Hash}
	ticketRef := "business-evidence:" + snapshot.ID + ":ticket"
	result := StepResult{SchemaVersion: 1, EvidenceRefs: []string{ticketRef}, Content: snapshot.Ticket}
	request := checkpointRequest(t, step, result)
	decision, err := DecideCommit(profile, snapshot, nil, request)
	if err != nil || decision.NextKind != "get_order" {
		t.Fatalf("ticket commit: %+v %v", decision, err)
	}
	prior := []Step{{ID: step.ID, Kind: step.Kind, Output: decision.CanonicalJSON}}
	for _, kind := range []string{"get_order", "get_delivery", "search_policy"} {
		step.ID, step.Kind = uuid.NewString(), kind
		result = StepResult{SchemaVersion: 1, ToolInvocationID: uuid.NewString(), PhysicalCallID: uuid.NewString()}
		if kind == "search_policy" {
			ref := "business-policy:" + snapshot.IndexID + ":P01.1"
			result.EvidenceRefs = []string{ref}
			result.Content = json.RawMessage(`{"snapshot_id":"` + snapshot.ID + `","matches":[{"index_id":"` + snapshot.IndexID + `","chunk_id":"P01.1","evidence_ref":"` + ref + `"}]}`)
		} else {
			evidenceKind := strings.TrimPrefix(kind, "get_")
			ref := "business-evidence:" + snapshot.ID + ":" + evidenceKind
			result.EvidenceRefs = []string{ref}
			result.Content = json.RawMessage(`{"snapshot_id":"` + snapshot.ID + `","kind":"` + evidenceKind + `","evidence_ref":"` + ref + `"}`)
		}
		decision, err = DecideCommit(profile, snapshot, prior, checkpointRequest(t, step, result))
		if err != nil {
			t.Fatal(err)
		}
		prior = append(prior, Step{ID: step.ID, Kind: step.Kind, Output: decision.CanonicalJSON})
	}
	step.ID, step.Kind = uuid.NewString(), "model_proposal"
	result = StepResult{SchemaVersion: 1, PhysicalCallID: uuid.NewString(), EvidenceRefs: []string{}, Content: json.RawMessage(`null`),
		Proposal: &Proposal{Decision: "proposal", Summary: "Synthetic recommendation", EvidenceRefs: []string{ticketRef, "business-policy:" + snapshot.IndexID + ":P01.1"}, Action: "escalate"}}
	decision, err = DecideCommit(profile, snapshot, prior, checkpointRequest(t, step, result))
	if err != nil || decision.NextKind != "submit_proposal" {
		t.Fatalf("model proposal: %+v %v", decision, err)
	}
	prior = append(prior, Step{ID: step.ID, Kind: step.Kind, Output: decision.CanonicalJSON})
	result.Proposal.EvidenceRefs = []string{"business-policy:" + snapshot.IndexID + ":unobserved"}
	if _, err := DecideCommit(profile, snapshot, prior, checkpointRequest(t, step, result)); err == nil {
		t.Fatal("uncommitted evidence accepted")
	}
	result.Proposal.EvidenceRefs = []string{ticketRef, "business-policy:" + snapshot.IndexID + ":P01.1"}
	step.ID, step.Kind, result.PhysicalCallID = uuid.NewString(), "submit_proposal", ""
	decision, err = DecideCommit(profile, snapshot, prior, checkpointRequest(t, step, result))
	if err != nil || !decision.CloseAttempt || decision.Proposal.Decision != "proposal" {
		t.Fatalf("proposal submit: %+v %v", decision, err)
	}
	result.Proposal.Summary = "Changed after model commit"
	if _, err := DecideCommit(profile, snapshot, prior, checkpointRequest(t, step, result)); !errors.Is(err, ErrStepConflict) {
		t.Fatalf("changed final proposal: %v", err)
	}
}

func TestCheckpointCorrectionCannotLoop(t *testing.T) {
	profile := Profile{ID: "fixture-v1", Hash: Fingerprint("fixture"), Strategy: BoundedReadonlyStrategy}
	step := StepIdentity{ID: uuid.NewString(), Kind: "model_proposal", ProfileID: profile.ID, ProfileHash: profile.Hash}
	result := StepResult{SchemaVersion: 1, PhysicalCallID: uuid.NewString(), EvidenceRefs: []string{}, Content: json.RawMessage(`null`), CorrectionRequired: true}
	decision, err := DecideCommit(profile, SnapshotBinding{}, nil, checkpointRequest(t, step, result))
	if err != nil || decision.NextKind != "protocol_correction" {
		t.Fatalf("initial correction: %+v %v", decision, err)
	}
	step.Kind = "protocol_correction"
	if _, err := DecideCommit(profile, SnapshotBinding{}, nil, checkpointRequest(t, step, result)); !errors.Is(err, ErrorCode("MODEL_PROTOCOL_ERROR")) {
		t.Fatalf("correction loop accepted: %v", err)
	}
}

func TestCheckpointByteLimitIsAtomicAndYieldExpiresAtRunDeadline(t *testing.T) {
	r, a, lease, now := runningFixture(t)
	r.ID, lease.RunID, r.ProfileID, r.ProfileHash = uuid.NewString(), "", "fixture-v1", Fingerprint("profile")
	lease.RunID = r.ID
	r.SnapshotID, r.SnapshotHash = uuid.NewString(), Fingerprint("snapshot")
	a.NextStepID, a.NextStepKind, a.NextInputHash = uuid.NewString(), "submit_proposal", Fingerprint("input")
	step := StepIdentity{ID: a.NextStepID, Sequence: r.CursorVersion + 1, CursorVersion: r.CursorVersion, Kind: a.NextStepKind,
		InputHash: a.NextInputHash, ProfileID: r.ProfileID, ProfileHash: r.ProfileHash, SnapshotID: r.SnapshotID, SnapshotHash: r.SnapshotHash}
	request := CommitStepRequest{Lease: lease, Step: step}
	decision := CommitDecision{CanonicalJSON: json.RawMessage(`{}`), CloseAttempt: true, Proposal: &Proposal{Decision: "proposal"}}
	a.CheckpointBytes = MaxCheckpointBytes - 1
	before, beforeA := r, a
	if err := ApplyCommit(&r, &a, request, decision, "", now); !errors.Is(err, ErrorCode("CHECKPOINT_TOO_LARGE")) ||
		!reflect.DeepEqual(r, before) || !reflect.DeepEqual(a, beforeA) {
		t.Fatalf("oversized commit changed state: %v", err)
	}
	a.CheckpointBytes--
	r.RunDeadline = now.Add(5 * time.Minute)
	if err := ApplyCommit(&r, &a, request, decision, "", now); err != nil || r.State != AwaitingApproval ||
		a.CheckpointBytes != MaxCheckpointBytes || !r.PermissionExpiresAt.Equal(r.RunDeadline) {
		t.Fatalf("bounded yield failed: state=%s bytes=%d error=%v", r.State, a.CheckpointBytes, err)
	}
}
