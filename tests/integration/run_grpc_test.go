package integration

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/run/grpcapi"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

func startRunGateway(t *testing.T, h *runHarness) agentv1.AgentServiceClient {
	t.Helper()
	server, err := grpcapi.NewServer(h.Store, grpcapi.Config{Workers: h.Options.Workers,
		Credentials: map[string]string{h.Principal: "contract-worker-token", "contract-worker-2": "contract-second-worker-token"}})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run gateway shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run gateway did not stop")
		}
	})
	connection, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return agentv1.NewAgentServiceClient(connection)
}

func assertRunRPCError(t *testing.T, err error, code codes.Code, reason string) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("RPC code=%s wanted=%s error=%v", status.Code(err), code, err)
	}
	for _, detail := range status.Convert(err).Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok && info.Domain == "jobforge.agent.v1" && info.Reason == reason && len(info.Metadata) == 0 {
			return
		}
	}
	t.Fatalf("RPC missing safe stable reason %s", reason)
}

func TestRunWorkerRPCAuthenticationAuthorityAndCheckpoint(t *testing.T) {
	h := setupRunHarness(t)
	r := h.submit(t, "tenant-a", "worker-rpc")
	client := startRunGateway(t, h)
	deadline, cancel := context.WithTimeout(h.Ctx, 20*time.Second)
	defer cancel()
	ctx := metadata.AppendToOutgoingContext(deadline, "authorization", "Bearer contract-worker-token")
	session := &agentv1.SessionIdentity{WorkerId: h.Principal, SessionId: h.Session.ID}
	_, err := client.Claim(deadline, &agentv1.ClaimRequest{Session: session})
	assertRunRPCError(t, err, codes.Unauthenticated, "UNAUTHORIZED")
	_, err = client.Claim(metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer contract-worker-token"), &agentv1.ClaimRequest{Session: session})
	assertRunRPCError(t, err, codes.InvalidArgument, "INVALID_ARGUMENT")
	forgedSession := proto.Clone(session).(*agentv1.SessionIdentity)
	forgedSession.WorkerId = "contract-worker-2"
	_, err = client.Claim(ctx, &agentv1.ClaimRequest{Session: forgedSession})
	assertRunRPCError(t, err, codes.PermissionDenied, "FORBIDDEN")
	claimed, err := client.Claim(ctx, &agentv1.ClaimRequest{Session: session})
	if err != nil || claimed.Lease == nil {
		t.Fatalf("real RPC Claim: %v", err)
	}
	lease := claimed.Lease
	if lease.Execution.RunId != r.ID || lease.Checkpoint.Snapshot == nil || lease.Checkpoint.Snapshot.SnapshotId != r.SnapshotID ||
		len(lease.Checkpoint.Snapshot.TicketBindingJson) == 0 || len(lease.Checkpoint.Snapshot.VersionVectorJson) == 0 {
		t.Fatal("RPC lost frozen business binding")
	}
	idle, err := client.Heartbeat(ctx, &agentv1.HeartbeatRequest{Session: session})
	if err != nil || idle.LeaseUntil != nil || idle.SessionExpiresAt == nil {
		t.Fatalf("idle session heartbeat granted execution: %v", err)
	}
	forged := proto.Clone(lease.Execution).(*agentv1.ExecutionIdentity)
	forged.FencingToken++
	_, err = client.GetCheckpoint(ctx, &agentv1.GetCheckpointRequest{Execution: forged})
	assertRunRPCError(t, err, codes.FailedPrecondition, "STALE_LEASE")
	checkpoint, err := client.GetCheckpoint(ctx, &agentv1.GetCheckpointRequest{Execution: lease.Execution})
	if err != nil || checkpoint.Checkpoint.NextStep.StepId != lease.Checkpoint.NextStep.StepId {
		t.Fatalf("real checkpoint RPC: %v", err)
	}
	_, err = client.CommitStep(ctx, &agentv1.CommitStepRequest{Execution: lease.Execution, Step: checkpoint.Checkpoint.NextStep,
		CommitHash: strings.Repeat("a", 64), ResultJson: []byte(`{"oversized":"` + strings.Repeat("x", 16*1024) + `"}`)})
	assertRunRPCError(t, err, codes.InvalidArgument, "CHECKPOINT_TOO_LARGE")
	if _, err := h.Service.Cancel(h.Ctx, r.TenantID, r.ID, "rpc-cancel"); err != nil {
		t.Fatal(err)
	}
	stop, err := client.Heartbeat(ctx, &agentv1.HeartbeatRequest{Session: session, Execution: lease.Execution})
	if err != nil || stop.Signal != agentv1.ControlSignal_CONTROL_SIGNAL_STOP || stop.StopReason != agentrun.StopCancel {
		t.Fatalf("cancel did not propagate through actual Worker RPC: %v", err)
	}
	if !stop.LeaseUntil.AsTime().Equal(lease.LeaseUntil.AsTime()) {
		t.Fatal("stopping heartbeat extended execution authority")
	}
	closed, err := client.AcknowledgeStopped(ctx, &agentv1.AcknowledgeStoppedRequest{Execution: lease.Execution})
	if err != nil || closed.State != agentv1.RunState_RUN_STATE_CANCELLED {
		t.Fatalf("stop acknowledgement RPC: %v", err)
	}
	_, err = client.AcknowledgeStopped(ctx, &agentv1.AcknowledgeStoppedRequest{Execution: lease.Execution})
	assertRunRPCError(t, err, codes.FailedPrecondition, "STALE_LEASE")
	secondCtx := metadata.AppendToOutgoingContext(deadline, "authorization", "Bearer contract-second-worker-token")
	registered, err := client.Register(secondCtx, &agentv1.RegisterRequest{StartupId: uuid.NewString(), Version: h.Profile.ExecutorVersion})
	if err != nil || registered.Session.WorkerId != "contract-worker-2" || registered.Capacity != 2 || len(registered.ProfileIds) != 1 {
		t.Fatalf("server configured registration: %v", err)
	}
}

func TestRunWorkerRPCPreservesRejectedModelClassification(t *testing.T) {
	h := setupRunHarness(t)
	h.submit(t, "tenant-a", "worker-model-error")
	claimed := h.claim(t)
	for currentRunStep(claimed).Kind != "model_proposal" {
		checkpointAdvance(t, h, &claimed, "proposal", false)
	}
	// Use the real ledger but explicitly synthetic model evidence. A structurally
	// valid proposal citing unavailable evidence must retain its permanent reason.
	result := checkpointFixtureResult(t, h, claimed, "proposal", false)
	result.Proposal.EvidenceRefs = []string{"business-evidence:" + uuid.NewString() + ":ticket"}
	request := checkpointCommitRequest(t, claimed, result)
	client := startRunGateway(t, h)
	deadline, cancel := context.WithTimeout(h.Ctx, 10*time.Second)
	defer cancel()
	ctx := metadata.AppendToOutgoingContext(deadline, "authorization", "Bearer contract-worker-token")
	execution := &agentv1.ExecutionIdentity{TenantId: claimed.Lease.TenantID, RunId: claimed.Lease.RunID,
		Session:   &agentv1.SessionIdentity{WorkerId: h.Principal, SessionId: h.Session.ID},
		AttemptNo: claimed.Lease.AttemptNo, FencingToken: claimed.Lease.FencingToken}
	checkpoint, err := client.GetCheckpoint(ctx, &agentv1.GetCheckpointRequest{Execution: execution})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CommitStep(ctx, &agentv1.CommitStepRequest{Execution: execution, Step: checkpoint.Checkpoint.NextStep,
		CommitHash: request.CommitHash, ResultJson: request.ResultJSON})
	assertRunRPCError(t, err, codes.InvalidArgument, "MODEL_PROTOCOL_ERROR")
	view, err := h.Store.Get(h.Ctx, claimed.Lease.TenantID, claimed.Lease.RunID)
	if err != nil || view.CursorVersion != request.Step.CursorVersion || view.State != agentrun.Running {
		t.Fatal("rejected model result changed Run state or cursor")
	}
	failed, err := client.FailAttempt(ctx, &agentv1.FailAttemptRequest{Execution: execution, Step: checkpoint.Checkpoint.NextStep,
		ErrorCode: "MODEL_PROTOCOL_ERROR"})
	if err != nil || failed.State != agentv1.RunState_RUN_STATE_FAILED || failed.RecoveryCount != 0 {
		t.Fatalf("permanent result failure became automatic recovery: %v", err)
	}
}
