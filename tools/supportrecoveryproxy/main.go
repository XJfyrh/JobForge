// Command supportrecoveryproxy forwards bounded Worker RPCs for a separately
// released S3 experiment. It is never built into a production image.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/xjfyrh/jobforge/internal/jsonstrict"
	"github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/run/grpcapi"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

type fault struct {
	Experiment  string `json:"experiment"`
	RunID       string `json:"run_id"`
	TenantID    string `json:"tenant_id"`
	ProfileHash string `json:"profile_hash"`
	Boundary    string `json:"boundary"`
}

type proxy struct {
	connection *grpc.ClientConn
	directory  string
	mu         sync.Mutex
	used       map[string]bool
	commits    map[string]bool
	searches   map[string]int
	armed      map[string]*agentv1.StepIdentity
	armedOwner map[string]*agentv1.ExecutionIdentity
}

func readFault(path string) (fault, error) {
	var f fault
	file, err := os.Open(path)
	if err != nil {
		return f, err
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, 2049))
	if err != nil || len(raw) > 2048 || jsonstrict.Decode(raw, &f) != nil {
		return f, errors.New("FAULT_INVALID")
	}
	if !run.ValidIdentifier(f.Experiment) || !run.ValidUUID(f.RunID) ||
		!run.ValidIdentifier(f.TenantID) || !run.ValidHash(f.ProfileHash) ||
		(f.Boundary != "first_search_commit" && f.Boundary != "first_read_commit" && f.Boundary != "second_search_decision" &&
			f.Boundary != "first_chat_observation") {
		return f, errors.New("FAULT_INVALID")
	}
	return f, nil
}

func (p *proxy) publish(f fault, execution *agentv1.ExecutionIdentity, step *agentv1.StepIdentity, phase, physicalCall string) error {
	raw, err := json.Marshal(map[string]any{"schema_version": 1, "experiment": f.Experiment, "run_id": f.RunID,
		"tenant_id": f.TenantID, "attempt_no": execution.AttemptNo, "fencing_token": execution.FencingToken,
		"worker_id": execution.GetSession().GetWorkerId(), "session_id": execution.GetSession().GetSessionId(), "step_id": step.StepId,
		"step_kind": step.Kind.String(), "sequence": step.Sequence, "cursor_version": step.CursorVersion,
		"profile_hash": step.ProfileHash, "snapshot_hash": step.SnapshotHash, "input_hash": step.InputHash,
		"phase": phase, "physical_call_id": physicalCall, "observed_at": time.Now().UTC()})
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(p.directory, ".barrier-*")
	if err != nil {
		return err
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	if _, err := file.Write(append(raw, '\n')); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	// Link publishes complete bytes atomically and fails if the immutable
	// final name already exists. Readers can never see a half-written JSON.
	return os.Link(file.Name(), filepath.Join(p.directory, f.Experiment+".barrier.json"))
}

// selectBoundary consumes only successful real upstream results. It never
// invents a report, observation, checkpoint or a permission.
func (p *proxy) selectBoundary(f fault, request, response any) (string, *agentv1.ExecutionIdentity, *agentv1.StepIdentity, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.used[f.Experiment] {
		return "", nil, nil, ""
	}
	var execution *agentv1.ExecutionIdentity
	var step *agentv1.StepIdentity
	phase, call := "", ""
	switch req := request.(type) {
	case *agentv1.CommitStepRequest:
		execution, step = req.Execution, req.Step
		accepted, ok := response.(*agentv1.CommitStepResponse)
		if !ok || accepted.AcceptedStep == nil || accepted.AcceptedStep.CommitHash != req.CommitHash || step == nil || execution == nil {
			return "", nil, nil, ""
		}
		if execution.RunId != f.RunID || execution.TenantId != f.TenantID || execution.AttemptNo != 1 || step.ProfileHash != f.ProfileHash {
			return "", nil, nil, ""
		}
		key := f.Experiment + ":" + step.StepId
		if p.commits[key] {
			return "", nil, nil, ""
		}
		p.commits[key] = true
		switch f.Boundary {
		case "first_read_commit":
			if step.Kind == agentv1.StepKind_STEP_KIND_GET_ORDER || step.Kind == agentv1.StepKind_STEP_KIND_GET_DELIVERY || step.Kind == agentv1.StepKind_STEP_KIND_SEARCH_POLICY {
				phase = "commit_ack_lost"
			}
		case "first_search_commit":
			if step.Kind == agentv1.StepKind_STEP_KIND_SEARCH_POLICY {
				p.armed[f.Experiment] = accepted.NextStep
				p.armedOwner[f.Experiment] = execution
			}
		case "second_search_decision":
			var result struct {
				Content run.SupportToolDecision `json:"content"`
			}
			if step.Kind == agentv1.StepKind_STEP_KIND_MODEL_DECISION && json.Unmarshal(req.ResultJson, &result) == nil &&
				result.Content.Type == "tool" && result.Content.Name == "search_policy" {
				p.searches[f.Experiment]++
				if p.searches[f.Experiment] == 2 {
					phase = "commit_ack_lost"
				}
			}
		}
	case *agentv1.ObserveCallRequest:
		execution, step = req.Execution, req.Step
		observed, ok := response.(*agentv1.ObserveCallResponse)
		if ok && observed.Reservation != nil && observed.Reservation.Subcall == agentv1.Subcall_SUBCALL_CHAT &&
			observed.Reservation.UsageKnown && !observed.Reservation.MeasurementAnomaly && observed.Reservation.PersistedReportHash != "" &&
			observed.Reservation.PersistedAuditHash != "" && req.BusinessOutcome == agentv1.BusinessOutcome_BUSINESS_OUTCOME_ACCEPTED &&
			req.TransportOutcome == agentv1.TransportOutcome_TRANSPORT_OUTCOME_RESPONSE && req.HttpStatus == 200 && req.ErrorCode == "" && f.Boundary == "first_chat_observation" {
			phase, call = "observation_ack_pending", req.PhysicalCallId
		}
	}
	if phase == "" || execution == nil || step == nil || execution.RunId != f.RunID || execution.TenantId != f.TenantID ||
		execution.AttemptNo != 1 || step.ProfileHash != f.ProfileHash {
		return "", nil, nil, ""
	}
	p.used[f.Experiment] = true
	return phase, execution, step, call
}

func (p *proxy) beforePermit(f fault, request any) (*agentv1.ExecutionIdentity, *agentv1.StepIdentity) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var execution *agentv1.ExecutionIdentity
	var step *agentv1.StepIdentity
	switch req := request.(type) {
	case *agentv1.BeginToolRequest:
		execution, step = req.Execution, req.Step
	case *agentv1.ReserveCallRequest:
		execution, step = req.Execution, req.Step
	}
	expected := p.armed[f.Experiment]
	if p.used[f.Experiment] || expected == nil || execution == nil || step == nil || execution.RunId != f.RunID ||
		execution.TenantId != f.TenantID || execution.AttemptNo != 1 || step.ProfileHash != f.ProfileHash ||
		!proto.Equal(step, expected) || !proto.Equal(execution, p.armedOwner[f.Experiment]) {
		return nil, nil
	}
	p.used[f.Experiment] = true
	return execution, step
}

func (p *proxy) forward(ctx context.Context, request any, info *grpc.UnaryServerInfo, _ grpc.UnaryHandler) (any, error) {
	name := strings.TrimPrefix(info.FullMethod, "/jobforge.agent.v1.AgentService/")
	method := agentv1.File_jobforge_agent_v1_agent_proto.Services().ByName("AgentService").Methods().ByName(protoreflect.Name(name))
	if method == nil {
		return nil, status.Error(codes.Unimplemented, "RPC_UNAVAILABLE")
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, status.Error(codes.InvalidArgument, "DEADLINE_REQUIRED")
	}
	typeOfResponse, err := protoregistry.GlobalTypes.FindMessageByName(method.Output().FullName())
	if err != nil {
		return nil, status.Error(codes.Internal, "RPC_UNAVAILABLE")
	}
	values := metadata.ValueFromIncomingContext(ctx, "authorization")
	if len(values) != 1 {
		return nil, status.Error(codes.Unauthenticated, "AUTH_REQUIRED")
	}
	bounded, cancel := context.WithTimeout(metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", values[0])), 5*time.Second)
	defer cancel()
	f, err := readFault(filepath.Join(p.directory, "fault.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, status.Error(codes.Unavailable, "FAULT_INVALID")
	}
	if err == nil {
		if execution, step := p.beforePermit(f, request); execution != nil {
			return nil, p.hold(ctx, f, execution, step, "before_next_permit", "")
		}
	}
	response := typeOfResponse.New().Interface()
	if err := p.connection.Invoke(bounded, info.FullMethod, request, response); err != nil {
		return nil, err
	}
	if errors.Is(err, os.ErrNotExist) {
		return response, nil
	}
	if err != nil {
		return nil, status.Error(codes.Unavailable, "FAULT_INVALID")
	}
	phase, execution, step, call := p.selectBoundary(f, request, response)
	if phase == "" {
		return response, nil
	}
	if phase == "commit_ack_lost" {
		if err := p.publish(f, execution, step, phase, call); err != nil {
			return nil, status.Error(codes.Unavailable, "BARRIER_UNCONFIRMED")
		}
		return nil, status.Error(codes.Unavailable, "EXPERIMENT_ACK_LOST")
	}
	if err := p.hold(ctx, f, execution, step, phase, call); err != nil {
		return nil, err
	}
	return response, nil
}

func (p *proxy) hold(ctx context.Context, f fault, execution *agentv1.ExecutionIdentity, step *agentv1.StepIdentity, phase, call string) error {
	// Presence proves this exact RPC is still blocked. Removing it on every
	// return prevents an old marker from identifying a later child as the fault.
	active := filepath.Join(p.directory, f.Experiment+".active")
	file, err := os.OpenFile(active, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return status.Error(codes.Unavailable, "BARRIER_UNCONFIRMED")
	}
	_ = file.Close()
	defer func() { _ = os.Remove(active) }()
	if err := p.publish(f, execution, step, phase, call); err != nil {
		return status.Error(codes.Unavailable, "BARRIER_UNCONFIRMED")
	}
	// The external owner suspends the actual step before allowing this ACK.
	// No payload is changed. The supervisor kills only after the real pipe
	// contains the ACK, then confirms Wait and complete group disappearance.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if phase == "observation_ack_pending" {
			if _, err := os.Stat(filepath.Join(p.directory, f.Experiment+".release")); err == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case <-ticker.C:
		}
	}
}

func serve(ctx context.Context, upstream, address, directory string) error {
	connection, err := grpc.NewClient(upstream, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(grpcapi.MaxMessageBytes), grpc.MaxCallSendMsgSize(grpcapi.MaxMessageBytes)))
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close() }()
	p := &proxy{connection: connection, directory: directory, used: map[string]bool{}, commits: map[string]bool{}, searches: map[string]int{}, armed: map[string]*agentv1.StepIdentity{}, armedOwner: map[string]*agentv1.ExecutionIdentity{}}
	server := grpc.NewServer(grpc.MaxRecvMsgSize(grpcapi.MaxMessageBytes), grpc.MaxSendMsgSize(grpcapi.MaxMessageBytes), grpc.MaxConcurrentStreams(16), grpc.UnaryInterceptor(p.forward))
	agentv1.RegisterAgentServiceServer(server, &agentv1.UnimplementedAgentServiceServer{})
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); server.Stop() }()
	return server.Serve(listener)
}

func main() {
	upstream := flag.String("upstream", "control:8094", "fixed private control gateway")
	address := flag.String("listen", "127.0.0.1:8095", "local experiment gateway")
	directory := flag.String("directory", "/var/lib/jobforge/recovery", "private experiment barriers")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := serve(ctx, *upstream, *address, *directory); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		_, _ = fmt.Fprintln(os.Stderr, "RECOVERY_PROXY_STOPPED")
		os.Exit(1)
	}
}
