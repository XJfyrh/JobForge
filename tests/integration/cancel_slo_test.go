package integration

import (
	"context"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"

	gatewaygrpc "github.com/xjfyrh/jobforge/internal/gateway/grpc"
	"github.com/xjfyrh/jobforge/internal/observability"
	"github.com/xjfyrh/jobforge/internal/store"
	"github.com/xjfyrh/jobforge/internal/store/postgres"
	"github.com/xjfyrh/jobforge/internal/worker"
	workerv1 "github.com/xjfyrh/jobforge/proto/jobforge/worker/v1"
)

const cancelSLOTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

type cancelSignalSample struct {
	jobID   string
	latency time.Duration
}

// observingHeartbeatStore captures the database-authored elapsed value for
// AT-24. The production Gateway still records the same value to its histogram;
// this wrapper only preserves raw samples so the test can calculate p95.
type observingHeartbeatStore struct {
	*postgres.JobStore
	signals chan cancelSignalSample
}

func (s *observingHeartbeatStore) Heartbeat(ctx context.Context, jobID, workerID string, fencingToken int64, ttl time.Duration) (*store.HeartbeatResult, error) {
	result, err := s.JobStore.Heartbeat(ctx, jobID, workerID, fencingToken, ttl)
	if err == nil && result.CancelRequested {
		s.signals <- cancelSignalSample{jobID: jobID, latency: result.CancelSignalLatency}
	}
	return result, err
}

type cancelHandlerSample struct {
	jobID string
	at    time.Time
}

type cancelAPIResult struct {
	jobID      string
	returnedAt time.Time
	err        error
}

func startCancelSLOGateway(
	t *testing.T,
	s gatewaygrpc.WorkerStore,
	leaseTTL, heartbeatInterval time.Duration,
	metrics *observability.Metrics,
) string {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := gatewaygrpc.NewWorkerService(s, stubPollWaiter{}, leaseTTL, heartbeatInterval, 0, true, logger, metrics)
	server := grpc.NewServer()
	workerv1.RegisterWorkerServiceServer(server, service)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen cancel SLO gateway: %v", err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	return listener.Addr().String()
}

func TestGatewayRegisterAdvertisesConfiguredHeartbeat(t *testing.T) {
	tests := []struct {
		name       string
		configured time.Duration
		want       time.Duration
	}{
		{name: "default", configured: 5 * time.Second, want: 5 * time.Second},
		{name: "non-default", configured: 2 * time.Second, want: 2 * time.Second},
		{name: "constructor fallback", configured: 0, want: 5 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jobStore := setupStore(t)
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			service := gatewaygrpc.NewWorkerService(jobStore, stubPollWaiter{}, 30*time.Second, tt.configured, 0, true, logger, nil)
			response, err := service.Register(context.Background(), &workerv1.RegisterRequest{
				WorkerId:       "heartbeat-config-" + uuid.New().String()[:8],
				InstanceId:     "heartbeat-config-instance",
				Queues:         []string{"heartbeat-config"},
				SupportedTypes: []string{"demo.echo"},
				Capacity:       1,
				Version:        "m4-test",
			})
			if err != nil {
				t.Fatalf("register: %v", err)
			}
			if response.HeartbeatInterval == nil || response.HeartbeatInterval.AsDuration() != tt.want {
				t.Fatalf("heartbeat recommendation = %v, want %v", response.HeartbeatInterval, tt.want)
			}
		})
	}
}

func TestHeartbeatDBClockResultAndNonDefaultTTL(t *testing.T) {
	ctx := context.Background()
	jobStore := setupStore(t)
	queue := "heartbeat-clock-" + uuid.New().String()[:8]
	workerID := "heartbeat-clock-worker-" + uuid.New().String()[:8]
	job := createTestJob(t, jobStore, queue, "demo.echo")
	claimed, err := claimJobs(ctx, jobStore, store.ClaimParams{
		Queues:   []string{queue},
		WorkerID: workerID,
		MaxJobs:  1,
		LeaseTTL: 7 * time.Second,
	})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: jobs=%d err=%v", len(claimed), err)
	}
	if err := jobStore.Cancel(ctx, "test-tenant", job.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := testEnv.pool.Exec(ctx,
		`update jobs set cancel_requested_at = clock_timestamp() - interval '2 seconds' where id = $1`, job.ID); err != nil {
		t.Fatalf("backdate cancel_requested_at: %v", err)
	}

	result, err := jobStore.Heartbeat(ctx, job.ID, workerID, claimed[0].FencingToken, 7*time.Second)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if !result.CancelRequested {
		t.Fatal("heartbeat did not return the committed cancel state")
	}
	if result.CancelSignalLatency < 1800*time.Millisecond || result.CancelSignalLatency > 3*time.Second {
		t.Fatalf("DB-clock cancel latency = %s, want approximately 2s", result.CancelSignalLatency)
	}
	var leaseRemainingSeconds float64
	if err := testEnv.pool.QueryRow(ctx,
		`select extract(epoch from (lease_until - clock_timestamp()))::double precision from jobs where id = $1`, job.ID,
	).Scan(&leaseRemainingSeconds); err != nil {
		t.Fatalf("read DB lease remaining: %v", err)
	}
	if leaseRemainingSeconds < 6 || leaseRemainingSeconds > 7.2 {
		t.Fatalf("non-default lease remaining = %.3fs, want approximately 7s", leaseRemainingSeconds)
	}
}

func TestGatewayNonDefaultLeaseLivenessThrottle(t *testing.T) {
	ctx := context.Background()
	jobStore := setupStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// A 6s lease produces a 2s liveness threshold. The advertised 500ms job
	// heartbeat cadence must not multiply workers.last_heartbeat_at writes.
	service := gatewaygrpc.NewWorkerService(jobStore, stubPollWaiter{}, 6*time.Second, 500*time.Millisecond, 0, true, logger, nil)
	workerID := "liveness-nondefault-" + uuid.New().String()[:8]
	queue := "liveness-nondefault-" + uuid.New().String()[:8]
	if _, err := service.Register(ctx, &workerv1.RegisterRequest{
		WorkerId:       workerID,
		InstanceId:     "liveness-nondefault-instance",
		Queues:         []string{queue},
		SupportedTypes: []string{"demo.echo"},
		Capacity:       1,
		Version:        "m4-test",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	job := createTestJob(t, jobStore, queue, "demo.echo")
	claimed, err := claimJobs(ctx, jobStore, store.ClaimParams{
		Queues:   []string{queue},
		WorkerID: workerID,
		MaxJobs:  1,
		LeaseTTL: 6 * time.Second,
	})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: jobs=%d err=%v", len(claimed), err)
	}

	if _, err := testEnv.pool.Exec(ctx,
		`update workers set last_heartbeat_at = clock_timestamp() - interval '1 second' where worker_id = $1`, workerID); err != nil {
		t.Fatalf("age worker inside throttle: %v", err)
	}
	insideThrottle := getWorkerLastHeartbeat(t, workerID)
	response, err := service.Heartbeat(ctx, &workerv1.HeartbeatRequest{
		JobId: job.ID, WorkerId: workerID, FencingToken: claimed[0].FencingToken,
	})
	if err != nil {
		t.Fatalf("heartbeat inside liveness throttle: %v", err)
	}
	if response.LeaseUntil == nil {
		t.Fatal("job lease was not renewed")
	}
	if got := getWorkerLastHeartbeat(t, workerID); !got.Equal(insideThrottle) {
		t.Fatalf("500ms job heartbeat unexpectedly rewrote 2s-throttled liveness: %s -> %s", insideThrottle, got)
	}

	if _, err := testEnv.pool.Exec(ctx,
		`update workers set last_heartbeat_at = clock_timestamp() - interval '3 seconds' where worker_id = $1`, workerID); err != nil {
		t.Fatalf("age worker past throttle: %v", err)
	}
	pastThrottle := getWorkerLastHeartbeat(t, workerID)
	if _, err := service.Heartbeat(ctx, &workerv1.HeartbeatRequest{
		JobId: job.ID, WorkerId: workerID, FencingToken: claimed[0].FencingToken,
	}); err != nil {
		t.Fatalf("heartbeat past liveness throttle: %v", err)
	}
	if got := getWorkerLastHeartbeat(t, workerID); !got.After(pastThrottle) {
		t.Fatalf("liveness did not refresh after LeaseTTL/3: %s -> %s", pastThrottle, got)
	}
}

// TestCancelAT24HeartbeatSignalSLO exercises the whole healthy P0 path with
// real PostgreSQL, HTTP cancellation, gRPC Gateway and Worker Runtime. Cancel
// phases are deterministic pseudo-random offsets within one 5s heartbeat
// period. Only the DB-clock signal segment is subject to the 6s p95 SLO;
// API-to-context and context-to-handler-return are reported separately.
func TestCancelAT24HeartbeatSignalSLO(t *testing.T) {
	const (
		sampleCount       = 20
		heartbeatPeriod   = 5 * time.Second
		cancelHandlerType = "test.cancel.slo"
	)

	ctx := context.Background()
	reg := prometheus.NewRegistry()
	metrics, shutdownMetrics, err := observability.SetupMetrics(ctx, reg)
	if err != nil {
		t.Fatalf("setup metrics: %v", err)
	}
	t.Cleanup(func() { _ = shutdownMetrics(context.Background()) })

	spanRecorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))
	previousTracerProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tracerProvider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = tracerProvider.Shutdown(context.Background())
		otel.SetTracerProvider(previousTracerProvider)
		otel.SetTextMapPropagator(previousPropagator)
	})

	_, cancelClient, jobStore := setupCtlServer(t)
	signalSamples := make(chan cancelSignalSample, sampleCount)
	observedStore := &observingHeartbeatStore{JobStore: jobStore, signals: signalSamples}
	gatewayAddr := startCancelSLOGateway(t, observedStore, 30*time.Second, heartbeatPeriod, metrics)

	queue := "cancel-slo-" + uuid.New().String()[:8]
	started := make(chan cancelHandlerSample, sampleCount)
	contextCancelled := make(chan cancelHandlerSample, sampleCount)
	handlerReturned := make(chan cancelHandlerSample, sampleCount)
	registry := worker.NewRegistry()
	registry.Register(cancelHandlerType, worker.HandlerFunc(func(handlerCtx context.Context, job *worker.ClaimedJob) (string, error) {
		started <- cancelHandlerSample{jobID: job.ID, at: time.Now()}
		<-handlerCtx.Done()
		contextCancelled <- cancelHandlerSample{jobID: job.ID, at: time.Now()}
		// Model a small, deterministic cleanup section so the independently
		// reported handler-stop segment is observable rather than clock-zero.
		time.Sleep(2 * time.Millisecond)
		handlerReturned <- cancelHandlerSample{jobID: job.ID, at: time.Now()}
		return "", handlerCtx.Err()
	}))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runtime := worker.NewRuntime(worker.RuntimeConfig{
		WorkerID:          "cancel-slo-worker-" + uuid.New().String()[:8],
		InstanceID:        "cancel-slo-instance",
		Queues:            []string{queue},
		Capacity:          sampleCount,
		GatewayAddr:       gatewayAddr,
		HeartbeatInterval: 0, // adopt the Gateway's 5s RegisterResponse value
		PollTimeout:       250 * time.Millisecond,
		ShutdownGrace:     10 * time.Second,
		Version:           "m4-at24",
	}, registry, logger, metrics)

	phaseByJob := make(map[string]time.Duration, sampleCount)
	random := rand.New(rand.NewSource(24)) // deterministic, reproducible phases
	for i := 0; i < sampleCount; i++ {
		job := createTestJob(t, jobStore, queue, cancelHandlerType)
		if _, err := testEnv.pool.Exec(ctx,
			`update jobs set trace_context = $2 where id = $1`, job.ID, cancelSLOTraceparent); err != nil {
			t.Fatalf("set test traceparent: %v", err)
		}
		phaseByJob[job.ID] = time.Duration(random.Int63n(int64(heartbeatPeriod)))
	}

	runtimeCtx, stopRuntime := context.WithCancel(context.Background())
	runtimeDone := make(chan error, 1)
	runtimeExited := make(chan struct{})
	go func() {
		runtimeDone <- runtime.Run(runtimeCtx)
		close(runtimeExited)
	}()
	t.Cleanup(func() {
		stopRuntime()
		select {
		case <-runtimeExited:
		case <-time.After(15 * time.Second):
		}
	})

	cancelResults := make(chan cancelAPIResult, sampleCount)
	startDeadline := time.NewTimer(15 * time.Second)
	defer startDeadline.Stop()
	for i := 0; i < sampleCount; i++ {
		var sample cancelHandlerSample
		select {
		case sample = <-started:
		case <-startDeadline.C:
			t.Fatalf("only %d/%d handlers started before timeout", i, sampleCount)
		}
		phase := phaseByJob[sample.jobID]
		go func(jobID string, delay time.Duration) {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			<-timer.C
			cancelCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := cancelClient.Cancel(cancelCtx, jobID)
			cancelResults <- cancelAPIResult{jobID: jobID, returnedAt: time.Now(), err: err}
		}(sample.jobID, phase)
	}

	apiReturnedAt := make(map[string]time.Time, sampleCount)
	for i := 0; i < sampleCount; i++ {
		select {
		case result := <-cancelResults:
			if result.err != nil {
				t.Fatalf("cancel %s: %v", result.jobID, result.err)
			}
			apiReturnedAt[result.jobID] = result.returnedAt
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d/%d Cancel API calls returned", i, sampleCount)
		}
	}

	contextTimes := receiveCancelHandlerSamples(t, contextCancelled, sampleCount, 10*time.Second)
	returnTimes := receiveCancelHandlerSamples(t, handlerReturned, sampleCount, 10*time.Second)
	dbSignalDurations := make([]time.Duration, 0, sampleCount)
	seenSignals := make(map[string]struct{}, sampleCount)
	for i := 0; i < sampleCount; i++ {
		select {
		case sample := <-signalSamples:
			if _, duplicate := seenSignals[sample.jobID]; duplicate {
				t.Fatalf("duplicate cancel signal sample for %s", sample.jobID)
			}
			seenSignals[sample.jobID] = struct{}{}
			dbSignalDurations = append(dbSignalDurations, sample.latency)
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d/%d DB-clock signal samples observed", i, sampleCount)
		}
	}

	apiToContext := make([]time.Duration, 0, sampleCount)
	contextToReturn := make([]time.Duration, 0, sampleCount)
	for jobID, cancelledAt := range contextTimes {
		elapsed := cancelledAt.Sub(apiReturnedAt[jobID])
		if elapsed < 0 {
			// Heartbeat may observe the committed cancel just before the HTTP
			// response is written. Treat that overlap as zero for this report.
			elapsed = 0
		}
		apiToContext = append(apiToContext, elapsed)
		contextToReturn = append(contextToReturn, returnTimes[jobID].Sub(cancelledAt))
	}

	signalP50, signalP95, signalMax := durationSummary(dbSignalDurations)
	apiP50, apiP95, apiMax := durationSummary(apiToContext)
	stopP50, stopP95, stopMax := durationSummary(contextToReturn)
	t.Logf("AT-24 DB cancel_requested_at->Gateway signal: n=%d p50=%s p95=%s max=%s", sampleCount, signalP50, signalP95, signalMax)
	t.Logf("AT-24 Cancel API return->Worker context cancelled (report only): n=%d p50=%s p95=%s max=%s", sampleCount, apiP50, apiP95, apiMax)
	t.Logf("AT-24 Worker context cancelled->Handler return (report only): n=%d p50=%s p95=%s max=%s", sampleCount, stopP50, stopP95, stopMax)
	if signalP95 > 6*time.Second {
		t.Fatalf("DB-clock cancel signal p95 = %s, want <= 6s", signalP95)
	}

	if got := histogramSampleCount(t, reg, "jobforge_cancel_signal_latency_seconds", map[string]string{"path": "heartbeat"}); got != sampleCount {
		t.Fatalf("cancel signal histogram count = %d, want %d", got, sampleCount)
	}
	if got := histogramSampleCount(t, reg, "jobforge_cancel_handler_stop_latency_seconds", map[string]string{"type": cancelHandlerType}); got != sampleCount {
		t.Fatalf("handler stop histogram count = %d, want %d", got, sampleCount)
	}

	cancelSpans := 0
	for _, span := range spanRecorder.Ended() {
		if span.Name() != "gateway.cancel_signal" {
			continue
		}
		cancelSpans++
		if got := span.SpanContext().TraceID().String(); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
			t.Fatalf("cancel signal trace ID = %s", got)
		}
		if got := span.Parent().SpanID().String(); got != "00f067aa0ba902b7" {
			t.Fatalf("cancel signal parent span ID = %s", got)
		}
		for _, attr := range span.Attributes() {
			key := strings.ToLower(string(attr.Key))
			if key != "path" && key != "cancel.signal_latency_seconds" {
				t.Fatalf("unexpected attribute on gateway.cancel_signal: %s", attr.Key)
			}
			if strings.Contains(key, "payload") || strings.Contains(key, "credential") ||
				strings.Contains(key, "authorization") || strings.Contains(key, "api_key") {
				t.Fatalf("sensitive attribute key on gateway.cancel_signal: %s", attr.Key)
			}
		}
	}
	if cancelSpans != sampleCount {
		t.Fatalf("gateway.cancel_signal spans = %d, want %d", cancelSpans, sampleCount)
	}

	for jobID := range phaseByJob {
		waitForJobState(t, jobStore, jobID, "cancelled", 10*time.Second)
	}
	stopRuntime()
	select {
	case err := <-runtimeDone:
		if err != nil {
			t.Fatalf("worker runtime: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("worker runtime did not stop")
	}
}

func receiveCancelHandlerSamples(t *testing.T, ch <-chan cancelHandlerSample, count int, timeout time.Duration) map[string]time.Time {
	t.Helper()
	result := make(map[string]time.Time, count)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for len(result) < count {
		select {
		case sample := <-ch:
			if _, duplicate := result[sample.jobID]; duplicate {
				t.Fatalf("duplicate handler sample for %s", sample.jobID)
			}
			result[sample.jobID] = sample.at
		case <-deadline.C:
			t.Fatalf("only %d/%d handler samples observed", len(result), count)
		}
	}
	return result
}

func durationSummary(values []time.Duration) (p50, p95, maxValue time.Duration) {
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	percentile := func(percent int) time.Duration {
		index := (percent*len(sorted)+99)/100 - 1
		if index < 0 {
			index = 0
		}
		return sorted[index]
	}
	return percentile(50), percentile(95), sorted[len(sorted)-1]
}

func histogramSampleCount(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) uint64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather Prometheus metrics: %v", err)
	}
	available := make([]string, 0, len(families))
	var matchingLabels []map[string]string
	for _, family := range families {
		available = append(available, family.GetName())
		if family.GetName() != name {
			continue
		}
		for _, sample := range family.GetMetric() {
			gotLabels := make(map[string]string, len(sample.GetLabel()))
			for _, label := range sample.GetLabel() {
				gotLabels[label.GetName()] = label.GetValue()
			}
			matchingLabels = append(matchingLabels, gotLabels)
			if metricLabelsMatch(gotLabels, labels) && sample.GetHistogram() != nil {
				return sample.GetHistogram().GetSampleCount()
			}
		}
	}
	t.Fatalf("histogram %s with labels %v not found; matching labels=%v gathered=%v", name, labels, matchingLabels, available)
	return 0
}

func metricLabelsMatch(got, expected map[string]string) bool {
	for key, value := range expected {
		if got[key] != value {
			return false
		}
	}
	for key := range got {
		if _, expectedLabel := expected[key]; !expectedLabel && !strings.HasPrefix(key, "otel_scope_") {
			return false
		}
	}
	return true
}
