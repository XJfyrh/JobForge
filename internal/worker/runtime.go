package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/observability"
	workerv1 "github.com/xjfyrh/jobforge/proto/jobforge/worker/v1"
)

// RuntimeConfig holds Worker Runtime tuning parameters.
type RuntimeConfig struct {
	// WorkerID is the unique identifier for this Worker instance.
	WorkerID string

	// InstanceID is a human-readable label (e.g. hostname-pid).
	InstanceID string

	// Queues is the list of queues this Worker polls from.
	Queues []string

	// Capacity is the maximum number of concurrent jobs.
	Capacity int

	// GatewayAddr is the gRPC address of the Worker Gateway.
	GatewayAddr string

	// HeartbeatInterval is how often to send heartbeats for running jobs.
	HeartbeatInterval time.Duration

	// PollTimeout is the gRPC deadline for Poll RPCs.
	PollTimeout time.Duration

	// ShutdownGrace is the maximum time to wait for inflight jobs on shutdown.
	ShutdownGrace time.Duration

	// Version is the Worker software version for observability.
	Version string
}

const (
	// reportAttempts bounds how many times a Complete/Fail RPC is attempted.
	reportAttempts = 3

	// reportAttemptTimeout bounds each individual reporting RPC attempt.
	reportAttemptTimeout = 5 * time.Second
)

// Runtime manages the Worker lifecycle: register, poll, execute, heartbeat.
type Runtime struct {
	cfg      RuntimeConfig
	registry *Registry
	logger   *slog.Logger
	client   workerv1.WorkerServiceClient
	conn     *grpc.ClientConn
	metrics  *observability.Metrics

	// heartbeatIntervalExplicit preserves whether the Runtime caller supplied
	// a local override. Unset Workers adopt RegisterResponse; explicit local
	// configuration wins (ADR-0008).
	heartbeatIntervalExplicit bool

	mu       sync.Mutex
	inflight int
	wg       sync.WaitGroup
	stopping bool

	// Heartbeat retry tuning (unexported so tests can shrink them).
	hbBackoffInitial     time.Duration
	hbBackoffMax         time.Duration
	reportBackoffInitial time.Duration
}

// NewRuntime creates a Worker Runtime with the given configuration and handlers.
func NewRuntime(cfg RuntimeConfig, registry *Registry, logger *slog.Logger, metrics *observability.Metrics) *Runtime {
	heartbeatIntervalExplicit := cfg.HeartbeatInterval > 0
	if cfg.Capacity <= 0 {
		cfg.Capacity = 5
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = domain.DefaultHeartbeat
	}
	if cfg.PollTimeout <= 0 {
		cfg.PollTimeout = 30 * time.Second
	}
	if cfg.ShutdownGrace <= 0 {
		cfg.ShutdownGrace = 30 * time.Second
	}
	return &Runtime{
		cfg:                       cfg,
		registry:                  registry,
		logger:                    logger,
		metrics:                   metrics,
		heartbeatIntervalExplicit: heartbeatIntervalExplicit,
		hbBackoffInitial:          1 * time.Second,
		hbBackoffMax:              10 * time.Second,
		reportBackoffInitial:      1 * time.Second,
	}
}

// Run starts the Worker Runtime. It blocks until ctx is cancelled.
func (r *Runtime) Run(ctx context.Context) error {
	// Connect to Gateway.
	conn, err := grpc.NewClient(r.cfg.GatewayAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("connect to gateway %s: %w", r.cfg.GatewayAddr, err)
	}
	r.conn = conn
	r.client = workerv1.NewWorkerServiceClient(conn)
	defer func() { _ = conn.Close() }()

	// Register with Gateway.
	if err := r.register(ctx); err != nil {
		return fmt.Errorf("register worker: %w", err)
	}

	r.logger.Info("worker runtime started",
		"worker_id", r.cfg.WorkerID,
		"queues", r.cfg.Queues,
		"types", r.registry.Types(),
		"capacity", r.cfg.Capacity,
		"heartbeat_interval", r.cfg.HeartbeatInterval,
	)

	// Main poll loop.
	sem := make(chan struct{}, r.cfg.Capacity)

	for ctx.Err() == nil {
		// Poll for jobs.
		jobs, err := r.poll(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			r.logger.Error("poll failed", "error", err)
			// Brief backoff on error.
			select {
			case <-time.After(1 * time.Second):
			case <-ctx.Done():
			}
			continue
		}

		// Dispatch claimed jobs.
		for _, job := range jobs {
			if ctx.Err() != nil {
				break
			}
			// Acquire semaphore slot (blocks if at capacity).
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
			}

			// After acquiring semaphore, re-check: if context was cancelled
			// during the select, do not dispatch a new goroutine.
			if ctx.Err() != nil {
				break
			}

			r.wg.Add(1)
			r.mu.Lock()
			r.inflight++
			r.mu.Unlock()

			go func(j *ClaimedJob) {
				defer func() {
					<-sem
					r.wg.Done()
					r.mu.Lock()
					r.inflight--
					r.mu.Unlock()
				}()
				r.executeJob(ctx, j)
			}(job)
		}
	}

	// Graceful shutdown: wait for inflight jobs.
	r.mu.Lock()
	r.stopping = true
	inflight := r.inflight
	r.mu.Unlock()

	if inflight > 0 {
		r.logger.Info("waiting for inflight jobs", "count", inflight, "grace", r.cfg.ShutdownGrace)
		done := make(chan struct{})
		go func() {
			r.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
			r.logger.Info("all inflight jobs completed")
		case <-time.After(r.cfg.ShutdownGrace):
			r.logger.Warn("shutdown grace period expired, some jobs may be retried")
		}
	}

	r.logger.Info("worker runtime stopped")
	return nil
}

// register announces this Worker to the Gateway.
func (r *Runtime) register(ctx context.Context) error {
	regCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	resp, err := r.client.Register(regCtx, &workerv1.RegisterRequest{
		WorkerId:       r.cfg.WorkerID,
		InstanceId:     r.cfg.InstanceID,
		Queues:         r.cfg.Queues,
		SupportedTypes: r.registry.Types(),
		Capacity:       int32(r.cfg.Capacity),
		Version:        r.cfg.Version,
	})
	if err != nil {
		return err
	}

	suggested, suggestedOK := validHeartbeatRecommendation(resp)
	source := "gateway"
	if r.heartbeatIntervalExplicit {
		source = "local"
		if suggestedOK && suggested != r.cfg.HeartbeatInterval {
			r.logger.Info("local heartbeat interval overrides gateway recommendation",
				"heartbeat_interval", r.cfg.HeartbeatInterval,
				"gateway_recommendation", suggested,
			)
		}
	} else if suggestedOK {
		r.cfg.HeartbeatInterval = suggested
	} else {
		r.cfg.HeartbeatInterval = domain.DefaultHeartbeat
		source = "default_fallback"
	}
	r.logger.Info("worker heartbeat interval selected",
		"heartbeat_interval", r.cfg.HeartbeatInterval,
		"source", source,
	)
	return nil
}

func validHeartbeatRecommendation(resp *workerv1.RegisterResponse) (time.Duration, bool) {
	if resp == nil || resp.HeartbeatInterval == nil || resp.HeartbeatInterval.CheckValid() != nil {
		return 0, false
	}
	value := resp.HeartbeatInterval.AsDuration()
	return value, value > 0
}

// poll requests jobs from the Gateway using long-polling.
func (r *Runtime) poll(ctx context.Context) ([]*ClaimedJob, error) {
	r.mu.Lock()
	available := r.cfg.Capacity - r.inflight
	stopping := r.stopping
	r.mu.Unlock()

	if stopping || available <= 0 {
		// No capacity or shutting down; wait briefly.
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
		}
		return nil, nil
	}

	pollCtx, cancel := context.WithTimeout(ctx, r.cfg.PollTimeout)
	defer cancel()

	resp, err := r.client.Poll(pollCtx, &workerv1.PollRequest{
		WorkerId:          r.cfg.WorkerID,
		MaxJobs:           int32(available),
		AvailableCapacity: int32(available),
		Queues:            r.cfg.Queues,
		Types:             r.registry.Types(),
	})
	if err != nil {
		return nil, fmt.Errorf("poll rpc: %w", err)
	}

	jobs := make([]*ClaimedJob, 0, len(resp.Jobs))
	for _, cj := range resp.Jobs {
		job := &ClaimedJob{
			ID:           cj.JobId,
			Queue:        cj.Queue,
			Type:         cj.Type,
			Payload:      cj.Payload,
			Attempt:      int(cj.Attempt),
			MaxAttempts:  int(cj.MaxAttempts),
			FencingToken: cj.FencingToken,
			TraceID:      cj.TraceId,
			TraceContext: cj.TraceContext,
		}
		if cj.LeaseUntil != nil {
			job.LeaseUntil = cj.LeaseUntil.AsTime()
		}
		if cj.Timeout != nil {
			job.Timeout = cj.Timeout.AsDuration()
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

// executeJob runs a single job: handler execution + heartbeat + result reporting.
func (r *Runtime) executeJob(ctx context.Context, job *ClaimedJob) {
	logger := r.logger.With("job_id", job.ID, "type", job.Type, "attempt", job.Attempt, "trace_id", job.TraceID)

	// Restore the submit span context from the job's W3C TraceContext so
	// worker.execute joins the original API trace (FR-503 / AT-12).
	if job.TraceContext != "" {
		ctx = observability.ContextWithTraceParent(ctx, job.TraceContext)
	}

	// worker.execute span (PRD 12.2).
	ctx, span := observability.Tracer("jobforge.worker").Start(ctx, "worker.execute")
	defer span.End()
	span.SetAttributes(
		attribute.String("queue", job.Queue),
		attribute.String("type", job.Type),
		attribute.Int("attempt", job.Attempt),
		attribute.String("worker_id", r.cfg.WorkerID),
	)

	// Look up handler.
	handler := r.registry.Lookup(job.Type)
	if handler == nil {
		logger.Error("no handler registered for type", "type", job.Type)
		span.SetStatus(otelcodes.Error, "no handler for type")
		r.reportFail(ctx, job, "UNKNOWN_TYPE", fmt.Sprintf("no handler for type %q", job.Type), false, 0)
		return
	}

	// Create execution context with timeout.
	timeout := job.Timeout
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	execCtx, execCancel := context.WithTimeout(ctx, timeout)
	defer execCancel()

	// Start heartbeat goroutine.
	var leaseLost atomic.Bool
	cancelSignalAt := make(chan time.Time, 1)
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go r.heartbeatLoop(hbCtx, job, execCancel, &leaseLost, cancelSignalAt)

	// Execute handler.
	start := time.Now()
	resultRef, err := handler.Execute(execCtx, job)
	durationMs := time.Since(start).Milliseconds()

	// Stop heartbeat before reporting result.
	hbCancel()
	if errors.Is(execCtx.Err(), context.Canceled) {
		r.observeCancelHandlerStop(ctx, job.Type, cancelSignalAt)
	}

	// If the heartbeat loop lost the lease, the job may already be
	// redelivered to another Worker. Discard the result: a stale Worker must
	// never overwrite state owned by a new lease; the scheduler redelivers.
	if leaseLost.Load() {
		span.SetStatus(otelcodes.Error, "lease lost during execution")
		span.SetAttributes(attribute.Bool("lease_lost", true))
		logger.Warn("lease lost during execution, discarding result", "duration_ms", durationMs)
		return
	}

	if err != nil {
		retryable := IsRetryable(err)
		errCode := "EXECUTION_ERROR"
		if errors.Is(err, context.DeadlineExceeded) {
			errCode = "TIMEOUT"
			retryable = true
		} else if errors.Is(err, context.Canceled) {
			errCode = "CANCELLED"
			retryable = false
		}
		span.SetStatus(otelcodes.Error, err.Error())
		span.SetAttributes(attribute.String("error_code", errCode))
		logger.Warn("job failed", "error", err, "retryable", retryable, "duration_ms", durationMs)
		r.reportFail(ctx, job, errCode, err.Error(), retryable, durationMs)
		return
	}

	logger.Info("job completed", "duration_ms", durationMs)
	r.reportComplete(ctx, job, resultRef, durationMs)
}

// heartbeatLoop sends periodic heartbeats, retries transient failures with
// exponential backoff, and cancels execution when the lease is lost.
//
// Transient errors (e.g. a Gateway restart or network jitter) do not stop
// renewal: the loop keeps retrying until the lease deadline last reported by
// the Gateway passes. Once the lease is lost (deadline exceeded or a
// STALE_LEASE rejection), execution is cancelled and leaseLost is set so the
// result is discarded — a stale Worker must not overwrite the new lease.
func (r *Runtime) heartbeatLoop(ctx context.Context, job *ClaimedJob, execCancel context.CancelFunc, leaseLost *atomic.Bool, cancelSignalAt chan<- time.Time) {
	// Track the lease deadline as reported by the Gateway (server clock),
	// avoiding local TTL/clock drift estimates.
	leaseUntil := job.LeaseUntil
	if leaseUntil.IsZero() {
		leaseUntil = time.Now().Add(3 * r.cfg.HeartbeatInterval)
	}

	wait := r.cfg.HeartbeatInterval
	backoff := r.hbBackoffInitial

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		hbCtx, cancel := context.WithTimeout(withJobTraceParent(ctx, job), 5*time.Second)
		resp, err := r.client.Heartbeat(hbCtx, &workerv1.HeartbeatRequest{
			JobId:        job.ID,
			WorkerId:     r.cfg.WorkerID,
			FencingToken: job.FencingToken,
		})
		cancel()

		if err == nil {
			// Lease renewed: reset backoff and adopt the server's deadline.
			backoff = r.hbBackoffInitial
			if resp.LeaseUntil != nil {
				leaseUntil = resp.LeaseUntil.AsTime()
			}
			if resp.Signal == workerv1.ControlSignal_CONTROL_SIGNAL_CANCEL {
				if cancelSignalAt != nil {
					select {
					case cancelSignalAt <- time.Now():
					default:
					}
				}
				r.logger.Info("cancel signal received", "job_id", job.ID)
				execCancel()
				return
			}
			wait = r.cfg.HeartbeatInterval
			continue
		}

		// Stable details take precedence: any permanent heartbeat rejection
		// means this process must stop using the lease. Old Gateways without
		// details retain the FailedPrecondition compatibility fallback.
		if isPermanentHeartbeatRejection(err) {
			r.logger.Warn("heartbeat rejected, lease lost", "job_id", job.ID, "error", err)
			r.abandonLease(job, execCancel, leaseLost)
			return
		}

		r.logger.Warn("heartbeat failed, will retry", "job_id", job.ID, "error", err)

		// Give up once the lease deadline has passed: the job may already be
		// redelivered, so continuing only widens the double-execution window.
		remaining := time.Until(leaseUntil)
		if remaining <= 0 {
			r.logger.Warn("lease expired during heartbeat retries", "job_id", job.ID)
			r.abandonLease(job, execCancel, leaseLost)
			return
		}

		wait = backoff
		if remaining < wait {
			wait = remaining
		}
		backoff = min(backoff*2, r.hbBackoffMax)
	}
}

func (r *Runtime) observeCancelHandlerStop(ctx context.Context, jobType string, cancelSignalAt <-chan time.Time) {
	if cancelSignalAt == nil {
		return
	}
	select {
	case receivedAt := <-cancelSignalAt:
		latency := time.Since(receivedAt)
		if latency < 0 {
			latency = 0
		}
		if r.metrics != nil {
			r.metrics.CancelHandlerStopLatencySeconds.Record(ctx, latency.Seconds(),
				metric.WithAttributes(attribute.String("type", jobType)))
		}
		r.logger.Info("handler stopped after cancel signal",
			"type", jobType,
			"latency", latency,
		)
	default:
	}
}

// abandonLease marks the lease as lost and cancels job execution so the
// handler winds down and the double-execution window is minimised.
func (r *Runtime) abandonLease(job *ClaimedJob, execCancel context.CancelFunc, leaseLost *atomic.Bool) {
	leaseLost.Store(true)
	execCancel()
	r.logger.Warn("lease lost, cancelling execution", "job_id", job.ID)
}

// reportComplete sends a Complete RPC to the Gateway, retrying transient
// failures. The Gateway absorbs duplicate Completes for the same lease
// (isIdempotentComplete), so retries are safe.
func (r *Runtime) reportComplete(ctx context.Context, job *ClaimedJob, resultRef string, durationMs int64) {
	ctx = withJobTraceParent(ctx, job)

	err := r.retryRPC(ctx, "complete", func(attemptCtx context.Context) error {
		_, rpcErr := r.client.Complete(attemptCtx, &workerv1.CompleteRequest{
			JobId:        job.ID,
			WorkerId:     r.cfg.WorkerID,
			FencingToken: job.FencingToken,
			ResultRef:    resultRef,
			Duration:     durationpb.New(time.Duration(durationMs) * time.Millisecond),
		})
		return rpcErr
	})
	if err != nil {
		r.logger.Error("complete rpc failed", "job_id", job.ID, "error", err)
	}
}

// reportFail sends a Fail RPC to the Gateway, retrying transient failures.
// The Gateway absorbs duplicate Fails for the same lease (isIdempotentFail),
// so retries are safe.
func (r *Runtime) reportFail(ctx context.Context, job *ClaimedJob, errCode, errMsg string, retryable bool, durationMs int64) {
	ctx = withJobTraceParent(ctx, job)

	err := r.retryRPC(ctx, "fail", func(attemptCtx context.Context) error {
		_, rpcErr := r.client.Fail(attemptCtx, &workerv1.FailRequest{
			JobId:        job.ID,
			WorkerId:     r.cfg.WorkerID,
			FencingToken: job.FencingToken,
			ErrorCode:    errCode,
			ErrorMessage: errMsg,
			Retryable:    retryable,
			Duration:     durationpb.New(time.Duration(durationMs) * time.Millisecond),
		})
		return rpcErr
	})
	if err != nil {
		r.logger.Error("fail rpc failed", "job_id", job.ID, "error", err)
	}
}

// retryRPC runs fn with up to reportAttempts attempts, retrying only
// transient gRPC errors with exponential backoff. Permanent errors (e.g.
// STALE_LEASE) are returned immediately: stale results must never overwrite
// newer state.
func (r *Runtime) retryRPC(ctx context.Context, rpcName string, fn func(attemptCtx context.Context) error) error {
	backoff := r.reportBackoffInitial
	var lastErr error
	for attempt := 1; attempt <= reportAttempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		attemptCtx, cancel := context.WithTimeout(ctx, reportAttemptTimeout)
		lastErr = fn(attemptCtx)
		cancel()
		if lastErr == nil {
			return nil
		}
		if !isTransientRPCError(lastErr) || attempt == reportAttempts {
			return lastErr
		}
		r.logger.Warn("rpc failed, retrying", "rpc", rpcName, "attempt", attempt, "error", lastErr)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return lastErr
		}
		backoff *= 2
	}
	return lastErr
}

// isTransientRPCError reports whether a gRPC error is transient and safe to
// retry. FailedPrecondition (STALE_LEASE / terminal state), NotFound and
// InvalidArgument are permanent and must not be retried.
func isTransientRPCError(err error) bool {
	if detail, ok := workerDomainErrorDetail(err); ok {
		return detail.Retryable
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Unknown, codes.ResourceExhausted:
		return true
	default:
		return false
	}
}

func isPermanentHeartbeatRejection(err error) bool {
	if detail, ok := workerDomainErrorDetail(err); ok {
		return !detail.Retryable
	}
	return status.Code(err) == codes.FailedPrecondition
}

func workerDomainErrorDetail(err error) (*workerv1.DomainErrorDetail, bool) {
	grpcStatus, ok := status.FromError(err)
	if !ok {
		return nil, false
	}
	for _, detail := range grpcStatus.Details() {
		if domainDetail, ok := detail.(*workerv1.DomainErrorDetail); ok {
			return domainDetail, true
		}
	}
	return nil, false
}

// withJobTraceParent attaches the job's W3C traceparent to outgoing gRPC
// metadata so the Gateway can join its spans to the original submit trace
// (FR-503). Returns ctx unchanged when the job carries no trace context.
func withJobTraceParent(ctx context.Context, job *ClaimedJob) context.Context {
	if job.TraceContext == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, observability.TraceParentKey, job.TraceContext)
}
