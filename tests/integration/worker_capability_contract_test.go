package integration

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xjfyrh/jobforge/internal/domain"
	gatewaygrpc "github.com/xjfyrh/jobforge/internal/gateway/grpc"
	"github.com/xjfyrh/jobforge/internal/observability"
	"github.com/xjfyrh/jobforge/internal/store/postgres"
	workerv1 "github.com/xjfyrh/jobforge/proto/jobforge/worker/v1"
)

func newWorkerContractService(
	t *testing.T,
) (*gatewaygrpc.WorkerService, *postgres.JobStore) {
	t.Helper()
	jobStore := setupStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := gatewaygrpc.NewWorkerService(
		jobStore,
		blockingWaiter{},
		testTaskTypeCatalog(t),
		30*time.Second,
		5*time.Second,
		0,
		true,
		logger,
		nil,
	)
	return service, jobStore
}

// TestAT29RegisterCapabilityValidation verifies every persisted capability
// field before the workers upsert and proves an invalid re-registration cannot
// partially overwrite an existing row (PRD v0.5 AT-29).
func TestAT29RegisterCapabilityValidation(t *testing.T) {
	service, _ := newWorkerContractService(t)
	ctx := context.Background()

	tests := []struct {
		name   string
		mutate func(*workerv1.RegisterRequest)
	}{
		{name: "missing worker id", mutate: func(req *workerv1.RegisterRequest) { req.WorkerId = "" }},
		{name: "zero capacity", mutate: func(req *workerv1.RegisterRequest) { req.Capacity = 0 }},
		{name: "missing queues", mutate: func(req *workerv1.RegisterRequest) { req.Queues = nil }},
		{name: "empty queue", mutate: func(req *workerv1.RegisterRequest) { req.Queues = []string{"default", ""} }},
		{name: "duplicate queue", mutate: func(req *workerv1.RegisterRequest) { req.Queues = []string{"default", "default"} }},
		{name: "missing types", mutate: func(req *workerv1.RegisterRequest) { req.SupportedTypes = nil }},
		{name: "empty type", mutate: func(req *workerv1.RegisterRequest) { req.SupportedTypes = []string{"demo.echo", ""} }},
		{name: "duplicate type", mutate: func(req *workerv1.RegisterRequest) { req.SupportedTypes = []string{"demo.echo", "demo.echo"} }},
		{name: "type outside catalog", mutate: func(req *workerv1.RegisterRequest) { req.SupportedTypes = []string{"demo.unknown"} }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workerID := "at29-invalid-" + uuid.New().String()[:8]
			req := &workerv1.RegisterRequest{
				WorkerId:       workerID,
				InstanceId:     "at29-instance",
				Queues:         []string{"default"},
				SupportedTypes: []string{"demo.echo"},
				Capacity:       2,
				Version:        "at29",
			}
			tt.mutate(req)
			if _, err := service.Register(ctx, req); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("register status = %s, want InvalidArgument: %v", status.Code(err), err)
			}

			if workerID == "" {
				return
			}
			var count int
			if err := testEnv.pool.QueryRow(ctx,
				`select count(*) from workers where worker_id = $1`, workerID,
			).Scan(&count); err != nil {
				t.Fatalf("count worker rows: %v", err)
			}
			if count != 0 {
				t.Fatalf("invalid Register wrote %d worker rows", count)
			}
		})
	}

	workerID := "at29-existing-" + uuid.New().String()[:8]
	valid := &workerv1.RegisterRequest{
		WorkerId:       workerID,
		InstanceId:     "at29-valid-instance",
		Queues:         []string{"queue-a", "queue-b"},
		SupportedTypes: []string{"demo.echo", "demo.sleep"},
		Capacity:       3,
		Version:        "at29-valid",
	}
	if _, err := service.Register(ctx, valid); err != nil {
		t.Fatalf("valid register: %v", err)
	}

	type workerSnapshot struct {
		instanceID string
		types      []string
		queues     []string
		capacity   int32
		version    string
		sessionID  string
		status     string
	}
	readSnapshot := func() workerSnapshot {
		t.Helper()
		var snapshot workerSnapshot
		if err := testEnv.pool.QueryRow(ctx, `
			select instance_id, supported_types, queues, capacity, version, session_id, status
			from workers where worker_id = $1`, workerID,
		).Scan(
			&snapshot.instanceID,
			&snapshot.types,
			&snapshot.queues,
			&snapshot.capacity,
			&snapshot.version,
			&snapshot.sessionID,
			&snapshot.status,
		); err != nil {
			t.Fatalf("read worker snapshot: %v", err)
		}
		return snapshot
	}

	before := readSnapshot()
	invalidOverwrite := &workerv1.RegisterRequest{
		WorkerId:       workerID,
		InstanceId:     "must-not-persist",
		Queues:         []string{"queue-c"},
		SupportedTypes: []string{"demo.unknown"},
		Capacity:       99,
		Version:        "must-not-persist",
	}
	if _, err := service.Register(ctx, invalidOverwrite); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid overwrite status = %s: %v", status.Code(err), err)
	}
	after := readSnapshot()
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("invalid Register changed worker row:\nbefore=%+v\nafter=%+v", before, after)
	}
}

// TestAT30PollCapabilitiesAndConcurrentCapacity verifies that Poll cannot
// exceed registered queue/type/capacity and that eight concurrent calls leave
// at most two inflight leases for a capacity=2 Worker (PRD v0.5 AT-30).
func TestAT30PollCapabilitiesAndConcurrentCapacity(t *testing.T) {
	service, jobStore := newWorkerContractService(t)
	ctx := context.Background()
	queue := "at30-capacity-" + uuid.New().String()[:8]
	workerID := "at30-worker-" + uuid.New().String()[:8]

	for range 10 {
		_ = createTestJob(t, jobStore, queue, "demo.echo")
	}

	validPoll := func(id string) *workerv1.PollRequest {
		return &workerv1.PollRequest{
			WorkerId:          id,
			MaxJobs:           1,
			AvailableCapacity: 1,
			Queues:            []string{queue},
			Types:             []string{"demo.echo"},
		}
	}

	if _, err := service.Poll(ctx, validPoll("at30-unregistered-"+uuid.New().String()[:8])); status.Code(err) != codes.NotFound {
		t.Fatalf("unregistered Poll status = %s, want NotFound: %v", status.Code(err), err)
	}
	if _, err := service.Register(ctx, &workerv1.RegisterRequest{
		WorkerId:       workerID,
		InstanceId:     "at30-instance",
		Queues:         []string{queue, queue + "-secondary"},
		SupportedTypes: []string{"demo.echo", "demo.sleep"},
		Capacity:       2,
		Version:        "at30",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	backdateWorkerHeartbeat(t, workerID, "1 hour")
	rejectedHeartbeat := getWorkerLastHeartbeat(t, workerID)

	queueMismatch := validPoll(workerID)
	queueMismatch.Queues = []string{queue + "-forbidden"}
	if _, err := service.Poll(ctx, queueMismatch); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("queue mismatch status = %s: %v", status.Code(err), err)
	}
	typeMismatch := validPoll(workerID)
	typeMismatch.Types = []string{"demo.fail"}
	if _, err := service.Poll(ctx, typeMismatch); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("type mismatch status = %s: %v", status.Code(err), err)
	}
	for _, capacityMismatch := range []*workerv1.PollRequest{
		{
			WorkerId: workerID, MaxJobs: 3, AvailableCapacity: 1,
			Queues: []string{queue}, Types: []string{"demo.echo"},
		},
		{
			WorkerId: workerID, MaxJobs: 1, AvailableCapacity: 3,
			Queues: []string{queue}, Types: []string{"demo.echo"},
		},
	} {
		if _, err := service.Poll(ctx, capacityMismatch); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("capacity mismatch status = %s: %v", status.Code(err), err)
		}
	}
	for _, malformed := range []*workerv1.PollRequest{
		{
			WorkerId: workerID, MaxJobs: 1, AvailableCapacity: 1,
			Queues: []string{queue, queue}, Types: []string{"demo.echo"},
		},
		{
			WorkerId: workerID, MaxJobs: 1, AvailableCapacity: 1,
			Queues: []string{queue}, Types: []string{"demo.echo", "demo.echo"},
		},
	} {
		if _, err := service.Poll(ctx, malformed); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("malformed Poll status = %s: %v", status.Code(err), err)
		}
	}
	if heartbeat := getWorkerLastHeartbeat(t, workerID); !heartbeat.Equal(rejectedHeartbeat) {
		t.Fatalf("rejected Poll changed liveness: %s -> %s", rejectedHeartbeat, heartbeat)
	}

	type pollResult struct {
		response *workerv1.PollResponse
		err      error
	}
	results := make(chan pollResult, 8)
	var waitGroup sync.WaitGroup
	for range 8 {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			pollCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			response, err := service.Poll(pollCtx, validPoll(workerID))
			results <- pollResult{response: response, err: err}
		}()
	}
	waitGroup.Wait()
	close(results)

	claimed := make([]*workerv1.ClaimedJob, 0, 2)
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent Poll: %v", result.err)
		}
		claimed = append(claimed, result.response.Jobs...)
	}
	if len(claimed) != 2 {
		t.Fatalf("concurrent Poll claimed %d jobs, want capacity 2", len(claimed))
	}

	var inflight int
	if err := testEnv.pool.QueryRow(ctx, `
		select count(*) from jobs
		where lease_owner = $1 and state in ('running', 'cancelling')`, workerID,
	).Scan(&inflight); err != nil {
		t.Fatalf("count inflight: %v", err)
	}
	if inflight != 2 {
		t.Fatalf("worker inflight = %d, want 2", inflight)
	}

	if err := jobStore.Complete(ctx, claimed[0].JobId, workerID, claimed[0].FencingToken, "at30-release", 1); err != nil {
		t.Fatalf("release capacity: %v", err)
	}
	replacement, err := service.Poll(ctx, validPoll(workerID))
	if err != nil {
		t.Fatalf("Poll after capacity release: %v", err)
	}
	if len(replacement.Jobs) != 1 {
		t.Fatalf("Poll after release returned %d jobs, want 1", len(replacement.Jobs))
	}
}

// TestAT30PollAndReregisterSerialize holds the workers row, queues a
// re-registration, then queues a Poll behind it. PostgreSQL lock-wait state is
// the synchronization barrier: after release, Poll must observe the new
// registration and reject the old capability rather than claim across it.
func TestAT30PollAndReregisterSerialize(t *testing.T) {
	service, jobStore := newWorkerContractService(t)
	ctx := context.Background()
	workerID := "at30-reregister-" + uuid.New().String()[:8]
	oldQueue := "at30-old-" + uuid.New().String()[:8]
	newQueue := "at30-new-" + uuid.New().String()[:8]
	oldJob := createTestJob(t, jobStore, oldQueue, "demo.echo")

	if _, err := service.Register(ctx, &workerv1.RegisterRequest{
		WorkerId:       workerID,
		InstanceId:     "at30-old-instance",
		Queues:         []string{oldQueue},
		SupportedTypes: []string{"demo.echo"},
		Capacity:       1,
		Version:        "at30-old",
	}); err != nil {
		t.Fatalf("initial register: %v", err)
	}

	locker, err := testEnv.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire row locker: %v", err)
	}
	defer locker.Release()
	tx, err := locker.Begin(ctx)
	if err != nil {
		t.Fatalf("begin row locker: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`select worker_id from workers where worker_id = $1 for update`, workerID,
	); err != nil {
		t.Fatalf("lock worker row: %v", err)
	}

	registerDone := make(chan error, 1)
	go func() {
		_, registerErr := service.Register(ctx, &workerv1.RegisterRequest{
			WorkerId:       workerID,
			InstanceId:     "at30-new-instance",
			Queues:         []string{newQueue},
			SupportedTypes: []string{"demo.sleep"},
			Capacity:       1,
			Version:        "at30-new",
		})
		registerDone <- registerErr
	}()
	waitForLockWait(t, "insert into workers")

	type pollOutcome struct {
		response *workerv1.PollResponse
		err      error
	}
	pollDone := make(chan pollOutcome, 1)
	go func() {
		pollCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		response, pollErr := service.Poll(pollCtx, &workerv1.PollRequest{
			WorkerId:          workerID,
			MaxJobs:           1,
			AvailableCapacity: 1,
			Queues:            []string{oldQueue},
			Types:             []string{"demo.echo"},
		})
		pollDone <- pollOutcome{response: response, err: pollErr}
	}()
	waitForLockWait(t, "select supported_types, queues, capacity, status")

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("release worker row: %v", err)
	}
	select {
	case err := <-registerDone:
		if err != nil {
			t.Fatalf("re-register: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("re-register did not finish")
	}
	select {
	case outcome := <-pollDone:
		if status.Code(outcome.err) != codes.PermissionDenied {
			t.Fatalf("old capability Poll = response %+v, status %s: %v",
				outcome.response, status.Code(outcome.err), outcome.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Poll did not finish")
	}

	storedOldJob, err := jobStore.GetByID(ctx, "test-tenant", oldJob.ID)
	if err != nil {
		t.Fatalf("get old job: %v", err)
	}
	if storedOldJob.State != domain.StateReady || storedOldJob.LeaseOwner != nil {
		t.Fatalf("old capability claimed a job after re-register: state=%s owner=%v",
			storedOldJob.State, storedOldJob.LeaseOwner)
	}

	newJob := createTestJob(t, jobStore, newQueue, "demo.sleep")
	response, err := service.Poll(ctx, &workerv1.PollRequest{
		WorkerId:          workerID,
		MaxJobs:           1,
		AvailableCapacity: 1,
		Queues:            []string{newQueue},
		Types:             []string{"demo.sleep"},
	})
	if err != nil {
		t.Fatalf("Poll with new registration: %v", err)
	}
	if len(response.Jobs) != 1 || response.Jobs[0].JobId != newJob.ID {
		t.Fatalf("new registration Poll returned %+v, want %s", response.Jobs, newJob.ID)
	}
}

func waitForLockWait(t *testing.T, queryFragment string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		err := testEnv.pool.QueryRow(ctx, `
			select exists (
			    select 1
			    from pg_stat_activity
			    where pid <> pg_backend_pid()
			      and wait_event_type = 'Lock'
			      and query ilike '%' || $1 || '%'
			)`, queryFragment,
		).Scan(&waiting)
		if err != nil {
			t.Fatalf("observe PostgreSQL lock wait for %q: %v", queryFragment, err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("query %q did not enter PostgreSQL lock wait", queryFragment)
		case <-ticker.C:
		}
	}
}

func TestWorkerContractRejectionMetrics(t *testing.T) {
	ctx := context.Background()
	registry := prometheus.NewRegistry()
	metrics, shutdown, err := observability.SetupMetrics(ctx, registry)
	if err != nil {
		t.Fatalf("setup metrics: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	jobStore := setupStore(t)
	service := gatewaygrpc.NewWorkerService(
		jobStore,
		blockingWaiter{},
		testTaskTypeCatalog(t),
		30*time.Second,
		5*time.Second,
		0,
		true,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		metrics,
	)
	workerID := "contract-metrics-" + uuid.New().String()[:8]
	registerRequest := func(id string) *workerv1.RegisterRequest {
		return &workerv1.RegisterRequest{
			WorkerId:       id,
			InstanceId:     "contract-metrics-instance",
			Queues:         []string{"contract-metrics"},
			SupportedTypes: []string{"demo.echo"},
			Capacity:       1,
			Version:        "contract-metrics",
		}
	}

	duplicate := registerRequest(workerID + "-duplicate")
	duplicate.Queues = []string{"contract-metrics", "contract-metrics"}
	_, _ = service.Register(ctx, duplicate)
	unknown := registerRequest(workerID + "-unknown")
	unknown.SupportedTypes = []string{"demo.unknown"}
	_, _ = service.Register(ctx, unknown)
	if _, err := service.Register(ctx, registerRequest(workerID)); err != nil {
		t.Fatalf("valid register: %v", err)
	}

	pollRequest := func(id string) *workerv1.PollRequest {
		return &workerv1.PollRequest{
			WorkerId:          id,
			MaxJobs:           1,
			AvailableCapacity: 1,
			Queues:            []string{"contract-metrics"},
			Types:             []string{"demo.echo"},
		}
	}
	queueMismatch := pollRequest(workerID)
	queueMismatch.Queues = []string{"contract-metrics-forbidden"}
	_, _ = service.Poll(ctx, queueMismatch)
	capacityMismatch := pollRequest(workerID)
	capacityMismatch.MaxJobs = 2
	_, _ = service.Poll(ctx, capacityMismatch)
	_, _ = service.Poll(ctx, pollRequest(workerID+"-missing"))
	unknownPoll := pollRequest(workerID)
	unknownPoll.Types = []string{"demo.unknown"}
	_, _ = service.Poll(ctx, unknownPoll)

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	got := make(map[string]float64)
	for _, family := range families {
		if family.GetName() != "jobforge_contract_rejections_total" {
			continue
		}
		for _, sample := range family.GetMetric() {
			labels := make(map[string]string)
			for _, label := range sample.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			got[labels["surface"]+"/"+labels["reason"]] = sample.GetCounter().GetValue()
		}
	}
	want := map[string]float64{
		"register/malformed_capability": 1,
		"register/unknown_type":         1,
		"poll/capability_mismatch":      1,
		"poll/capacity_exceeded":        1,
		"poll/unregistered_worker":      1,
		"poll/unknown_type":             1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("contract rejection metrics = %v, want %v", got, want)
	}
}
