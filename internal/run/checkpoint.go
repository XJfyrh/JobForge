package run

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// BoundedReadonlyStrategy names the single registered sequential graph.
	BoundedReadonlyStrategy = "bounded_readonly_v1"
	// MaxCheckpointBytes bounds all accepted protected outputs together.
	MaxCheckpointBytes = 256 * 1024
)

// CommitStepRequest carries output and its complete identity, never a next cursor.
type CommitStepRequest struct {
	Lease      Lease
	Step       StepIdentity
	ResultJSON json.RawMessage
	CommitHash string
}

// AcceptedStep is immutable evidence of a completed commit, not execution permission.
type AcceptedStep struct {
	Identity   StepIdentity
	CommitHash string
	ResultJSON json.RawMessage
	ResultRef  string
}

// CommitStepResponse returns only server-computed progress and lease disposition.
type CommitStepResponse struct {
	AcceptedStep  AcceptedStep
	CursorVersion int64
	State         State
	AttemptClosed bool
	NextStep      *StepIdentity
}

// AcceptedCommitResponse permits read-only confirmation after a lost final ACK.
type AcceptedCommitResponse struct {
	Found          bool
	AcceptedStep   *AcceptedStep
	AttemptOutcome string
	State          State
}

// Proposal describes one non-authorized business recommendation. The action
// vocabulary records conclusions or requests follow-up; it never executes writes.
type Proposal struct {
	Decision     string   `json:"decision"`
	Summary      string   `json:"summary"`
	EvidenceRefs []string `json:"evidence_refs"`
	Action       string   `json:"action"`
}

// StepResult is the registered protected envelope. Content contains the actual
// bounded business response; model and final steps carry only structured Proposal.
type StepResult struct {
	SchemaVersion      int             `json:"schema_version"`
	ToolInvocationID   string          `json:"tool_invocation_id"`
	PhysicalCallID     string          `json:"physical_call_id"`
	EvidenceRefs       []string        `json:"evidence_refs"`
	Content            json.RawMessage `json:"content"`
	Proposal           *Proposal       `json:"proposal"`
	CorrectionRequired bool            `json:"correction_required"`
}

// CommitDecision is computed from the registered graph and accepted evidence.
type CommitDecision struct {
	CanonicalJSON json.RawMessage
	Result        StepResult
	NextKind      string
	CloseAttempt  bool
	Proposal      *Proposal
}

// InitialStepInput binds the first registered step to immutable resources.
func InitialStepInput(profileHash, snapshotHash string) string {
	return NextInputHash(profileHash, snapshotHash, 0, "")
}

// NextInputHash binds each server cursor to the entire preceding commit chain.
func NextInputHash(profileHash, snapshotHash string, cursor int64, acceptedCommitHash string) string {
	return Fingerprint("jobforge.run.step-input.v1", profileHash, snapshotHash, strconv.FormatInt(cursor, 10), acceptedCommitHash)
}

// CommitHash binds every immutable step field and canonical protected content.
func CommitHash(step StepIdentity, canonicalJSON []byte) string {
	return Fingerprint("jobforge.run.commit.v1", step.ID, strconv.FormatInt(step.Sequence, 10), step.Kind,
		strconv.FormatInt(step.CursorVersion, 10), step.InputHash, step.ProfileID, step.ProfileHash,
		step.SnapshotID, step.SnapshotHash, string(canonicalJSON))
}

// CanonicalStepResult validates a complete envelope and normalizes JSON object
// order/whitespace without coercing numeric values or removing evidence.
func CanonicalStepResult(data []byte, kind string) (StepResult, json.RawMessage, error) {
	limit := 16384
	if kind == "read_ticket" || len(ToolSequence(kind)) != 0 {
		limit = 8192
	}
	var result StepResult
	if err := ValidateStepJSON(data, limit); err != nil {
		return result, nil, err
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || len(object) != 7 {
		return result, nil, ErrInvalidArgument
	}
	for _, key := range []string{"schema_version", "tool_invocation_id", "physical_call_id", "evidence_refs", "content", "proposal", "correction_required"} {
		value, ok := object[key]
		if !ok || (key != "proposal" && key != "content" && bytes.Equal(bytes.TrimSpace(value), []byte("null"))) {
			return result, nil, ErrInvalidArgument
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || result.SchemaVersion != 1 || len(result.EvidenceRefs) > 32 {
		return result, nil, ErrInvalidArgument
	}
	if !validStringArray(object["evidence_refs"]) {
		return result, nil, ErrInvalidArgument
	}
	if result.Proposal != nil {
		var proposal map[string]json.RawMessage
		if json.Unmarshal(object["proposal"], &proposal) != nil || len(proposal) != 4 {
			return result, nil, ErrInvalidArgument
		}
		for _, key := range []string{"decision", "summary", "evidence_refs", "action"} {
			value, ok := proposal[key]
			if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return result, nil, ErrInvalidArgument
			}
		}
		if !validStringArray(proposal["evidence_refs"]) {
			return result, nil, ErrInvalidArgument
		}
	}
	if result.ToolInvocationID != "" && !ValidUUID(result.ToolInvocationID) ||
		result.PhysicalCallID != "" && !ValidUUID(result.PhysicalCallID) {
		return result, nil, ErrInvalidArgument
	}
	encoded, err := CanonicalCheckpointJSON(data)
	if err != nil {
		return result, nil, err
	}
	if len(encoded) > limit {
		return result, nil, ErrCheckpointTooLarge
	}
	return result, encoded, nil
}

func validStringArray(raw []byte) bool {
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil || values == nil {
		return false
	}
	for _, value := range values {
		var text string
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &text) != nil {
			return false
		}
	}
	return true
}

// CanonicalCheckpointJSON gives PostgreSQL JSONB and incoming JSON the same
// byte identity, including exponent/decimal numbers without float rounding.
func CanonicalCheckpointJSON(data []byte) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil, ErrInvalidArgument
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, ErrInvalidArgument
	}
	normalized, err := normalizeCheckpointNumbers(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}

func normalizeCheckpointNumbers(value any) (any, error) {
	switch typed := value.(type) {
	case json.Number:
		text := string(typed)
		number, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsInf(number, 0) || math.IsNaN(number) {
			return nil, ErrInvalidArgument
		}
		negative := strings.HasPrefix(text, "-")
		text = strings.TrimPrefix(text, "-")
		parts := strings.FieldsFunc(text, func(r rune) bool { return r == 'e' || r == 'E' })
		exponent := 0
		if len(parts) == 2 {
			exponent, err = strconv.Atoi(parts[1])
			if err != nil || exponent < -308 || exponent > 308 {
				return nil, ErrInvalidArgument
			}
		}
		text = parts[0]
		if dot := strings.IndexByte(text, '.'); dot >= 0 {
			exponent -= len(text) - dot - 1
			text = text[:dot] + text[dot+1:]
		}
		text = strings.TrimLeft(text, "0")
		if text == "" {
			return json.Number("0"), nil
		}
		if exponent >= 0 {
			text += strings.Repeat("0", exponent)
		} else {
			point := len(text) + exponent
			if point <= 0 {
				text = "0." + strings.Repeat("0", -point) + text
			} else {
				text = text[:point] + "." + text[point:]
			}
			text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
		}
		if negative {
			text = "-" + text
		}
		return json.Number(text), nil
	case []any:
		for i, child := range typed {
			normalized, err := normalizeCheckpointNumbers(child)
			if err != nil {
				return nil, err
			}
			typed[i] = normalized
		}
	case map[string]any:
		for key, child := range typed {
			normalized, err := normalizeCheckpointNumbers(child)
			if err != nil {
				return nil, err
			}
			typed[key] = normalized
		}
	}
	return value, nil
}

// DecideCommit validates evidence from this Run before choosing its next step.
// Physical observation binding is checked by the transactional store separately.
func DecideCommit(profile Profile, snapshot SnapshotBinding, prior []Step, req CommitStepRequest) (CommitDecision, error) {
	var decision CommitDecision
	if profile.Strategy != BoundedReadonlyStrategy || profile.ID != req.Step.ProfileID || profile.Hash != req.Step.ProfileHash {
		return decision, ErrProfileUnavailable
	}
	result, canonical, err := CanonicalStepResult(req.ResultJSON, req.Step.Kind)
	if err != nil {
		return decision, err
	}
	if req.CommitHash != CommitHash(req.Step, canonical) {
		return decision, ErrStepConflict
	}
	decision.Result, decision.CanonicalJSON = result, canonical
	allowed := map[string]bool{"business-evidence:" + snapshot.ID + ":ticket": true}
	for _, step := range prior {
		previous, _, err := CanonicalStepResult(step.Output, step.Kind)
		if err != nil {
			return decision, ErrInternal
		}
		if len(ToolSequence(step.Kind)) != 0 || step.Kind == "read_ticket" {
			for _, ref := range previous.EvidenceRefs {
				allowed[ref] = true
			}
		}
	}
	if req.Step.Kind == "read_ticket" {
		if result.PhysicalCallID != "" || result.ToolInvocationID != "" || result.Proposal != nil || result.CorrectionRequired ||
			!slices.Equal(result.EvidenceRefs, []string{"business-evidence:" + snapshot.ID + ":ticket"}) ||
			!sameJSON(result.Content, snapshot.Ticket) {
			return decision, ErrStepConflict
		}
		decision.NextKind = "get_order"
		return decision, nil
	}
	if len(ToolSequence(req.Step.Kind)) != 0 {
		if result.PhysicalCallID == "" || result.ToolInvocationID == "" || result.Proposal != nil || result.CorrectionRequired ||
			!toolEvidence(snapshot, req.Step.Kind, result) {
			return decision, ErrStepConflict
		}
		decision.NextKind = map[string]string{"get_order": "get_delivery", "get_delivery": "search_policy", "search_policy": "model_proposal"}[req.Step.Kind]
		return decision, nil
	}
	if req.Step.Kind != "model_proposal" && req.Step.Kind != "protocol_correction" && req.Step.Kind != "submit_proposal" {
		return decision, ErrInvalidArgument
	}
	if result.ToolInvocationID != "" || !bytes.Equal(bytes.TrimSpace(result.Content), []byte("null")) || len(result.EvidenceRefs) != 0 {
		return decision, ErrStepConflict
	}
	if req.Step.Kind == "submit_proposal" {
		if result.PhysicalCallID != "" || result.CorrectionRequired || len(prior) == 0 {
			return decision, ErrStepConflict
		}
		last := prior[len(prior)-1]
		previous, _, err := CanonicalStepResult(last.Output, last.Kind)
		if err != nil || (last.Kind != "model_proposal" && last.Kind != "protocol_correction") || previous.Proposal == nil ||
			!sameProposal(previous.Proposal, result.Proposal) {
			return decision, ErrStepConflict
		}
		decision.CloseAttempt = true
	} else if result.PhysicalCallID == "" {
		return decision, ErrStepConflict
	}
	if result.CorrectionRequired {
		if req.Step.Kind != "model_proposal" || result.Proposal != nil {
			return decision, ErrModelProtocol
		}
		decision.NextKind = "protocol_correction"
		return decision, nil
	}
	if !validProposal(result.Proposal, allowed) {
		return decision, ErrModelProtocol
	}
	decision.Proposal = result.Proposal
	if !decision.CloseAttempt {
		decision.NextKind = "submit_proposal"
	}
	return decision, nil
}

// ApplyCommit advances the validated cursor, preserving the authority until
// the storage layer atomically closes attempt resources for a final commit.
func ApplyCommit(r *Run, a *Authority, req CommitStepRequest, decision CommitDecision, nextStepID string, now time.Time) error {
	if err := CheckExecution(*r, *a, req.Lease, now); err != nil {
		return err
	}
	if err := CheckStep(*r, *a, req.Step); err != nil {
		return err
	}
	if a.ActiveCallID != nil {
		return ErrCallConflict
	}
	if r.CursorVersion >= 32 || a.CheckpointBytes < 0 || a.CheckpointBytes > MaxCheckpointBytes ||
		int64(len(decision.CanonicalJSON)) > MaxCheckpointBytes-a.CheckpointBytes {
		return ErrCheckpointTooLarge
	}
	if !decision.CloseAttempt && (!validStepKind(decision.NextKind) || !ValidUUID(nextStepID)) {
		return ErrInternal
	}
	if decision.CloseAttempt && decision.Proposal == nil {
		return ErrInternal
	}
	r.CursorVersion++
	a.CheckpointBytes += int64(len(decision.CanonicalJSON))
	r.UpdatedAt = now
	if decision.CloseAttempt {
		if decision.Proposal.Decision == "proposal" {
			return YieldForApproval(r, now)
		}
		r.State = Succeeded
		outcome := "no_action"
		r.Outcome = &outcome
		return nil
	}
	a.NextStepID, a.NextStepKind = nextStepID, decision.NextKind
	a.NextInputHash = NextInputHash(r.ProfileHash, r.SnapshotHash, r.CursorVersion, req.CommitHash)
	return nil
}

// YieldForApproval sets only the waiting state and bounded permission; storage
// must commit the proposal/bindings and close the attempt in this transaction.
func YieldForApproval(r *Run, now time.Time) error {
	if r.State != Running || !r.RunDeadline.After(now) {
		return ErrInvalidTransition
	}
	expires := now.Add(time.Hour)
	if r.RunDeadline.Before(expires) {
		expires = r.RunDeadline
	}
	ref := "run-proposal:" + r.ID
	r.State, r.PermissionExpiresAt, r.ProposalRef, r.UpdatedAt = AwaitingApproval, &expires, &ref, now
	return nil
}

func toolEvidence(snapshot SnapshotBinding, kind string, result StepResult) bool {
	var content struct {
		SnapshotID  string `json:"snapshot_id"`
		EvidenceRef string `json:"evidence_ref"`
		Kind        string `json:"kind"`
		Matches     []struct {
			IndexID     string `json:"index_id"`
			EvidenceRef string `json:"evidence_ref"`
			ChunkID     string `json:"chunk_id"`
		} `json:"matches"`
	}
	if json.Unmarshal(result.Content, &content) != nil || content.SnapshotID != snapshot.ID {
		return false
	}
	if kind != "search_policy" {
		evidenceKind := strings.TrimPrefix(kind, "get_")
		ref := "business-evidence:" + snapshot.ID + ":" + evidenceKind
		return content.Kind == evidenceKind && content.EvidenceRef == ref && slices.Equal(result.EvidenceRefs, []string{ref})
	}
	if len(content.Matches) > 3 || len(content.Matches) != len(result.EvidenceRefs) {
		return false
	}
	for i, match := range content.Matches {
		ref := "business-policy:" + snapshot.IndexID + ":" + match.ChunkID
		if match.IndexID != snapshot.IndexID || !ValidIdentifier(match.ChunkID) || match.EvidenceRef != ref || result.EvidenceRefs[i] != ref {
			return false
		}
	}
	return true
}

func validProposal(p *Proposal, allowed map[string]bool) bool {
	if p == nil || p.Summary == "" || len(p.Summary) > 8192 || len(p.EvidenceRefs) == 0 || len(p.EvidenceRefs) > 32 {
		return false
	}
	if (p.Decision == "no_action" && p.Action != "") || (p.Decision != "no_action" && p.Decision != "proposal") ||
		(p.Decision == "proposal" && p.Action != "record_conclusion" && p.Action != "request_information" && p.Action != "escalate") {
		return false
	}
	seen := map[string]bool{}
	for _, ref := range p.EvidenceRefs {
		if !allowed[ref] || seen[ref] {
			return false
		}
		seen[ref] = true
	}
	return true
}

func sameProposal(a, b *Proposal) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}
func sameJSON(a, b []byte) bool {
	var left, right any
	decode := func(data []byte, value *any) error {
		d := json.NewDecoder(bytes.NewReader(data))
		d.UseNumber()
		return d.Decode(value)
	}
	if decode(a, &left) != nil || decode(b, &right) != nil {
		return false
	}
	x, _ := json.Marshal(left)
	y, _ := json.Marshal(right)
	return bytes.Equal(x, y)
}

// ValidateStepJSON rejects duplicate fields recursively, invalid UTF-8, trailing
// data and excessive nesting without logging any rejected protected content.
func ValidateStepJSON(data []byte, maxBytes int) error {
	if len(data) > maxBytes {
		return ErrCheckpointTooLarge
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || !utf8.Valid(data) || trimmed[0] != '{' || !validCheckpointEscapes(data) {
		return ErrInvalidArgument
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if scanStepJSON(d, 0) != nil {
		return ErrInvalidArgument
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrInvalidArgument
	}
	return nil
}

// Go otherwise replaces unpaired surrogate escapes, changing protected content.
func validCheckpointEscapes(data []byte) bool {
	inString := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) {
			return false
		}
		if data[i] != 'u' {
			continue
		}
		if i+4 >= len(data) {
			return false
		}
		unit, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		if err != nil || unit == 0 || unit >= 0xdc00 && unit <= 0xdfff {
			return false
		}
		i += 4
		if unit < 0xd800 || unit > 0xdbff {
			continue
		}
		if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
			return false
		}
		low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return false
		}
		i += 6
	}
	return !inString
}

func scanStepJSON(d *json.Decoder, depth int) error {
	if depth > 32 {
		return ErrInvalidArgument
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delimiter != '{' && delimiter != '[' {
		return ErrInvalidArgument
	}
	seen := map[string]bool{}
	for d.More() {
		if delimiter == '{' {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return ErrInvalidArgument
			}
			seen[name] = true
		}
		if err := scanStepJSON(d, depth+1); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

// StepReference constructs a protected tenant-authorized checkpoint reference.
func StepReference(runID string, sequence int64) string {
	return fmt.Sprintf("run-step:%s:%d", runID, sequence)
}
