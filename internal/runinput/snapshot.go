package runinput

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/xjfyrh/jobforge/internal/business"
	"github.com/xjfyrh/jobforge/internal/run"
	runprotocol "github.com/xjfyrh/jobforge/internal/runprotocol/v2"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

// The latest server cursor may advance, but GetCheckpoint cannot replace the
// resources captured by Claim. Compare canonical objects, never raw whitespace.
func validateClaimResources(claim *agentv1.Checkpoint, current checkpoint) error {
	s, now := claim.Snapshot, current.Snapshot
	if !validStep(projectStep(claim.NextStep)) || claim.NextStep.ProfileId != current.NextStep.ProfileID ||
		claim.NextStep.ProfileHash != current.NextStep.ProfileHash || claim.NextStep.SnapshotId != now.SnapshotID ||
		claim.NextStep.SnapshotHash != now.SnapshotHash || s.TenantId != now.TenantID || s.TicketId != now.TicketID ||
		s.SnapshotId != now.SnapshotID || s.SnapshotHash != now.SnapshotHash || s.IndexId != now.IndexID ||
		s.IndexProfileHash != now.IndexProfileHash {
		return run.ErrStepConflict
	}
	var ticket business.Ticket
	var vector business.VersionVector
	if decodeProjection(s.TicketBindingJson, &ticket) != nil || decodeProjection(s.VersionVectorJson, &vector) != nil {
		return run.ErrInvalidArgument
	}
	for _, pair := range [][2][]byte{{s.TicketBindingJson, now.TicketBindingJSON}, {s.VersionVectorJson, now.VersionVectorJSON}} {
		if err := run.ValidateStepJSON(pair[0], 8192); err != nil {
			return err
		}
		original, err := run.CanonicalCheckpointJSON(pair[0])
		if err != nil {
			return err
		}
		updated, err := run.CanonicalCheckpointJSON(pair[1])
		if err != nil {
			return err
		}
		if !bytes.Equal(original, updated) {
			return run.ErrStepConflict
		}
	}
	return nil
}

func validateSnapshot(s snapshot, b runprotocol.Binding) error {
	if !run.ValidIdentifier(s.TicketID) || s.TenantID != b.TenantID || s.SnapshotID != b.SnapshotID ||
		s.SnapshotHash != b.SnapshotHash || !run.ValidUUID(s.IndexID) || !run.ValidHash(s.IndexProfileHash) {
		return run.ErrStepConflict
	}
	var ticket business.Ticket
	var vector business.VersionVector
	for _, raw := range []json.RawMessage{s.TicketBindingJSON, s.VersionVectorJSON} {
		if err := run.ValidateStepJSON(raw, 8192); err != nil {
			return err
		}
	}
	if decodeProjection(s.TicketBindingJSON, &ticket) != nil || decodeProjection(s.VersionVectorJSON, &vector) != nil {
		return run.ErrInvalidArgument
	}
	if ticket.TenantID != s.TenantID || ticket.TicketID != s.TicketID || !revision(ticket.Revision) || ticket.ObservedAt.IsZero() ||
		!run.ValidIdentifier(ticket.PolicyVersion) || !run.ValidIdentifier(ticket.Status) ||
		!boundedText(ticket.Subject, 256) || !boundedText(ticket.Description, 1024) ||
		vector.SchemaVersion != 1 || vector.Ticket.ID != s.TicketID || vector.Ticket.Revision != ticket.Revision ||
		vector.Policy.Version != ticket.PolicyVersion || !revision(vector.Policy.Revision) || !run.ValidHash(vector.Policy.CorpusSHA256) ||
		vector.Index.ID != s.IndexID || vector.Index.ProfileHash != s.IndexProfileHash || !run.ValidHash(vector.Index.ContentHash) ||
		!equalOptionalID(vector.Order.ID, ticket.OrderID) || !validFact(vector.Order.ID, vector.Order.Exists, vector.Order.Revision) ||
		!validFact(vector.Delivery.ID, vector.Delivery.Exists, vector.Delivery.AggregateRevision) ||
		(!vector.Order.Exists && vector.Delivery.ID != nil) {
		return run.ErrStepConflict
	}
	return nil
}

func revision(value int64) bool { return value > 0 && value <= run.MaxSafeInteger }

func boundedText(value string, limit int) bool {
	return utf8.ValidString(value) && len(value) <= limit && !strings.ContainsRune(value, 0)
}

func equalOptionalID(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func validFact(id *string, exists bool, version *int64) bool {
	if id != nil && !run.ValidIdentifier(*id) {
		return false
	}
	if exists {
		return id != nil && version != nil && revision(*version)
	}
	return version == nil
}

// decodeProjection requires exact fields rather than encoding/json's aliases,
// omitted zero values and null scalar coercion. Domain JSON objects remain raw
// until their existing typed validator checks them; no fields are discarded.
func decodeProjection(data []byte, value any) error {
	if err := run.ValidateStepJSON(data, runprotocol.MaxCheckpointBytes); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil {
		return run.ErrInvalidArgument
	}
	expected, err := json.Marshal(value)
	if err != nil || !sameShape(data, expected) {
		return run.ErrInvalidArgument
	}
	return nil
}

func sameShape(actual, expected []byte) bool {
	actual, expected = bytes.TrimSpace(actual), bytes.TrimSpace(expected)
	if len(actual) == 0 || len(expected) == 0 || (actual[0] == 'n' && expected[0] != 'n') {
		return false
	}
	switch expected[0] {
	case '{':
		var a, e map[string]json.RawMessage
		if json.Unmarshal(actual, &a) != nil || a == nil || json.Unmarshal(expected, &e) != nil || len(a) != len(e) {
			return false
		}
		for key, child := range e {
			if !sameShape(a[key], child) {
				return false
			}
		}
	case '[':
		var a, e []json.RawMessage
		if json.Unmarshal(actual, &a) != nil || a == nil || json.Unmarshal(expected, &e) != nil || len(a) != len(e) {
			return false
		}
		for index := range e {
			if !sameShape(a[index], e[index]) {
				return false
			}
		}
	}
	return true
}
