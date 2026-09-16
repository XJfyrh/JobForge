package runworker

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/runclock"
	"github.com/xjfyrh/jobforge/internal/runinput"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

type stepRunner func(context.Context, *agentv1.RunLease, *agentv1.Checkpoint, executionAuthority) stepOutcome

type workerSession struct {
	identity *agentv1.SessionIdentity
	deadline int64
}

// Run registers one fixed-version session and executes server-selected Runs at
// capacity one. Native non-Linux clocks fail before any registration or Claim.
func (w *Worker) Run(ctx context.Context) error {
	return w.runLoop(ctx, runclock.Now, w.runStep)
}

func (w *Worker) runLoop(ctx context.Context, now clockSample, execute stepRunner) error {
	session, err := w.register(ctx, now)
	if err != nil {
		return err
	}
	nextHeartbeat, err := now()
	if err != nil {
		return err
	}
	nextHeartbeat += 5000
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		current, clockErr := now()
		if clockErr != nil || current >= session.deadline {
			return ErrAuthority
		}
		if current >= nextHeartbeat {
			if err = w.idleHeartbeat(ctx, now, &session); err != nil {
				return err
			}
			nextHeartbeat = current + 5000
		}
		start, clockErr := now()
		if clockErr != nil || start >= session.deadline {
			return ErrAuthority
		}
		rpcContext, cancel := context.WithTimeout(ctx, controlTimeout)
		response, claimErr := w.client.Claim(rpcContext, &agentv1.ClaimRequest{Session: session.identity})
		cancel()
		received, clockErr := now()
		if clockErr != nil || received >= session.deadline {
			return ErrAuthority
		}
		if claimErr != nil {
			if authorityRPCFailure(claimErr) {
				return ErrAuthority
			}
		} else if response == nil {
			return run.ErrInternal
		} else if response.Lease != nil {
			lease := response.Lease
			if err = w.validClaim(lease, session.identity); err != nil {
				return err
			}
			session.deadline, err = w.runClaim(ctx, now, execute, lease, session.deadline, start, received)
			if err != nil {
				return err
			}
			// Keep the existing session heartbeat schedule across short Runs.
			// Completing work does not renew session authority.
		}
		if err = waitClaimPoll(ctx); err != nil {
			return err
		}
	}
}

func (w *Worker) register(ctx context.Context, now clockSample) (workerSession, error) {
	start, err := now()
	if err != nil {
		return workerSession{}, err
	}
	rpcContext, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	response, err := w.client.Register(rpcContext, &agentv1.RegisterRequest{StartupId: uuid.NewString(), Version: runinput.ExecutorVersion})
	received, clockErr := now()
	if err != nil {
		return workerSession{}, run.ErrDependencyUnavailable
	}
	if clockErr != nil || response == nil || response.Session == nil || !run.ValidIdentifier(response.Session.WorkerId) ||
		!run.ValidUUID(response.Session.SessionId) || response.Capacity != 1 || response.HeartbeatInterval == nil ||
		response.HeartbeatInterval.CheckValid() != nil || response.HeartbeatInterval.AsDuration() != 5*time.Second ||
		len(response.ProfileIds) != len(w.profiles) {
		return workerSession{}, run.ErrProfileUnavailable
	}
	seen := make(map[string]bool, len(response.ProfileIds))
	for _, id := range response.ProfileIds {
		if _, ok := w.profiles[id]; !ok || seen[id] {
			return workerSession{}, run.ErrProfileUnavailable
		}
		seen[id] = true
	}
	deadline, err := stampDeadline(start, received, response.AuthorityObservedAt, response.ExpiresAt, 60*time.Second)
	if err != nil {
		return workerSession{}, err
	}
	return workerSession{identity: proto.Clone(response.Session).(*agentv1.SessionIdentity), deadline: deadline}, nil
}

func (w *Worker) idleHeartbeat(ctx context.Context, now clockSample, session *workerSession) error {
	start, err := now()
	if err != nil || start >= session.deadline {
		return ErrAuthority
	}
	rpcContext, cancel := context.WithTimeout(ctx, controlTimeout)
	response, err := w.client.Heartbeat(rpcContext, &agentv1.HeartbeatRequest{Session: session.identity})
	cancel()
	received, clockErr := now()
	if clockErr != nil || received >= session.deadline {
		return ErrAuthority
	}
	if err != nil {
		if authorityRPCFailure(err) {
			return ErrAuthority
		}
		// A transport failure cannot renew authority; keep its old deadline.
		return nil
	}
	if response == nil || response.Signal != agentv1.ControlSignal_CONTROL_SIGNAL_CONTINUE || response.StopReason != "" || response.LeaseUntil != nil {
		return ErrAuthority
	}
	deadline, err := stampDeadline(start, received, response.AuthorityObservedAt, response.SessionExpiresAt, 60*time.Second)
	if err != nil {
		return ErrAuthority
	}
	session.deadline = deadline
	return nil
}

func (w *Worker) validClaim(lease *agentv1.RunLease, session *agentv1.SessionIdentity) error {
	e := lease.Execution
	if e == nil || !proto.Equal(e.Session, session) || !run.ValidIdentifier(e.TenantId) || !run.ValidUUID(e.RunId) ||
		e.AttemptNo < 1 || e.AttemptNo > run.MaxSafeInteger || e.FencingToken < 1 || e.FencingToken > run.MaxSafeInteger ||
		lease.Checkpoint == nil || lease.Checkpoint.NextStep == nil || lease.Checkpoint.Snapshot == nil {
		return run.ErrInvalidArgument
	}
	if _, ok := w.environments[e.TenantId]; !ok {
		return run.ErrProfileUnavailable
	}
	next := lease.Checkpoint.NextStep
	_, err := w.manifest.profile(next.ProfileId, next.ProfileHash)
	return err
}

func (w *Worker) runClaim(ctx context.Context, now clockSample, execute stepRunner, lease *agentv1.RunLease,
	sessionDeadline, start, received int64,
) (int64, error) {
	stepContext, cancel := context.WithCancel(ctx)
	keeper, err := newLeaseKeeper(lease, sessionDeadline, start, received, now, cancel)
	if err != nil {
		cancel()
		return sessionDeadline, err
	}
	var joined sync.WaitGroup
	joined.Add(2)
	go func() { defer joined.Done(); keeper.watch(stepContext) }()
	go func() { defer joined.Done(); w.leaseHeartbeat(stepContext, now, lease.Execution, keeper) }()
	defer func() {
		keeper.Stop("STOP_REQUESTED")
		cancel()
		joined.Wait()
	}()
	// Read committed state after Claim before starting a guardian. Only the
	// server can supply the cursor, including a recovered attempt's history.
	checkpoint, err := w.readCheckpoint(stepContext, lease.Execution)
	if err != nil || keeper.Check() != nil {
		keeper.Stop("STOP_REQUESTED")
		w.acknowledgeServerStop(ctx, lease.Execution, keeper)
		return keeper.sessionExpiry(), nil
	}
	if checkpoint.CursorVersion != lease.Checkpoint.CursorVersion || !proto.Equal(checkpoint.NextStep, lease.Checkpoint.NextStep) {
		return keeper.sessionExpiry(), run.ErrInternal
	}
	for {
		if keeper.Check() != nil {
			w.acknowledgeServerStop(ctx, lease.Execution, keeper)
			return keeper.sessionExpiry(), nil
		}
		outcome := execute(stepContext, lease, checkpoint, keeper)
		if outcome.Fatal != nil {
			return keeper.sessionExpiry(), outcome.Fatal
		}
		// execute returns only after actual process cleanup or a fatal receipt.
		// STOP/authority loss always outranks a result or a known domain failure.
		if keeper.Check() != nil {
			w.acknowledgeServerStop(ctx, lease.Execution, keeper)
			return keeper.sessionExpiry(), nil
		}
		if outcome.Abandoned {
			keeper.Stop("STOP_REQUESTED")
			return keeper.sessionExpiry(), nil
		}
		if outcome.Failure != "" {
			if !validStepFailure(outcome.Failure) {
				return keeper.sessionExpiry(), run.ErrInternal
			}
			rpcContext, rpcCancel := context.WithTimeout(stepContext, controlTimeout)
			_, _ = w.client.FailAttempt(rpcContext, &agentv1.FailAttemptRequest{Execution: lease.Execution, Step: checkpoint.NextStep, ErrorCode: outcome.Failure})
			rpcCancel()
			return keeper.sessionExpiry(), nil
		}
		commit := outcome.Commit
		if commit == nil || commit.AcceptedStep == nil || !proto.Equal(commit.AcceptedStep.Step, checkpoint.NextStep) ||
			commit.CursorVersion != checkpoint.CursorVersion+1 {
			return keeper.sessionExpiry(), run.ErrInternal
		}
		if commit.AttemptClosed {
			if commit.NextStep != nil || (commit.State != agentv1.RunState_RUN_STATE_SUCCEEDED && commit.State != agentv1.RunState_RUN_STATE_AWAITING_APPROVAL) {
				return keeper.sessionExpiry(), run.ErrInternal
			}
			return keeper.sessionExpiry(), nil
		}
		if commit.State != agentv1.RunState_RUN_STATE_RUNNING || commit.NextStep == nil {
			return keeper.sessionExpiry(), run.ErrInternal
		}
		nextCheckpoint, readErr := w.readCheckpoint(stepContext, lease.Execution)
		if readErr != nil || keeper.Check() != nil {
			keeper.Stop("STOP_REQUESTED")
			w.acknowledgeServerStop(ctx, lease.Execution, keeper)
			return keeper.sessionExpiry(), nil
		}
		if nextCheckpoint.CursorVersion != commit.CursorVersion || !proto.Equal(nextCheckpoint.NextStep, commit.NextStep) {
			return keeper.sessionExpiry(), run.ErrInternal
		}
		checkpoint = nextCheckpoint
	}
}

func (w *Worker) readCheckpoint(ctx context.Context, execution *agentv1.ExecutionIdentity) (*agentv1.Checkpoint, error) {
	rpcContext, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	response, err := w.client.GetCheckpoint(rpcContext, &agentv1.GetCheckpointRequest{Execution: execution})
	if err != nil {
		return nil, err
	}
	if response == nil || response.Checkpoint == nil || response.Checkpoint.NextStep == nil || response.Checkpoint.Snapshot == nil {
		return nil, run.ErrInternal
	}
	return response.Checkpoint, nil
}

func (w *Worker) leaseHeartbeat(ctx context.Context, now clockSample, execution *agentv1.ExecutionIdentity, keeper *leaseKeeper) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if keeper.Check() != nil {
				return
			}
			start, err := now()
			if err != nil {
				keeper.Stop("STALE_LEASE")
				return
			}
			rpcContext, cancel := context.WithTimeout(ctx, controlTimeout)
			response, rpcErr := w.client.Heartbeat(rpcContext, &agentv1.HeartbeatRequest{Execution: execution, Session: execution.Session})
			cancel()
			received, clockErr := now()
			if clockErr != nil {
				keeper.Stop("STALE_LEASE")
				return
			}
			if rpcErr != nil {
				if authorityRPCFailure(rpcErr) {
					keeper.Stop("STALE_LEASE")
					return
				}
				continue
			}
			if keeper.heartbeat(start, received, response) != nil {
				return
			}
		}
	}
}

func (w *Worker) acknowledgeServerStop(ctx context.Context, execution *agentv1.ExecutionIdentity, keeper *leaseKeeper) {
	if !keeper.stoppedByServer() || ctx.Err() != nil {
		return
	}
	rpcContext, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	_, _ = w.client.AcknowledgeStopped(rpcContext, &agentv1.AcknowledgeStoppedRequest{Execution: execution})
}

func authorityRPCFailure(err error) bool {
	return stopsAuthority(rpcReason(err)) || status.Code(err) == codes.Unauthenticated || status.Code(err) == codes.PermissionDenied ||
		errors.Is(err, run.ErrStaleLease) || errors.Is(err, run.ErrUnauthorized) || errors.Is(err, run.ErrForbidden) ||
		errors.Is(err, run.ErrStopRequested) || errors.Is(err, run.ErrCancelRequested) || errors.Is(err, run.ErrAlreadyTerminal)
}

func validStepFailure(reason string) bool {
	switch reason {
	case "INVALID_ARGUMENT", "PROFILE_UNAVAILABLE", "BUDGET_EXHAUSTED", "EXECUTOR_PROTOCOL_ERROR", "MODEL_PROTOCOL_ERROR", "MODEL_UNSUPPORTED", "CHECKPOINT_TOO_LARGE", "DEPENDENCY_UNAVAILABLE", "TIMEOUT":
		return true
	default:
		return false
	}
}

func waitClaimPoll(ctx context.Context) error {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
