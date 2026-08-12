package grpc

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
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

	// WorkerCounts samples workers with a heartbeat fresher than freshWithin
	// per (version, status) for the jobforge_workers_active gauge (PRD 12.1).
	WorkerCounts(ctx context.Context, freshWithin time.Duration) ([]store.WorkerCountRow, error)

	// RefreshWorkerHeartbeat advances the worker's liveness timestamp,
	// writing at most once per minInterval (SQL-layer throttling).
	RefreshWorkerHeartbeat(ctx context.Context, workerID string, minInterval time.Duration) error
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
	// heartbeatInterval is advertised to Workers during Register. Workers
	// without an explicit local override adopt this value (ADR-0008).
	heartbeatInterval time.Duration
	metrics           *observability.Metrics

	// tenantMaxInflight limits how many inflight (running + cancelling) jobs
	// a tenant may have. Claim reserves slots on the derived counter and
	// skips jobs of tenants at their quota (PRD v0.3 FR-720~726). <= 0
	// disables it.
	tenantMaxInflight int

	// quotaPrefilter enables the candidate pre-filter that excludes full
	// tenants before the row-lock window (ADR-0007 §4). Correctness never
	// depends on it; disabling only costs fairness performance.
	quotaPrefilter bool

	// gaugeMu guards prevGaugeKeys, used to zero out workers_active series
	// whose (version, status) group disappeared since the previous sample.
	gaugeMu      sync.Mutex
	prevGaugeKey map[workerCountKey]struct{}
}

// NewWorkerService creates a WorkerService. tenantMaxInflight is the
// per-tenant inflight-job quota enforced during claim (PRD v0.3 FR-720~726);
// <= 0 disables the limit. quotaPrefilter toggles the full-tenant candidate
// pre-filter (ADR-0007 §4).
func NewWorkerService(s WorkerStore, waiter PollWaiter, leaseTTL, heartbeatInterval time.Duration, tenantMaxInflight int, quotaPrefilter bool, logger *slog.Logger, metrics *observability.Metrics) *WorkerService {
	if leaseTTL <= 0 {
		leaseTTL = domain.DefaultLeaseTTL
	}
	if heartbeatInterval <= 0 {
		heartbeatInterval = domain.DefaultHeartbeat
	}
	return &WorkerService{
		store:             s,
		waiter:            waiter,
		logger:            logger,
		leaseTTL:          leaseTTL,
		heartbeatInterval: heartbeatInterval,
		metrics:           metrics,
		tenantMaxInflight: tenantMaxInflight,
		quotaPrefilter:    quotaPrefilter,
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
	if len(req.Queues) == 0 {
		// Fail loud: a worker without queues could register but never claim.
		return nil, status.Error(codes.InvalidArgument, "queues must not be empty")
	}

	sessionID := domain.NewID()
	if err := svc.store.RegisterWorker(ctx, req, sessionID); err != nil {
		return nil, mapError(err)
	}

	// Emit the jobforge_workers_active gauge (PRD 12.1). Best-effort.
	svc.SampleWorkerCounts(ctx)

	svc.logger.Info("worker registered",
		"worker_id", req.WorkerId,
		"queues", req.Queues,
		"types", req.SupportedTypes,
		"capacity", req.Capacity,
	)

	return &workerv1.RegisterResponse{
		SessionId:         sessionID,
		HeartbeatInterval: durationpb.New(svc.heartbeatInterval),
	}, nil
}

// Poll requests available job leases using long-polling with pg_notify wakeup.
func (svc *WorkerService) Poll(ctx context.Context, req *workerv1.PollRequest) (*workerv1.PollResponse, error) {
	if req.WorkerId == "" {
		return nil, status.Error(codes.InvalidArgument, "worker_id is required")
	}

	// Idle workers only reach the Gateway through Poll, so every Poll also
	// serves as a worker liveness signal (best-effort, throttled in SQL).
	svc.refreshWorkerLiveness(ctx, req.WorkerId)

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

	// Fail loud on missing or malformed queues instead of silently claiming
	// nothing: every declared queue participates in the claim.
	if len(req.Queues) == 0 {
		return nil, status.Error(codes.InvalidArgument, "queues must not be empty")
	}
	for _, q := range req.Queues {
		if q == "" {
			return nil, status.Error(codes.InvalidArgument, "queues must not contain empty entries")
		}
	}

	claimParams := store.ClaimParams{
		Queues:            req.Queues,
		WorkerID:          req.WorkerId,
		Types:             req.Types,
		MaxJobs:           maxJobs,
		LeaseTTL:          svc.leaseTTL,
		TenantMaxInflight: svc.tenantMaxInflight, // PRD v0.3 tenant quota.
		QuotaPrefilter:    svc.quotaPrefilter,
	}

	// Try immediate claim.
	start := time.Now()
	res, err := svc.store.Claim(ctx, claimParams)
	if err != nil {
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, mapError(err)
	}
	jobs := res.Jobs
	svc.observeQuotaConflicts(ctx, res.QuotaConflicts)

	// Long-poll: if no jobs, wait for notification or deadline.
	for len(jobs) == 0 && ctx.Err() == nil {
		// Wait with a short timeout to allow periodic re-check.
		waitCtx, waitCancel := context.WithTimeout(ctx, 500*time.Millisecond)
		svc.waiter.WaitForNotification(waitCtx)
		waitCancel()

		if ctx.Err() != nil {
			break
		}

		res, err = svc.store.Claim(ctx, claimParams)
		if err != nil {
			span.SetStatus(otelcodes.Error, err.Error())
			return nil, mapError(err)
		}
		jobs = res.Jobs
		svc.observeQuotaConflicts(ctx, res.QuotaConflicts)
	}

	// Record claim duration metric (PRD 12.1). No queue attribute: a single
	// Poll may claim across several declared queues.
	claimDuration := time.Since(start).Seconds()
	if svc.metrics != nil {
		svc.metrics.ClaimDurationSeconds.Record(ctx, claimDuration)
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

	result, err := svc.store.Heartbeat(ctx, req.JobId, req.WorkerId, req.FencingToken, svc.leaseTTL)
	if err != nil {
		return nil, mapError(err)
	}

	// A successful job heartbeat also proves the worker process is alive
	// (best-effort, throttled in SQL).
	svc.refreshWorkerLiveness(ctx, req.WorkerId)

	signal := workerv1.ControlSignal_CONTROL_SIGNAL_CONTINUE
	if result.CancelRequested {
		signal = workerv1.ControlSignal_CONTROL_SIGNAL_CANCEL
		traceCtx := traceContextFromMetadata(ctx)
		traceCtx, span := observability.Tracer("jobforge.gateway").Start(traceCtx, "gateway.cancel_signal")
		span.SetAttributes(
			attribute.String("path", "heartbeat"),
			attribute.Float64("cancel.signal_latency_seconds", result.CancelSignalLatency.Seconds()),
		)
		if svc.metrics != nil {
			svc.metrics.CancelSignalLatencySeconds.Record(traceCtx, result.CancelSignalLatency.Seconds(),
				metric.WithAttributes(attribute.String("path", "heartbeat")))
		}
		span.End()
	}

	return &workerv1.HeartbeatResponse{
		Signal:     signal,
		LeaseUntil: timestamppb.New(result.LeaseUntil),
	}, nil
}

// Complete reports successful job execution. Idempotent.
func (svc *WorkerService) Complete(ctx context.Context, req *workerv1.CompleteRequest) (*workerv1.CompleteResponse, error) {
	if req.JobId == "" || req.WorkerId == "" {
		return nil, status.Error(codes.InvalidArgument, "job_id and worker_id are required")
	}

	// Restore the job's trace from incoming gRPC metadata so the
	// gateway.complete_job span joins the original submit trace (FR-503).
	ctx = traceContextFromMetadata(ctx)

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

// refreshWorkerLiveness advances workers.last_heartbeat_at at most once per
// throttle interval. Failures never affect the RPC outcome: liveness is an
// observability signal, not part of the lease contract.
func (svc *WorkerService) refreshWorkerLiveness(ctx context.Context, workerID string) {
	if err := svc.store.RefreshWorkerHeartbeat(ctx, workerID, svc.livenessRefreshInterval()); err != nil {
		svc.logger.Debug("refresh worker heartbeat failed", "worker_id", workerID, "error", err)
	}
}

// observeQuotaConflicts records candidates skipped because their tenant hit
// the hard cap during reservation (pre-filter staleness; PRD v0.3 §8).
func (svc *WorkerService) observeQuotaConflicts(ctx context.Context, conflicts int) {
	if svc.metrics == nil || conflicts <= 0 {
		return
	}
	svc.metrics.QuotaReservationConflictsTotal.Add(ctx, int64(conflicts))
}

// livenessRefreshInterval is the SQL-layer throttle for worker liveness
// writes: one third of the lease TTL (10s at the 30s default). This remains
// deliberately independent of the 5s per-job lease renewal cadence.
func (svc *WorkerService) livenessRefreshInterval() time.Duration {
	if svc.leaseTTL/3 > 0 {
		return svc.leaseTTL / 3
	}
	return time.Second
}

// workerFreshness is the window within which a worker's last heartbeat is
// considered live for the jobforge_workers_active gauge: twice the lease
// TTL, tolerating one missed touch (idle workers reach the Gateway at most
// once per Poll timeout).
func (svc *WorkerService) workerFreshness() time.Duration {
	return 2 * svc.leaseTTL
}

// workerCountKey identifies one workers_active gauge time series.
type workerCountKey struct {
	version string
	status  string
}

// SampleWorkerCounts records the jobforge_workers_active gauge from the
// current freshness-filtered worker counts (PRD 12.1). Groups present in
// the previous sample but absent now are explicitly recorded as 0 so the
// gauge decays to zero after workers crash or drain away. Best-effort.
func (svc *WorkerService) SampleWorkerCounts(ctx context.Context) {
	if svc.metrics == nil {
		return
	}
	counts, err := svc.store.WorkerCounts(ctx, svc.workerFreshness())
	if err != nil {
		return
	}

	current := make(map[workerCountKey]struct{}, len(counts))
	for _, c := range counts {
		svc.metrics.WorkersActive.Record(ctx, c.Count,
			metric.WithAttributes(
				attribute.String("version", c.Version),
				attribute.String("status", c.Status),
			))
		current[workerCountKey{version: c.Version, status: c.Status}] = struct{}{}
	}

	svc.gaugeMu.Lock()
	prev := svc.prevGaugeKey
	svc.prevGaugeKey = current
	svc.gaugeMu.Unlock()

	for key := range prev {
		if _, ok := current[key]; ok {
			continue
		}
		svc.metrics.WorkersActive.Record(ctx, 0,
			metric.WithAttributes(
				attribute.String("version", key.version),
				attribute.String("status", key.status),
			))
	}
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
		if j.TraceContext != nil {
			cj.TraceContext = *j.TraceContext
		}
		result = append(result, cj)
	}
	return result
}

// traceContextFromMetadata extracts the W3C traceparent from incoming gRPC
// metadata and returns a context carrying the remote span context (FR-503).
// Workers attach the job's traceparent to Complete/Fail RPCs so gateway
// spans join the original submit trace.
func traceContextFromMetadata(ctx context.Context) context.Context {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx
	}
	vals := md.Get(observability.TraceParentKey)
	if len(vals) == 0 {
		return ctx
	}
	return observability.ContextWithTraceParent(ctx, vals[0])
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
