package runprotocol

import "github.com/xjfyrh/jobforge/internal/run"

// ReportBinding derives only immutable identity. It grants no execution rights.
func ReportBinding(f Frame, expectedModel string) (run.ReportBinding, error) {
	b := f.Binding
	hash, err := run.ExecutionBindingHash(run.Lease{TenantID: b.TenantID, RunID: b.RunID,
		WorkerID: b.WorkerID, SessionID: b.SessionID, AttemptNo: b.AttemptNo, FencingToken: b.FencingToken},
		run.StepIdentity{ID: b.StepID, Sequence: b.StepSequence, Kind: b.StepKind, CursorVersion: b.CursorVersion,
			InputHash: b.InputHash, ProfileID: b.ProfileID, ProfileHash: b.ProfileHash, SnapshotID: b.SnapshotID, SnapshotHash: b.SnapshotHash})
	subcall := run.SubcallQueryEmbedding
	if chatStep(b.StepKind) {
		subcall = run.SubcallChat
	}
	return run.ReportBinding{ExecutionBindingHash: hash, PhysicalCallID: f.PhysicalCallID,
		ParameterHash: f.ParameterHash, Subcall: subcall, ExpectedResponseModel: expectedModel}, err
}

// Report returns the complete immutable content to persist through SettleUsage.
func Report(f Frame) run.CallReport {
	r := run.CallReport{ProviderAudit: f.ProviderAudit}
	if f.Usage != nil {
		u := f.Usage
		r.Usage = &run.UsageReport{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
			CachedInputTokens: u.CachedInputTokens, ReceiptHash: u.ReceiptHash, UsageHash: u.UsageHash}
	}
	return r
}

func validReport(f Frame) bool {
	if !chatStep(f.Binding.StepKind) && f.Binding.StepKind != "search_policy" {
		return false
	}
	// The only installed provider adapter has this fixed expected model. The
	// coordinator additionally checks the immutable profile's identical value.
	binding, err := ReportBinding(f, "deepseek-flash")
	return err == nil && Report(f).Verify(binding, f.ReportHash) == nil
}

func chatStep(kind string) bool { return oneOf(kind, "model_proposal", "protocol_correction") }

func validAuditHash(f Frame) bool {
	if chatStep(f.Binding.StepKind) {
		return f.AuditHash != nil && hashPattern.MatchString(*f.AuditHash)
	}
	return f.AuditHash == nil
}

func priceable(r *run.CallReport) bool {
	return r.Usage != nil && (r.ProviderAudit == nil || r.ProviderAudit.IdentityState == run.ProviderIdentityCompatible)
}

func reportAllowsContinuation(r *run.CallReport) bool {
	return priceable(r) && (r.ProviderAudit == nil || r.ProviderAudit.ModeState == run.ProviderModeNonthinking)
}
