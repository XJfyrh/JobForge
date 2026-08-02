package grpc

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/observability"
	"github.com/xjfyrh/jobforge/internal/store"
	workerv1 "github.com/xjfyrh/jobforge/proto/jobforge/worker/v1"
)

// WorkerStore defines the persistence operations required by the Gateway.
type WorkerStore interface {
	store.JobStore

	// RegisterWorker upserts a worker registration record.
	RegisterWorker(ctx context.Context, req *workerv1.RegisterRequest, sessionID string) error

	// GetJobState returns the current state of a job (for control signal checks).
	GetJobState(ctx context.Context, jobID string) (domain.JobState, error)

	// GetJobRunAt returns the run_at timestamp of a job (for next_retry_at).
	GetJobRunAt(ctx context.Context, jobID string) (*time.Time, error)
}

// PollWaiter provides notification-based wakeup for Poll long-polling.
type PollWaiter interface {
	// WaitForNotification blocks until a notification is received or ctx expires.
	WaitForNotification(ctx context.Context) bool
}

// WorkerService implements the WorkerServiceServer gRPC interface.
type WorkerService struct {
	workerv1.UnimplementedWorkerServiceServer
	store    WorkerStore
	waiter   PollWaiter
	logger   *slog.Logger
	leaseTTL time.Duration
	metrics  *observability.Metrics
}

// NewWorkerService creates a WorkerService.
func NewWorkerService(s WorkerStore, waiter PollWaiter, leaseTTL time.Duration, logger *slog.Logger, metrics *observability.Metrics) *WorkerService {
	return &WorkerService{
		store:    s,
		waiter:   waiter,
		logger:   logger,
		leaseTTL: leaseTTL,
		metrics:  metrics,
	}
}

// Register announces or refreshes a Worker's capabilities.
func (svc *WorkerService) Register(ctx context.Context, req *workerv1.RegisterRequest) (*workerv1.RegisterResponse, error) {
	if req.WorkerId == "" {
		return nil, status.Error(codes.InvalidArgument, "worker_id is required")
	}
	if req.Capacity <= 0 {
		return nil, status.Error(codes.InvalidArgument, "capacity must be positive")
	}

	sessionID := domain.NewID()
	if err := svc.store.RegisterWorker(ctx, req, sessionID); err != nil {
		return nil, mapError(err)
	}

	svc.logger.Info("worker registered",
		"worker_id", req.WorkerId,
		"queues", req.Queues,
		"types", req.SupportedTypes,
		"capacity", req.Capacity,
	)

	return &workerv1.RegisterResponse{
		SessionId:         sessionID,
		HeartbeatInterval: durationpb.New(10 * time.Second),
	}, nil
}

// Poll requests available job leases using long-polling with pg_notify wakeup.
func (svc *WorkerService) Poll(ctx context.Context, req *workerv1.PollRequest) (*workerv1.PollResponse, error) {
	if req.WorkerId == "" {
		return nil, status.Error(codes.InvalidArgument, "worker_id is required")
	}

	// gateway.claim_jobs span (PRD 12.2).
	ctx, span := observability.Tracer("jobforge.gateway").Start(ctx, "gateway.claim_jobs")
	defer span.End()
	span.SetAttributes(
		attribute.String("worker_id", req.WorkerId),
		attribute.Int("max_jobs", int(req.MaxJobs)),
	)

	maxJobs := int(req.MaxJobs)
	if maxJobs <= 0 {
		maxJobs = 1
	}
	if req.AvailableCapacity > 0 && int(req.AvailableCapacity) < maxJobs {
		maxJobs = int(req.AvailableCapacity)
	}

	// Determine queue: use first registered queue or request queue.
	queue := ""
	if len(req.Queues) > 0 {
		queue = req.Queues[0]
	}

	claimParams := store.ClaimParams{
		Queue:    queue,
		WorkerID: req.WorkerId,
		Types:    req.Types,
		MaxJobs:  maxJobs,
		LeaseTTL: svc.leaseTTL,
	}

	// Try immediate claim.
	start := time.Now()
	jobs, err := svc.store.Claim(ctx, claimParams)
	if err != nil {
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, mapError(err)
	}

	// Long-poll: if no jobs, wait for notification or deadline.
	for len(jobs) == 0 && ctx.Err() == nil {
		// Wait with a short timeout to allow periodic re-check.
		waitCtx, waitCancel := context.WithTimeout(ctx, 500*time.Millisecond)
		svc.waiter.WaitForNotification(waitCtx)
		waitCancel()

		if ctx.Err() != nil {
			break
		}

		jobs, err = svc.store.Claim(ctx, claimParams)
		if err != nil {
			span.SetStatus(otelcodes.Error, err.Error())
			return nil, mapError(err)
		}
	}

	// Record claim duration metric (PRD 12.1).
	claimDuration := time.Since(start).Seconds()
	if svc.metrics != nil {
		svc.metrics.ClaimDurationSeconds.Record(ctx, claimDuration,
			metric.WithAttributes(attribute.String("queue", queue)))
	}

	span.SetAttributes(attribute.Int("jobs.claimed", len(jobs)))

	return &workerv1.PollResponse{
		Jobs: toClaimedJobs(jobs),
	}, nil
}

// Heartbeat extends the lease and returns control signals.
func (svc *WorkerService) Heartbeat(ctx context.Context, req *workerv1.HeartbeatRequest) (*workerv1.HeartbeatResponse, error) {
	if req.JobId == "" || req.WorkerId == "" {
		return nil, status.Error(codes.InvalidArgument, "job_id and worker_id are required")
	}

	err := svc.store.Heartbeat(ctx, req.JobId, req.WorkerId, req.FencingToken, svc.leaseTTL)
	if err != nil {
		return nil, mapError(err)
	}

	// Check if the job has a pending cancel request.
	signal := workerv1.ControlSignal_CONTROL_SIGNAL_CONTINUE
	state, err := svc.store.GetJobState(ctx, req.JobId)
	if err == nil && state == domain.StateCancelling {
		signal = workerv1.ControlSignal_CONTROL_SIGNAL_CANCEL
	}

	leaseUntil := time.Now().Add(svc.leaseTTL)
	return &workerv1.HeartbeatResponse{
		Signal:     signal,
		LeaseUntil: timestamppb.New(leaseUntil),
	}, nil
}

// Complete reports successful job execution. Idempotent.
func (svc *WorkerService) Complete(ctx context.Context, req *workerv1.CompleteRequest) (*workerv1.CompleteResponse, error) {
	if req.JobId == "" || req.WorkerId == "" {
		return nil, status.Error(codes.InvalidArgument, "job_id and worker_id are required")
	}

	// gateway.complete_job span (PRD 12.2).
	ctx, span := observability.Tracer("jobforge.gateway").Start(ctx, "gateway.complete_job")
	defer span.End()
	span.SetAttributes(
		attribute.String("worker_id", req.WorkerId),
	)

	var durationMs int64
	if req.Duration != nil {
		durationMs = req.Duration.AsDuration().Milliseconds()
	}

	err := svc.store.Complete(ctx, req.JobId, req.WorkerId, req.FencingToken, req.ResultRef, durationMs)
	if err != nil {
		// Idempotency: if already succeeded with same token, return success.
		if isIdempotentComplete(ctx, err, svc.store, req) {
			return &workerv1.CompleteResponse{State: "succeeded"}, nil
		}
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, mapError(err)
	}

	// Record job attempt and latency metrics (PRD 12.1).
	if svc.metrics != nil {
		svc.metrics.JobAttemptsTotal.Add(ctx, 1,
			metric.WithAttributes(
				attribute.String("queue", ""),
				attribute.String("type", ""),
				attribute.String("outcome", "succeeded"),
			))
		if durationMs > 0 {
			svc.metrics.JobLatencySeconds.Record(ctx, float64(durationMs)/1000.0,
				metric.WithAttributes(
					attribute.String("queue", ""),
					attribute.String("type", ""),
					attribute.String("outcome", "succeeded"),
				))
		}
	}

	return &workerv1.CompleteResponse{State: "succeeded"}, nil
}

// Fail reports job execution failure. Idempotent.
func (svc *WorkerService) Fail(ctx context.Context, req *workerv1.FailRequest) (*workerv1.FailResponse, error) {
	if req.JobId == "" || req.WorkerId == "" {
		return nil, status.Error(codes.InvalidArgument, "job_id and worker_id are required")
	}

	var durationMs int64
	if req.Duration != nil {
		durationMs = req.Duration.AsDuration().Milliseconds()
	}

	err := svc.store.Fail(ctx, req.JobId, req.WorkerId, req.FencingToken,
		req.ErrorCode, req.ErrorMessage, req.Retryable, durationMs)
	if err != nil {
		// Idempotency: if the job already transitioned due to a prior Fail call,
		// return success with the current state.
		if isIdempotentFail(ctx, err, svc.store, req.JobId) {
			state, _ := svc.store.GetJobState(ctx, req.JobId)
			return &workerv1.FailResponse{State: string(state)}, nil
		}
		return nil, mapError(err)
	}

	// Record failure metrics (PRD 12.1).
	if svc.metrics != nil {
		svc.metrics.JobAttemptsTotal.Add(ctx, 1,
			metric.WithAttributes(
				attribute.String("queue", ""),
				attribute.String("type", ""),
				attribute.String("outcome", "failed"),
			))
		if req.Retryable {
			svc.metrics.RetriesTotal.Add(ctx, 1,
				metric.WithAttributes(
					attribute.String("queue", ""),
					attribute.String("error_code", req.ErrorCode),
				))
		}
	}

	// Determine resulting state.
	state, _ := svc.store.GetJobState(ctx, req.JobId)
	resp := &workerv1.FailResponse{State: string(state)}
	switch state {
	case domain.StateRetryWait:
		if runAt, err := svc.store.GetJobRunAt(ctx, req.JobId); err == nil && runAt != nil {
			resp.NextRetryAt = timestamppb.New(*runAt)
		}
	case domain.StateDead:
		// Record DLQ metric (PRD 12.1).
		if svc.metrics != nil {
			svc.metrics.DLQTotal.Add(ctx, 1,
				metric.WithAttributes(
					attribute.String("queue", ""),
					attribute.String("type", ""),
				))
		}
	}
	return resp, nil
}

// isIdempotentComplete checks if a Complete error is due to the job already
// being succeeded (idempotent retry).
func isIdempotentComplete(ctx context.Context, err error, s WorkerStore, req *workerv1.CompleteRequest) bool {
	var de *domain.Error
	if errors.As(err, &de) && de.Code == domain.CodeAlreadyTerminal {
		state, stateErr := s.GetJobState(ctx, req.JobId)
		return stateErr == nil && state == domain.StateSucceeded
	}
	return false
}

// isIdempotentFail checks if a Fail error is due to the job already having
// transitioned from a prior Fail call (idempotent retry). The job is expected
// to be in retry_wait, dead, or cancelled state.
func isIdempotentFail(ctx context.Context, err error, s WorkerStore, jobID string) bool {
	var de *domain.Error
	if !errors.As(err, &de) {
		return false
	}
	if de.Code != domain.CodeStaleLease && de.Code != domain.CodeAlreadyTerminal {
		return false
	}
	state, stateErr := s.GetJobState(ctx, jobID)
	if stateErr != nil {
		return false
	}
	return state == domain.StateRetryWait || state == domain.StateDead || state == domain.StateCancelled
}

// toClaimedJobs converts domain jobs to proto ClaimedJob messages.
func toClaimedJobs(jobs []*domain.Job) []*workerv1.ClaimedJob {
	result := make([]*workerv1.ClaimedJob, 0, len(jobs))
	for _, j := range jobs {
		cj := &workerv1.ClaimedJob{
			JobId:        j.ID,
			Queue:        j.Queue,
			Type:         j.Type,
			Payload:      j.Payload,
			Attempt:      int32(j.Attempt),
			MaxAttempts:  int32(j.MaxAttempts),
			FencingToken: j.FencingToken,
			Timeout:      durationpb.New(time.Duration(j.TimeoutSeconds) * time.Second),
		}
		if j.LeaseUntil != nil {
			cj.LeaseUntil = timestamppb.New(*j.LeaseUntil)
		}
		if j.TraceID != nil {
			cj.TraceId = *j.TraceID
		}
		result = append(result, cj)
	}
	return result
}

// mapError converts domain errors to gRPC status errors per ADR-0002.
func mapError(err error) error {
	var de *domain.Error
	if !errors.As(err, &de) {
		return status.Error(codes.Internal, "internal error")
	}

	switch de.Code {
	case domain.CodeInvalidArgument:
		return status.Error(codes.InvalidArgument, de.Message)
	case domain.CodeNotFound:
		return status.Error(codes.NotFound, de.Message)
	case domain.CodeStaleLease:
		return status.Error(codes.FailedPrecondition, de.Message)
	case domain.CodeAlreadyTerminal:
		return status.Error(codes.FailedPrecondition, de.Message)
	case domain.CodeCancelRequested:
		return status.Error(codes.Aborted, de.Message)
	case domain.CodeInvalidTransition:
		return status.Error(codes.FailedPrecondition, de.Message)
	case domain.CodeQueueOverloaded:
		return status.Error(codes.ResourceExhausted, de.Message)
	default:
		return status.Error(codes.Internal, de.Message)
	}
}
