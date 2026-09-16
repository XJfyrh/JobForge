package runinput

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/xjfyrh/jobforge/internal/run"
	runprotocol "github.com/xjfyrh/jobforge/internal/runprotocol/v2"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

var updateFixtures = flag.Bool("update-runtime-fixtures", false, "regenerate synthetic runtime input fixtures with authoritative Go hashes")

const testAdapter = "bounded-readonly-mechanism-v1"

type fixture struct {
	Name  string          `json:"name"`
	Frame json.RawMessage `json:"frame"`
}

type rawFixture struct {
	Name string `json:"name"`
	JSON string `json:"json"`
}

type fixtureFile struct {
	SchemaVersion int          `json:"schema_version"`
	Valid         []fixture    `json:"valid"`
	Invalid       []fixture    `json:"invalid"`
	InvalidJSON   []rawFixture `json:"invalid_json"`
}

func TestSharedRuntimeFixtures(t *testing.T) {
	path := filepath.Join("..", "..", "api", "executor", "v2", "fixtures", "runtime-input.json")
	if *updateFixtures {
		encoded, err := json.MarshalIndent(makeFixtures(t), "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(encoded, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var vectors fixtureFile
	if err = json.Unmarshal(data, &vectors); err != nil || vectors.SchemaVersion != 1 || len(vectors.Valid) < 4 || len(vectors.Invalid) < 15 {
		t.Fatalf("invalid fixture source: %v", err)
	}
	for _, vector := range vectors.Valid {
		t.Run(vector.Name, func(t *testing.T) {
			f, err := runprotocol.Decode(append(compact(t, vector.Frame), '\n'))
			if err != nil {
				t.Fatal(err)
			}
			if err = ValidateExecute(f); err != nil {
				t.Fatal(err)
			}
			lease, cp, selection := reverseProjection(t, f)
			built, err := BuildExecute(lease, cp, selection, f.RequestID, f.EmittedMonoMS, f.EmittedMonoMS+f.RemainingMS)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := runprotocol.Encode(built)
			want, _ := runprotocol.Encode(f)
			if !bytes.Equal(got, want) {
				t.Fatal("RPC projection differs from shared frame")
			}
		})
	}
	for _, vector := range vectors.Invalid {
		t.Run(vector.Name, func(t *testing.T) { assertRejected(t, append(compact(t, vector.Frame), '\n')) })
	}
	for _, vector := range vectors.InvalidJSON {
		t.Run(vector.Name, func(t *testing.T) { assertRejected(t, []byte(vector.JSON)) })
	}
}

func assertRejected(t *testing.T, line []byte) {
	t.Helper()
	f, err := runprotocol.Decode(line)
	if err == nil {
		err = ValidateExecute(f)
	}
	if err == nil {
		t.Fatal("accepted invalid runtime input")
	}
}

func TestBuildExecuteRejectsRPCBoundaryFailures(t *testing.T) {
	base := exampleFrame(t, "submit_proposal")
	cases := []struct {
		name string
		edit func(*agentv1.RunLease, *agentv1.Checkpoint, *Selection)
		want error
	}{
		{"missing execution", func(l *agentv1.RunLease, _ *agentv1.Checkpoint, _ *Selection) { l.Execution = nil }, run.ErrInvalidArgument},
		{"missing claim checkpoint", func(l *agentv1.RunLease, _ *agentv1.Checkpoint, _ *Selection) { l.Checkpoint = nil }, run.ErrInvalidArgument},
		{"claim profile changed", func(l *agentv1.RunLease, _ *agentv1.Checkpoint, _ *Selection) {
			l.Checkpoint.NextStep.ProfileHash = strings.Repeat("f", 64)
		}, run.ErrStepConflict},
		{"claim resource content changed", func(l *agentv1.RunLease, _ *agentv1.Checkpoint, _ *Selection) {
			l.Checkpoint.Snapshot.TicketBindingJson = bytes.ReplaceAll(l.Checkpoint.Snapshot.TicketBindingJson, []byte("synthetic"), []byte("modified"))
		}, run.ErrStepConflict},
		{"claim float revision", func(l *agentv1.RunLease, _ *agentv1.Checkpoint, _ *Selection) {
			l.Checkpoint.Snapshot.TicketBindingJson = bytes.Replace(l.Checkpoint.Snapshot.TicketBindingJson, []byte(`"revision":1`), []byte(`"revision":1.0`), 1)
		}, run.ErrInvalidArgument},
		{"terminal checkpoint", func(_ *agentv1.RunLease, c *agentv1.Checkpoint, _ *Selection) { c.NextStep = nil }, run.ErrInvalidArgument},
		{"null accepted step", func(_ *agentv1.RunLease, c *agentv1.Checkpoint, _ *Selection) { c.Steps[0] = nil }, run.ErrInvalidArgument},
		{"unknown enum", func(_ *agentv1.RunLease, c *agentv1.Checkpoint, _ *Selection) { c.NextStep.Kind = 99 }, run.ErrInvalidArgument},
		{"unsafe attempt", func(l *agentv1.RunLease, _ *agentv1.Checkpoint, _ *Selection) {
			l.Execution.AttemptNo = run.MaxSafeInteger + 1
		}, run.ErrInvalidArgument},
		{"duplicate result key", func(_ *agentv1.RunLease, c *agentv1.Checkpoint, _ *Selection) {
			c.Steps[0].ResultJson = bytes.Replace(c.Steps[0].ResultJson, []byte(`"schema_version":1`), []byte(`"schema_version":1,"schema_version":1`), 1)
		}, run.ErrInvalidArgument},
		{"duplicate vector key", func(_ *agentv1.RunLease, c *agentv1.Checkpoint, _ *Selection) {
			c.Snapshot.VersionVectorJson = bytes.Replace(c.Snapshot.VersionVectorJson, []byte(`"schema_version":1`), []byte(`"schema_version":1,"schema_version":1`), 1)
		}, run.ErrInvalidArgument},
		{"original result whitespace exceeds limit", func(_ *agentv1.RunLease, c *agentv1.Checkpoint, _ *Selection) {
			c.Steps[0].ResultJson = append(bytes.Repeat([]byte(" "), 8192), c.Steps[0].ResultJson...)
		}, run.ErrCheckpointTooLarge},
		{"original ticket whitespace exceeds limit", func(_ *agentv1.RunLease, c *agentv1.Checkpoint, _ *Selection) {
			c.Snapshot.TicketBindingJson = append(bytes.Repeat([]byte(" "), 8192), c.Snapshot.TicketBindingJson...)
		}, run.ErrCheckpointTooLarge},
		{"valid hash but changed result", func(_ *agentv1.RunLease, c *agentv1.Checkpoint, _ *Selection) {
			c.Steps[0].ResultJson = bytes.ReplaceAll(c.Steps[0].ResultJson, []byte("synthetic"), []byte("modified"))
		}, run.ErrStepConflict},
		{"bad invocation", func(_ *agentv1.RunLease, _ *agentv1.Checkpoint, s *Selection) { s.ToolInvocationID = uuidAt(99) }, run.ErrInvalidArgument},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			lease, cp, selected := reverseProjection(t, base)
			test.edit(lease, cp, &selected)
			_, err := BuildExecute(lease, cp, selected, base.RequestID, 1000, 181000)
			if !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
		})
	}
}

func TestBuildExecuteDeepCopyAndDeadline(t *testing.T) {
	f := exampleFrame(t, "search_policy")
	lease, cp, selected := reverseProjection(t, f)
	// Claim and a later checkpoint can serialize the same immutable object in
	// different order/whitespace. Their canonical identity must remain equal.
	lease.Checkpoint.Snapshot.TicketBindingJson = append([]byte("  "), lease.Checkpoint.Snapshot.TicketBindingJson...)
	built, err := BuildExecute(lease, cp, selected, f.RequestID, 1000, 1100)
	if err != nil || built.RemainingMS != 100 {
		t.Fatalf("frame deadline: %v", err)
	}
	before, _ := runprotocol.Encode(built)
	cp.Snapshot.TicketBindingJson[0] = '['
	cp.Steps[0].ResultJson[0] = '['
	cp.NextStep.StepId = uuidAt(999)
	lease.Execution.TenantId = "other"
	after, _ := runprotocol.Encode(built)
	if !bytes.Equal(before, after) {
		t.Fatal("returned frame aliases RPC buffers")
	}
	for _, times := range [][2]int64{{-1, 100}, {1000, 1000}, {1001, 1000}, {0, 180001}, {run.MaxSafeInteger, run.MaxSafeInteger}, {0, run.MaxSafeInteger + 1}} {
		lease, cp, selected = reverseProjection(t, f)
		if _, err = BuildExecute(lease, cp, selected, f.RequestID, times[0], times[1]); !errors.Is(err, run.ErrInvalidArgument) {
			t.Fatalf("accepted invalid deadline %v: %v", times, err)
		}
	}
}

func TestDomainDecimalCanonicalizationAndFinalSize(t *testing.T) {
	f := exampleFrame(t, "model_proposal")
	lease, cp, selected := reverseProjection(t, f)
	search := cp.Steps[len(cp.Steps)-1]
	search.ResultJson = bytes.Replace(search.ResultJson, []byte(`"distance":0.0000001`), []byte(`"distance":1e-7`), 1)
	if _, err := BuildExecute(lease, cp, selected, f.RequestID, 1000, 2000); err != nil {
		t.Fatalf("domain-equivalent decimal rejected: %v", err)
	}
	// The entire final JSON object, including wrapper overhead, is bounded.
	oversized := f
	oversized.Checkpoint = json.RawMessage(`{"padding":"` + strings.Repeat("x", runprotocol.MaxCheckpointBytes) + `"}`)
	if err := ValidateExecute(oversized); !errors.Is(err, run.ErrCheckpointTooLarge) {
		t.Fatalf("oversized checkpoint: %v", err)
	}
	// Escaping can make JSON larger than the original bytes or protobuf size.
	oversized.Checkpoint = json.RawMessage(`{"padding":"` + strings.Repeat("<", 50000) + `"}`)
	if err := ValidateExecute(oversized); !errors.Is(err, run.ErrCheckpointTooLarge) {
		t.Fatalf("expanded checkpoint: %v", err)
	}
	lease, cp, selected = reverseProjection(t, f)
	changed := cp.Steps[0]
	var result map[string]any
	_ = json.Unmarshal(changed.ResultJson, &result)
	result["content"] = map[string]any{"text": strings.Repeat("<", 2000)}
	changed.ResultJson = marshal(t, result)
	if _, err := BuildExecute(lease, cp, selected, f.RequestID, 1000, 2000); !errors.Is(err, run.ErrCheckpointTooLarge) {
		t.Fatalf("expanded result: %v", err)
	}
}

func reverseProjection(t *testing.T, f runprotocol.Frame) (*agentv1.RunLease, *agentv1.Checkpoint, Selection) {
	t.Helper()
	var p checkpoint
	var i input
	if json.Unmarshal(f.Checkpoint, &p) != nil || json.Unmarshal(f.Input, &i) != nil {
		t.Fatal("invalid fixture projection")
	}
	c := &agentv1.Checkpoint{CursorVersion: p.CursorVersion, NextStep: wireStep(p.NextStep), Snapshot: &agentv1.SnapshotBinding{
		TenantId: p.Snapshot.TenantID, TicketId: p.Snapshot.TicketID, SnapshotId: p.Snapshot.SnapshotID, SnapshotHash: p.Snapshot.SnapshotHash,
		VersionVectorJson: bytes.Clone(p.Snapshot.VersionVectorJSON), TicketBindingJson: bytes.Clone(p.Snapshot.TicketBindingJSON),
		IndexId: p.Snapshot.IndexID, IndexProfileHash: p.Snapshot.IndexProfileHash}}
	for _, step := range p.Steps {
		c.Steps = append(c.Steps, &agentv1.AcceptedStep{Step: wireStep(step.Step), CommitHash: step.CommitHash,
			ResultJson: bytes.Clone(step.ResultJSON), ResultRef: step.ResultRef})
	}
	b := f.Binding
	l := &agentv1.RunLease{Execution: &agentv1.ExecutionIdentity{TenantId: b.TenantID, RunId: b.RunID,
		Session: &agentv1.SessionIdentity{WorkerId: b.WorkerID, SessionId: b.SessionID}, AttemptNo: b.AttemptNo, FencingToken: b.FencingToken}, TraceContext: f.TraceContext,
		Checkpoint: proto.Clone(c).(*agentv1.Checkpoint)}
	return l, c, Selection{ExecutorVersion: i.ExecutorVersion, AdapterID: i.AdapterID, ToolInvocationID: i.ToolInvocationID}
}

func wireStep(s identity) *agentv1.StepIdentity {
	var kind agentv1.StepKind
	for code, name := range stepNames {
		if name == s.Kind {
			kind = code
		}
	}
	return &agentv1.StepIdentity{StepId: s.StepID, Sequence: int32(s.Sequence), Kind: kind, CursorVersion: s.CursorVersion,
		InputHash: s.InputHash, ProfileId: s.ProfileID, ProfileHash: s.ProfileHash, SnapshotId: s.SnapshotID, SnapshotHash: s.SnapshotHash}
}

func exampleFrame(t *testing.T, kind string) runprotocol.Frame {
	t.Helper()
	ticket := json.RawMessage(`{"tenant_id":"runtime-test","ticket_id":"synthetic-ticket","revision":1,"observed_at":"2026-09-16T00:00:00Z","order_id":null,"policy_version":"synthetic-policy-v1","subject":"synthetic mechanism fixture","description":"Synthetic data verifies execution mechanics only.","status":"open"}`)
	profileHash, snapshotHash := strings.Repeat("a", 64), strings.Repeat("b", 64)
	snapshotID, indexID, runID := uuidAt(2), uuidAt(3), uuidAt(1)
	vector := marshal(t, map[string]any{"schema_version": 1, "ticket": map[string]any{"id": "synthetic-ticket", "revision": 1},
		"order": map[string]any{"id": nil, "exists": false, "revision": nil}, "delivery": map[string]any{"id": nil, "exists": false, "aggregate_revision": nil},
		"policy": map[string]any{"version": "synthetic-policy-v1", "revision": 1, "corpus_sha256": strings.Repeat("c", 64)},
		"index":  map[string]any{"id": indexID, "profile_hash": strings.Repeat("d", 64), "content_hash": strings.Repeat("e", 64)}})
	p := checkpoint{Steps: []accepted{}, Snapshot: snapshot{TenantID: "runtime-test", TicketID: "synthetic-ticket", SnapshotID: snapshotID,
		SnapshotHash: snapshotHash, VersionVectorJSON: vector, TicketBindingJSON: ticket, IndexID: indexID, IndexProfileHash: strings.Repeat("d", 64)}}
	previous := ""
	for index, stepKind := range []string{"read_ticket", "get_order", "get_delivery", "search_policy", "model_proposal", "submit_proposal"} {
		s := identity{StepID: uuidAt(10 + index), Sequence: int64(index + 1), Kind: stepKind, CursorVersion: int64(index),
			InputHash: run.NextInputHash(profileHash, snapshotHash, int64(index), previous), ProfileID: "bounded-readonly-mechanism-profile-v1", ProfileHash: profileHash, SnapshotID: snapshotID, SnapshotHash: snapshotHash}
		if stepKind == kind {
			p.NextStep, p.CursorVersion = s, int64(index)
			break
		}
		result := run.StepResult{SchemaVersion: 1, EvidenceRefs: []string{}, Content: json.RawMessage("null")}
		switch stepKind {
		case "read_ticket":
			result.Content, result.EvidenceRefs = ticket, []string{"business-evidence:" + snapshotID + ":ticket"}
		case "get_order", "get_delivery":
			factKind := strings.TrimPrefix(stepKind, "get_")
			ref := "business-evidence:" + snapshotID + ":" + factKind
			result.ToolInvocationID, result.PhysicalCallID, result.EvidenceRefs = uuidAt(30+index), uuidAt(40+index), []string{ref}
			result.Content = marshal(t, map[string]any{"snapshot_id": snapshotID, "evidence_ref": ref, "kind": factKind, "missing": true, "missing_reason": "no_association"})
		case "search_policy":
			ref := "business-policy:" + indexID + ":synthetic-chunk"
			result.ToolInvocationID, result.PhysicalCallID, result.EvidenceRefs = uuidAt(30+index), uuidAt(40+index), []string{ref}
			result.Content = marshal(t, map[string]any{"snapshot_id": snapshotID, "matches": []any{map[string]any{"index_id": indexID, "chunk_id": "synthetic-chunk", "policy_version": "synthetic-policy-v1", "evidence_ref": ref, "source": "synthetic", "text": "Synthetic policy fixture.", "distance": json.Number("1e-7")}}})
		case "model_proposal":
			result.PhysicalCallID = uuidAt(40 + index)
			result.Proposal = &run.Proposal{Decision: "no_action", Summary: "Synthetic mechanism outcome.", EvidenceRefs: []string{"business-evidence:" + snapshotID + ":ticket"}, Action: ""}
		}
		_, canonical, err := run.CanonicalStepResult(marshal(t, result), stepKind)
		if err != nil {
			t.Fatal(err)
		}
		previous = run.CommitHash(s.domain(), canonical)
		p.Steps = append(p.Steps, accepted{Step: s, CommitHash: previous, ResultJSON: canonical, ResultRef: run.StepReference(runID, s.Sequence)})
	}
	n := p.NextStep
	toolID := ""
	if len(run.ToolSequence(kind)) > 0 {
		toolID = uuidAt(100)
	}
	f := runprotocol.Frame{Version: 2, Kind: "execute_step", RequestID: uuidAt(4), EmittedMonoMS: 1000, RemainingMS: 60000,
		Binding: runprotocol.Binding{TenantID: "runtime-test", WorkerID: "runtime-test-worker", RunID: runID, StepID: n.StepID, SessionID: uuidAt(5),
			ProfileID: n.ProfileID, ProfileHash: n.ProfileHash, SnapshotID: n.SnapshotID, SnapshotHash: n.SnapshotHash, InputHash: n.InputHash,
			AttemptNo: 1, FencingToken: 1, CursorVersion: n.CursorVersion, StepSequence: n.Sequence, StepKind: n.Kind},
		Checkpoint: marshal(t, p), Input: marshal(t, input{SchemaVersion: 1, ExecutorVersion: ExecutorVersion, AdapterID: testAdapter, ToolInvocationID: toolID})}
	if err := ValidateExecute(f); err != nil {
		t.Fatal(err)
	}
	return f
}

func uuidAt(value int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", value) }

func marshal(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func compact(t *testing.T, raw []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
