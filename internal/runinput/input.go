// Package runinput projects trusted Agent RPC checkpoints into the registered
// runtime input. It owns no clock, RPC, deployment registry or execution right.
package runinput

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/xjfyrh/jobforge/internal/run"
	runprotocol "github.com/xjfyrh/jobforge/internal/runprotocol/v2"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

// ExecutorVersion rejects deployments predating the observation ACK contract.
const ExecutorVersion = "linux-v2-ack-runtime-1"

// Selection comes from the trusted deployment manifest and confirmed BeginTool.
// Shape validation does not register an adapter or authorize a tool invocation.
type Selection struct {
	ExecutorVersion  string
	AdapterID        string
	ToolInvocationID string
}

type input struct {
	SchemaVersion    int    `json:"schema_version"`
	ExecutorVersion  string `json:"executor_version"`
	AdapterID        string `json:"adapter_id"`
	ToolInvocationID string `json:"tool_invocation_id"`
}

type identity struct {
	StepID        string `json:"step_id"`
	Sequence      int64  `json:"sequence"`
	Kind          string `json:"kind"`
	CursorVersion int64  `json:"cursor_version"`
	InputHash     string `json:"input_hash"`
	ProfileID     string `json:"profile_id"`
	ProfileHash   string `json:"profile_hash"`
	SnapshotID    string `json:"snapshot_id"`
	SnapshotHash  string `json:"snapshot_hash"`
}

type accepted struct {
	Step       identity        `json:"step"`
	CommitHash string          `json:"commit_hash"`
	ResultJSON json.RawMessage `json:"result_json"`
	ResultRef  string          `json:"result_ref"`
}

type snapshot struct {
	TenantID          string          `json:"tenant_id"`
	TicketID          string          `json:"ticket_id"`
	SnapshotID        string          `json:"snapshot_id"`
	SnapshotHash      string          `json:"snapshot_hash"`
	VersionVectorJSON json.RawMessage `json:"version_vector_json"`
	TicketBindingJSON json.RawMessage `json:"ticket_binding_json"`
	IndexID           string          `json:"index_id"`
	IndexProfileHash  string          `json:"index_profile_hash"`
}

type checkpoint struct {
	CursorVersion int64      `json:"cursor_version"`
	NextStep      identity   `json:"next_step"`
	Steps         []accepted `json:"steps"`
	Snapshot      snapshot   `json:"snapshot"`
}

var stepNames = map[agentv1.StepKind]string{
	agentv1.StepKind_STEP_KIND_READ_TICKET:         "read_ticket",
	agentv1.StepKind_STEP_KIND_GET_ORDER:           "get_order",
	agentv1.StepKind_STEP_KIND_GET_DELIVERY:        "get_delivery",
	agentv1.StepKind_STEP_KIND_SEARCH_POLICY:       "search_policy",
	agentv1.StepKind_STEP_KIND_MODEL_PROPOSAL:      "model_proposal",
	agentv1.StepKind_STEP_KIND_PROTOCOL_CORRECTION: "protocol_correction",
	agentv1.StepKind_STEP_KIND_SUBMIT_PROPOSAL:     "submit_proposal",
}

// BuildExecute returns an independent frame, preserving the current RPC cursor.
// The caller owns clock samples, manifest selection and BeginTool confirmation.
func BuildExecute(lease *agentv1.RunLease, value *agentv1.Checkpoint, selection Selection,
	requestID string, emittedMonoMS, stepDeadlineMonoMS int64,
) (runprotocol.Frame, error) {
	if lease == nil || lease.Execution == nil || lease.Execution.Session == nil || lease.Checkpoint == nil ||
		lease.Checkpoint.NextStep == nil || lease.Checkpoint.Snapshot == nil || value == nil ||
		value.NextStep == nil || value.Snapshot == nil || len(value.Steps) > 31 ||
		emittedMonoMS < 0 || stepDeadlineMonoMS > runprotocol.MaxInteger || stepDeadlineMonoMS <= emittedMonoMS ||
		stepDeadlineMonoMS-emittedMonoMS > 180000 {
		return runprotocol.Frame{}, run.ErrInvalidArgument
	}
	p := checkpoint{CursorVersion: value.CursorVersion, NextStep: projectStep(value.NextStep), Steps: make([]accepted, 0, len(value.Steps)),
		Snapshot: snapshot{TenantID: value.Snapshot.TenantId, TicketID: value.Snapshot.TicketId,
			SnapshotID: value.Snapshot.SnapshotId, SnapshotHash: value.Snapshot.SnapshotHash,
			VersionVectorJSON: bytes.Clone(value.Snapshot.VersionVectorJson), TicketBindingJSON: bytes.Clone(value.Snapshot.TicketBindingJson),
			IndexID: value.Snapshot.IndexId, IndexProfileHash: value.Snapshot.IndexProfileHash}}
	for _, step := range value.Steps {
		if step == nil || step.Step == nil {
			return runprotocol.Frame{}, run.ErrInvalidArgument
		}
		p.Steps = append(p.Steps, accepted{Step: projectStep(step.Step), CommitHash: step.CommitHash,
			ResultJSON: bytes.Clone(step.ResultJson), ResultRef: step.ResultRef})
	}
	e, next := lease.Execution, p.NextStep
	f := runprotocol.Frame{Version: 2, Kind: "execute_step", RequestID: requestID, EmittedMonoMS: emittedMonoMS,
		RemainingMS: stepDeadlineMonoMS - emittedMonoMS, TraceContext: lease.TraceContext,
		Binding: runprotocol.Binding{TenantID: e.TenantId, WorkerID: e.Session.WorkerId, RunID: e.RunId, SessionID: e.Session.SessionId,
			AttemptNo: e.AttemptNo, FencingToken: e.FencingToken, StepID: next.StepID, StepSequence: next.Sequence,
			StepKind: next.Kind, CursorVersion: next.CursorVersion, InputHash: next.InputHash, ProfileID: next.ProfileID,
			ProfileHash: next.ProfileHash, SnapshotID: next.SnapshotID, SnapshotHash: next.SnapshotHash}}
	i := input{SchemaVersion: 1, ExecutorVersion: selection.ExecutorVersion, AdapterID: selection.AdapterID, ToolInvocationID: selection.ToolInvocationID}
	// Validate original RPC byte lengths before normalization can shrink whitespace.
	if err := validateProjection(p, i, f.Binding); err != nil {
		return runprotocol.Frame{}, err
	}
	if err := validateClaimResources(lease.Checkpoint, p); err != nil {
		return runprotocol.Frame{}, err
	}
	var err error
	f.Input, err = json.Marshal(i)
	if err != nil {
		return runprotocol.Frame{}, run.ErrInvalidArgument
	}
	f.Checkpoint, err = json.Marshal(p)
	if err != nil {
		return runprotocol.Frame{}, run.ErrInvalidArgument
	}
	if err = ValidateExecute(f); err != nil {
		return runprotocol.Frame{}, err
	}
	return f, nil
}

// ValidateExecute applies the runtime contract in addition to the generic v2
// codec. A valid shape alone grants no adapter, clock or execution authority.
func ValidateExecute(frame runprotocol.Frame) error {
	if frame.Kind != "execute_step" {
		return run.ErrInvalidArgument
	}
	if len(frame.Checkpoint) > runprotocol.MaxCheckpointBytes || len(frame.Input) > 16384 {
		return run.ErrCheckpointTooLarge
	}
	checkpointJSON, marshalErr := json.Marshal(frame.Checkpoint)
	if marshalErr == nil && len(checkpointJSON) > runprotocol.MaxCheckpointBytes {
		return run.ErrCheckpointTooLarge
	}
	var original checkpoint
	var originalInput input
	if err := decodeProjection(frame.Checkpoint, &original); err != nil {
		return err
	}
	if err := decodeProjection(frame.Input, &originalInput); err != nil {
		return err
	}
	if err := validateProjection(original, originalInput, frame.Binding); err != nil {
		return err
	}
	line, err := runprotocol.Encode(frame)
	if errors.Is(err, runprotocol.ErrFrameLimit) {
		return run.ErrCheckpointTooLarge
	}
	if err != nil {
		return run.ErrInvalidArgument
	}
	// Encode may expand HTML-sensitive characters in RawMessage values. Check
	// the actual final representation, not protobuf size or pre-encoding bytes.
	encoded, err := runprotocol.Decode(line)
	if err != nil {
		return run.ErrInvalidArgument
	}
	var p checkpoint
	var i input
	if err = decodeProjection(encoded.Checkpoint, &p); err != nil {
		return err
	}
	if err = decodeProjection(encoded.Input, &i); err != nil {
		return err
	}
	return validateProjection(p, i, frame.Binding)
}

func projectStep(s *agentv1.StepIdentity) identity {
	return identity{StepID: s.StepId, Sequence: int64(s.Sequence), Kind: stepNames[s.Kind], CursorVersion: s.CursorVersion,
		InputHash: s.InputHash, ProfileID: s.ProfileId, ProfileHash: s.ProfileHash, SnapshotID: s.SnapshotId, SnapshotHash: s.SnapshotHash}
}

func (s identity) domain() run.StepIdentity {
	return run.StepIdentity{ID: s.StepID, Sequence: s.Sequence, Kind: s.Kind, CursorVersion: s.CursorVersion,
		InputHash: s.InputHash, ProfileID: s.ProfileID, ProfileHash: s.ProfileHash, SnapshotID: s.SnapshotID, SnapshotHash: s.SnapshotHash}
}

func validStep(s identity) bool {
	known := false
	for _, kind := range stepNames {
		known = known || s.Kind == kind
	}
	return known && run.ValidUUID(s.StepID) && s.Sequence >= 1 && s.Sequence <= 32 && s.CursorVersion == s.Sequence-1 &&
		run.ValidHash(s.InputHash) && run.ValidIdentifier(s.ProfileID) && run.ValidHash(s.ProfileHash) &&
		run.ValidUUID(s.SnapshotID) && run.ValidHash(s.SnapshotHash)
}

func validateProjection(p checkpoint, i input, b runprotocol.Binding) error {
	if i.SchemaVersion != 1 || i.ExecutorVersion != ExecutorVersion || !run.ValidIdentifier(i.AdapterID) ||
		p.CursorVersion < 0 || p.CursorVersion > 31 || p.Steps == nil || int64(len(p.Steps)) != p.CursorVersion || !validStep(p.NextStep) {
		return run.ErrInvalidArgument
	}
	n := p.NextStep
	if n.Sequence != p.CursorVersion+1 || n.StepID != b.StepID || n.Sequence != b.StepSequence || n.Kind != b.StepKind ||
		n.CursorVersion != b.CursorVersion || n.InputHash != b.InputHash || n.ProfileID != b.ProfileID || n.ProfileHash != b.ProfileHash ||
		n.SnapshotID != b.SnapshotID || n.SnapshotHash != b.SnapshotHash {
		return run.ErrStepConflict
	}
	tool := len(run.ToolSequence(n.Kind)) != 0
	if tool && !run.ValidUUID(i.ToolInvocationID) || !tool && i.ToolInvocationID != "" {
		return run.ErrInvalidArgument
	}
	if err := validateSnapshot(p.Snapshot, b); err != nil {
		return err
	}
	previousHash := ""
	seen := map[string]bool{n.StepID: true}
	for index, step := range p.Steps {
		s := step.Step
		if !validStep(s) || s.Sequence != int64(index)+1 || seen[s.StepID] || s.ProfileID != n.ProfileID ||
			s.ProfileHash != n.ProfileHash || s.SnapshotID != n.SnapshotID || s.SnapshotHash != n.SnapshotHash ||
			s.InputHash != run.NextInputHash(n.ProfileHash, n.SnapshotHash, s.CursorVersion, previousHash) ||
			!run.ValidHash(step.CommitHash) || step.ResultRef != run.StepReference(b.RunID, s.Sequence) {
			return run.ErrStepConflict
		}
		_, canonical, err := run.CanonicalRegisteredStepResult(step.ResultJSON, s.Kind)
		if err != nil {
			return err
		}
		if step.CommitHash != run.CommitHash(s.domain(), canonical) {
			return run.ErrStepConflict
		}
		previousHash, seen[s.StepID] = step.CommitHash, true
	}
	if n.InputHash != run.NextInputHash(n.ProfileHash, n.SnapshotHash, p.CursorVersion, previousHash) {
		return run.ErrStepConflict
	}
	return nil
}
