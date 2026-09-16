// Package grpcapi exposes the authenticated, bounded Agent Worker protocol.
// It maps typed requests only; Run state, lease and budget decisions belong to API.
package grpcapi

import (
	"context"
	"crypto/sha256"
	"slices"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/xjfyrh/jobforge/internal/run"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

// MaxMessageBytes bounds both directions, including a protected checkpoint.
const MaxMessageBytes = 384 * 1024

// API is the consuming transport's transactional Worker service boundary.
type API interface {
	Register(context.Context, string, string, string) (run.Session, error)
	Claim(context.Context, string, string) (*run.ClaimedRun, error)
	HeartbeatSession(context.Context, string, string, string) (run.Session, error)
	HeartbeatExecution(context.Context, string, run.Lease) (run.HeartbeatResult, error)
	GetCheckpoint(context.Context, string, run.Lease) (run.Checkpoint, error)
	BeginTool(context.Context, string, run.BeginToolRequest) (run.BeginToolResponse, error)
	ReserveCall(context.Context, string, run.ReserveCallRequest) (run.ReserveCallResponse, error)
	ObserveCall(context.Context, string, run.ObserveCallRequest) (run.CallReservation, error)
	SettleUsage(context.Context, string, run.SettleUsageRequest) (run.SettleUsageResponse, error)
	CommitStep(context.Context, string, run.CommitStepRequest) (run.CommitStepResponse, error)
	FailExecution(context.Context, string, run.Lease, run.StepIdentity, string) (run.Run, error)
	AcknowledgeStop(context.Context, string, run.Lease) (run.StopResult, error)
	GetAcceptedCommit(context.Context, string, run.Lease, string) (run.AcceptedCommitResponse, error)
}

// Config contains only trusted deployment data, never Worker-provided capacity.
type Config struct {
	Credentials map[string]string // Authenticated principal -> distinct Bearer token.
	Workers     []run.WorkerConfig
}

type identityKey struct{}

type service struct {
	agentv1.UnimplementedAgentServiceServer
	api         API
	credentials map[[32]byte]string
	workers     map[string]run.WorkerConfig
}

// NewServer copies fixed credentials and capabilities before registering the
// twelve unary RPCs. Callers own listeners and graceful shutdown.
func NewServer(api API, config Config) (*grpc.Server, error) {
	if api == nil || len(config.Credentials) == 0 || len(config.Credentials) > 128 || len(config.Workers) > 128 {
		return nil, run.ErrInvalidArgument
	}
	h := &service{api: api, credentials: make(map[[32]byte]string), workers: make(map[string]run.WorkerConfig)}
	for _, worker := range config.Workers {
		if !run.ValidIdentifier(worker.ID) || worker.Capacity < 1 || worker.Capacity > 100 || len(worker.Tenants) == 0 || len(worker.Tenants) > 128 || len(worker.ProfileIDs) > 128 {
			return nil, run.ErrInvalidArgument
		}
		if _, exists := h.workers[worker.ID]; exists {
			return nil, run.ErrInvalidArgument
		}
		for _, value := range append(slices.Clone(worker.Tenants), worker.ProfileIDs...) {
			if !run.ValidIdentifier(value) {
				return nil, run.ErrInvalidArgument
			}
		}
		worker.Tenants, worker.ProfileIDs = slices.Clone(worker.Tenants), slices.Clone(worker.ProfileIDs)
		h.workers[worker.ID] = worker
	}
	for principal, token := range config.Credentials {
		if _, exists := h.workers[principal]; !exists || !validCredential(token) {
			return nil, run.ErrInvalidArgument
		}
		digest := sha256.Sum256([]byte(token))
		if _, exists := h.credentials[digest]; exists {
			return nil, run.ErrInvalidArgument
		}
		h.credentials[digest] = principal
	}
	server := grpc.NewServer(grpc.MaxRecvMsgSize(MaxMessageBytes), grpc.MaxSendMsgSize(MaxMessageBytes),
		grpc.MaxConcurrentStreams(128), grpc.UnaryInterceptor(h.boundary))
	agentv1.RegisterAgentServiceServer(server, h)
	return server, nil
}

func (h *service) boundary(ctx context.Context, request any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
	values := metadata.ValueFromIncomingContext(ctx, "authorization")
	if len(values) != 1 {
		return nil, mapError(run.ErrUnauthorized)
	}
	parts := strings.Split(values[0], " ")
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || !validCredential(parts[1]) {
		return nil, mapError(run.ErrUnauthorized)
	}
	principal, ok := h.credentials[sha256.Sum256([]byte(parts[1]))]
	if !ok {
		return nil, mapError(run.ErrUnauthorized)
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		return nil, mapError(run.ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return nil, mapError(err)
	}
	carrier := propagation.MapCarrier{}
	for _, name := range []string{"traceparent", "tracestate"} {
		if incoming := metadata.ValueFromIncomingContext(ctx, name); len(incoming) == 1 {
			carrier[name] = incoming[0]
		}
	}
	ctx = propagation.TraceContext{}.Extract(ctx, carrier)
	ctx, span := otel.Tracer("jobforge/run/grpcapi").Start(ctx, "run.worker_rpc")
	defer span.End()
	span.SetAttributes(attribute.String("rpc.method", info.FullMethod))
	response, err := next(context.WithValue(ctx, identityKey{}, principal), request)
	if err != nil {
		return nil, mapError(err)
	}
	return response, nil
}

func authenticated(ctx context.Context) string {
	principal, _ := ctx.Value(identityKey{}).(string)
	return principal
}

func validCredential(value string) bool {
	if len(value) == 0 || len(value) > 256 {
		return false
	}
	for _, char := range value {
		if char < 33 || char > 126 {
			return false
		}
	}
	return true
}
