package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

func proxyFixture() (fault, *agentv1.CommitStepRequest, *agentv1.CommitStepResponse) {
	f := fault{Experiment: "DEV-027-C", RunID: "11111111-1111-4111-8111-111111111111", TenantID: "tenant-north", ProfileHash: strings.Repeat("a", 64), Boundary: "first_read_commit"}
	execution := &agentv1.ExecutionIdentity{RunId: f.RunID, TenantId: f.TenantID, AttemptNo: 1, FencingToken: 1,
		Session: &agentv1.SessionIdentity{WorkerId: "worker-one", SessionId: "22222222-2222-4222-8222-222222222222"}}
	step := &agentv1.StepIdentity{StepId: "33333333-3333-4333-8333-333333333333", Sequence: 3, Kind: agentv1.StepKind_STEP_KIND_GET_ORDER,
		ProfileHash: f.ProfileHash, InputHash: strings.Repeat("b", 64), SnapshotHash: strings.Repeat("c", 64)}
	request := &agentv1.CommitStepRequest{Execution: execution, Step: step, CommitHash: strings.Repeat("d", 64)}
	response := &agentv1.CommitStepResponse{AcceptedStep: &agentv1.AcceptedStep{Step: step, CommitHash: request.CommitHash}, NextStep: proto.Clone(step).(*agentv1.StepIdentity)}
	response.NextStep.Kind, response.NextStep.Sequence = agentv1.StepKind_STEP_KIND_MODEL_DECISION, 4
	return f, request, response
}

func newProxyFixture() *proxy {
	return &proxy{used: map[string]bool{}, commits: map[string]bool{}, searches: map[string]int{}, armed: map[string]*agentv1.StepIdentity{}, armedOwner: map[string]*agentv1.ExecutionIdentity{}}
}

func TestProxyBoundaryUsesOriginalTupleAndSuccessfulCommitOnly(t *testing.T) {
	f, req, accepted := proxyFixture()
	for _, mode := range []string{"wrong_run", "wrong_tenant", "wrong_attempt", "wrong_profile", "wrong_commit", "missing_commit"} {
		t.Run(mode, func(t *testing.T) {
			request := proto.Clone(req).(*agentv1.CommitStepRequest)
			response := proto.Clone(accepted).(*agentv1.CommitStepResponse)
			switch mode {
			case "wrong_run":
				request.Execution.RunId = "different"
			case "wrong_tenant":
				request.Execution.TenantId = "tenant-south"
			case "wrong_attempt":
				request.Execution.AttemptNo = 2
			case "wrong_profile":
				request.Step.ProfileHash = strings.Repeat("e", 64)
			case "wrong_commit":
				response.AcceptedStep.CommitHash = strings.Repeat("f", 64)
			case "missing_commit":
				response.AcceptedStep = nil
			}
			if phase, _, _, _ := newProxyFixture().selectBoundary(f, request, response); phase != "" {
				t.Fatal("unconfirmed/wrong tuple selected")
			}
		})
	}
	p := newProxyFixture()
	if phase, _, _, _ := p.selectBoundary(f, req, accepted); phase != "commit_ack_lost" {
		t.Fatal("actual accepted commit not selected")
	}
	if phase, _, _, _ := p.selectBoundary(f, req, accepted); phase != "" {
		t.Fatal("fault selected twice")
	}
}

func TestProxyF06ReturnsCommitAndBlocksOnlyTheExactNextPermit(t *testing.T) {
	f, req, accepted := proxyFixture()
	f.Boundary = "first_search_commit"
	req.Step.Kind = agentv1.StepKind_STEP_KIND_SEARCH_POLICY
	p := newProxyFixture()
	if phase, _, _, _ := p.selectBoundary(f, req, accepted); phase != "" || p.armed[f.Experiment] == nil {
		t.Fatal("F06 lost the original Commit ACK")
	}
	wrong := &agentv1.ReserveCallRequest{Execution: req.Execution, Step: proto.Clone(accepted.NextStep).(*agentv1.StepIdentity)}
	wrong.Step.StepId = "another"
	if execution, _ := p.beforePermit(f, wrong); execution != nil {
		t.Fatal("wrong next step blocked as the fault")
	}
	for _, mode := range []string{"fence", "session", "snapshot", "kind"} {
		wrong := &agentv1.ReserveCallRequest{Execution: proto.Clone(req.Execution).(*agentv1.ExecutionIdentity), Step: proto.Clone(accepted.NextStep).(*agentv1.StepIdentity)}
		switch mode {
		case "fence":
			wrong.Execution.FencingToken++
		case "session":
			wrong.Execution.Session.SessionId = "44444444-4444-4444-8444-444444444444"
		case "snapshot":
			wrong.Step.SnapshotHash = strings.Repeat("f", 64)
		case "kind":
			wrong.Step.Kind = agentv1.StepKind_STEP_KIND_GET_DELIVERY
		}
		if execution, _ := p.beforePermit(f, wrong); execution != nil {
			t.Fatalf("wrong %s matched original Commit authority", mode)
		}
	}
	next := &agentv1.ReserveCallRequest{Execution: req.Execution, Step: accepted.NextStep}
	if execution, _ := p.beforePermit(f, next); execution == nil {
		t.Fatal("actual next permit not blocked")
	}
}

type upstreamFixture struct {
	agentv1.UnimplementedAgentServiceServer
}

func (*upstreamFixture) CommitStep(ctx context.Context, req *agentv1.CommitStepRequest) (*agentv1.CommitStepResponse, error) {
	if values := metadata.ValueFromIncomingContext(ctx, "authorization"); len(values) != 1 || values[0] != "Bearer synthetic-proxy-test" {
		return nil, status.Error(codes.Unauthenticated, "AUTH_REQUIRED")
	}
	return &agentv1.CommitStepResponse{AcceptedStep: &agentv1.AcceptedStep{Step: req.Step, CommitHash: req.CommitHash}}, nil
}

func TestProxyRealTCPForwardsAuthAndPreservesSuccessfulUpstreamBeforeLosingACK(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(server, &upstreamFixture{})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	connection, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	p := newProxyFixture()
	p.connection, p.directory = connection, t.TempDir()
	f, req, _ := proxyFixture()
	raw, _ := json.Marshal(f)
	if err := os.WriteFile(filepath.Join(p.directory, "fault.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(metadata.NewIncomingContext(t.Context(), metadata.Pairs("authorization", "Bearer synthetic-proxy-test")), time.Second)
	defer cancel()
	info := &grpc.UnaryServerInfo{FullMethod: "/jobforge.agent.v1.AgentService/CommitStep"}
	if _, err := p.forward(ctx, req, info, nil); status.Code(err) != codes.Unavailable {
		t.Fatalf("ACK was not lost: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.directory, f.Experiment+".barrier.json")); err != nil {
		t.Fatal("successful upstream fact was not archived")
	}
	response, err := p.forward(ctx, req, info, nil)
	if err != nil || response.(*agentv1.CommitStepResponse).AcceptedStep.CommitHash != req.CommitHash {
		t.Fatal("fault changed/replayed upstream content")
	}
}

func TestProxyStrictFaultFileAndActiveWindowExpires(t *testing.T) {
	f, req, _ := proxyFixture()
	p := newProxyFixture()
	p.directory = t.TempDir()
	path := filepath.Join(p.directory, "fault.json")
	for _, raw := range []string{`{"Run_ID":"x"}`, `null`, `{"experiment":"x","experiment":"y"}`} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readFault(path); err == nil {
			t.Fatal("ambiguous file selected a fault")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	if err := p.hold(ctx, f, req.Execution, req.Step, "before_next_permit", ""); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("hold did not expire: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.directory, f.Experiment+".active")); !os.IsNotExist(err) {
		t.Fatal("expired RPC left a false current-step window")
	}
}

func TestProxyMarkerPublicationIsCompleteAndExclusive(t *testing.T) {
	f, req, _ := proxyFixture()
	p := newProxyFixture()
	p.directory = t.TempDir()
	path := filepath.Join(p.directory, f.Experiment+".barrier.json")
	var readers sync.WaitGroup
	readers.Go(func() {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			raw, err := os.ReadFile(path)
			if os.IsNotExist(err) {
				continue
			}
			var marker map[string]any
			if err != nil || json.Unmarshal(raw, &marker) != nil || marker["run_id"] != f.RunID || marker["phase"] != "commit_ack_lost" {
				t.Error("reader observed incomplete publication")
			}
			return
		}
		t.Error("complete publication was not visible")
	})
	if err := p.publish(f, req.Execution, req.Step, "commit_ack_lost", ""); err != nil {
		t.Fatal(err)
	}
	readers.Wait()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.publish(f, req.Execution, req.Step, "different", ""); err == nil {
		t.Fatal("existing evidence was overwritten")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("failed publication changed the original marker")
	}
}
