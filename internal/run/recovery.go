package run

import (
	"encoding/json"

	"github.com/xjfyrh/jobforge/internal/jsonstrict"
)

const (
	// SupportRecoveryExecutorVersion selects the confirmed-step recovery runtime.
	SupportRecoveryExecutorVersion = "linux-v2-recovery-runtime-1"
	// ConfirmedUncommittedRecovery binds the S3 policy to a new immutable profile.
	ConfirmedUncommittedRecovery = "confirmed_uncommitted_v1"
)

// ConfirmedStepRecovery requires the original complete profile identity. Neither
// a current request nor a newer deployment can upgrade an older call's policy.
func (p Profile) ConfirmedStepRecovery() bool {
	if (p.ExecutorVersion != SupportRecoveryExecutorVersion && p.ExecutorVersion != SupportApprovalExecutorVersion) || p.Strategy != SupportAgentStrategy || !p.AuditEnabled() {
		return false
	}
	d, err := DecodeSupportDefinition(p.Definition)
	if err != nil || (d.SchemaVersion != 3 && d.SchemaVersion != 4 && d.SchemaVersion != 6) || d.Program.RecoveryPolicy != ConfirmedUncommittedRecovery {
		return false
	}
	hash, err := SupportProfileHash(p)
	return err == nil && hash == p.Hash
}

// PendingStep captures the current immutable logical step before authority is
// cleared. It is not an execution grant or a substitute for a committed step.
func PendingStep(r Run, a Authority) StepIdentity {
	return StepIdentity{ID: a.NextStepID, Sequence: r.CursorVersion + 1, Kind: a.NextStepKind,
		CursorVersion: r.CursorVersion, InputHash: a.NextInputHash, ProfileID: r.ProfileID,
		ProfileHash: r.ProfileHash, SnapshotID: r.SnapshotID, SnapshotHash: r.SnapshotHash}
}

// DecodeRecoveryStep rejects missing, aliased, duplicate and unknown fields in
// persisted recovery evidence. It never infers evidence from the current cursor.
func DecodeRecoveryStep(raw []byte) (StepIdentity, error) {
	var step StepIdentity
	if len(raw) > 2048 || jsonstrict.Decode(raw, &step) != nil {
		return step, ErrInvalidArgument
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 9 {
		return step, ErrInvalidArgument
	}
	for _, key := range []string{"step_id", "sequence", "kind", "cursor_version", "input_hash", "profile_id", "profile_hash", "snapshot_id", "snapshot_hash"} {
		value, ok := fields[key]
		if !ok || string(value) == "null" {
			return step, ErrInvalidArgument
		}
	}
	r := Run{CursorVersion: step.CursorVersion, ProfileID: step.ProfileID, ProfileHash: step.ProfileHash,
		SnapshotID: step.SnapshotID, SnapshotHash: step.SnapshotHash}
	a := Authority{NextStepID: step.ID, NextStepKind: step.Kind, NextInputHash: step.InputHash}
	if CheckStep(r, a, step) != nil {
		return step, ErrInvalidArgument
	}
	return step, nil
}

// RecoveryClosure is the closed set of existing retry outcomes and codes that
// may carry an S3 proof. Terminal failures and cancellation never carry one.
func RecoveryClosure(outcome, code string) bool {
	if outcome != "failed_retry" && outcome != "lease_expired_retry" {
		return false
	}
	switch code {
	case "LEASE_EXPIRED", "TIMEOUT", "DEPENDENCY_UNAVAILABLE", "ATTEMPT_DEADLINE_EXCEEDED":
		return true
	default:
		return false
	}
}
