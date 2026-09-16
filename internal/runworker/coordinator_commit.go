package runworker

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/xjfyrh/jobforge/internal/run"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

func (c *coordinator) commit(ctx context.Context) stepOutcome {
	f := c.result
	parsed, canonical, err := run.CanonicalStepResult(f.Result, f.Binding.StepKind)
	if err != nil {
		if errors.Is(err, run.ErrCheckpointTooLarge) {
			return stepOutcome{Failure: "CHECKPOINT_TOO_LARGE"}
		}
		return stepOutcome{Failure: "EXECUTOR_PROTOCOL_ERROR"}
	}
	var last *callRecord
	for _, call := range c.calls {
		if !call.confirmed || call.observation == nil || (call.observation.UsageDisposition == "reported" && !call.settled) {
			return stepOutcome{Failure: "EXECUTOR_PROTOCOL_ERROR"}
		}
		if last == nil || call.intent.CallSequence > last.intent.CallSequence {
			last = call
		}
	}
	if last == nil {
		if parsed.PhysicalCallID != "" || parsed.ToolInvocationID != "" {
			return stepOutcome{Failure: "EXECUTOR_PROTOCOL_ERROR"}
		}
	} else if parsed.PhysicalCallID != last.id || parsed.ToolInvocationID != last.intent.ToolInvocationID {
		return stepOutcome{Failure: "EXECUTOR_PROTOCOL_ERROR"}
	}
	if f.Outcome == "error" {
		// Only a complete, acknowledged first-model validation failure creates
		// the existing one-correction marker. No other local failure is a step.
		if f.Binding.StepKind != "model_proposal" || f.ErrorCode != "OUTPUT_INVALID" || last == nil ||
			last.observation.TransportOutcome != "response" || last.observation.BusinessOutcome != "rejected" ||
			last.observation.ErrorCode != "OUTPUT_INVALID" || !parsed.CorrectionRequired || parsed.Proposal != nil ||
			len(parsed.EvidenceRefs) != 0 || !bytes.Equal(bytes.TrimSpace(parsed.Content), []byte("null")) {
			return stepOutcome{Failure: "EXECUTOR_PROTOCOL_ERROR"}
		}
	} else if parsed.CorrectionRequired {
		return stepOutcome{Failure: "EXECUTOR_PROTOCOL_ERROR"}
	}
	if c.stopped || c.authority.Check() != nil || ctx.Err() != nil {
		return stepOutcome{Abandoned: true}
	}
	s := c.checkpoint.NextStep
	hash := run.CommitHash(domainStep(s, f.Binding.StepKind), canonical)
	bounded, cancel := context.WithTimeout(ctx, controlTimeout)
	response, commitErr := c.worker.client.CommitStep(bounded, &agentv1.CommitStepRequest{Execution: c.lease.Execution,
		Step: s, ResultJson: canonical, CommitHash: hash})
	cancel()
	if commitErr == nil && response != nil && acceptedMatches(response.AcceptedStep, s, c.lease.Execution.RunId, hash, canonical) {
		return stepOutcome{Commit: response}
	}
	if commitErr != nil && !uncertainRPC(commitErr) {
		reason := rpcReason(commitErr)
		if stopsAuthority(reason) {
			c.authority.Stop(reason)
			return stepOutcome{Abandoned: true}
		}
		if reason != "" {
			return stepOutcome{Failure: reason}
		}
	}
	// A missing or inconsistent ACK is not rollback evidence. This narrow read
	// confirms only the original content; it never supplies a new next cursor.
	confirmation, confirmErr := confirmFact(context.WithoutCancel(ctx), func(readCtx context.Context) (*agentv1.GetAcceptedCommitResponse, error) {
		return c.worker.client.GetAcceptedCommit(readCtx, &agentv1.GetAcceptedCommitRequest{Execution: c.lease.Execution, StepId: s.StepId})
	})
	if confirmErr == nil && confirmation != nil && confirmation.Found {
		if !acceptedMatches(confirmation.AcceptedStep, s, c.lease.Execution.RunId, hash, canonical) {
			return stepOutcome{Fatal: run.ErrStepConflict}
		}
	}
	// Even a confirmed intermediate result is resumed through a later Claim;
	// we do not infer a next step from this read-only response or report Fail.
	return stepOutcome{Abandoned: true}
}

func acceptedMatches(accepted *agentv1.AcceptedStep, step *agentv1.StepIdentity, runID, hash string, canonical []byte) bool {
	return accepted != nil && proto.Equal(accepted.Step, step) && accepted.CommitHash == hash &&
		bytes.Equal(accepted.ResultJson, canonical) && accepted.ResultRef == fmt.Sprintf("run-step:%s:%d", runID, step.Sequence)
}
