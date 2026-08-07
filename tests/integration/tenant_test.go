package integration

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/xjfyrh/jobforge/internal/domain"
	gatewaygrpc "github.com/xjfyrh/jobforge/internal/gateway/grpc"
	"github.com/xjfyrh/jobforge/internal/store"
	workerv1 "github.com/xjfyrh/jobforge/proto/jobforge/worker/v1"
)

// TestTenantAT10Isolation verifies AT-10: Tenant isolation.
//
// AT-10 Requirements:
//   - Tenant A fills its concurrent quota
//   - Tenant B submits a task
//   - Tenant B can still execute within its own quota (not affected by A)
//
// FR-302: Tenant concurrent limit - when a tenant reaches inflight limit,
// new claims for that tenant's jobs are paused.
func TestTenantAT10Isolation(t *testing.T) {
	js := setupStore(t)
	ctx := context.Background()

	// Use a low tenant limit for testing.
	tenantMaxInflight := 2

	// 1. Tenant A submits 3 jobs.
	tenantA := "tenant-A-" + uuid.New().String()[:8]
	queueA := "isolation-queue-A"

	var jobsA []*domain.Job
	for i := 0; i < 3; i++ {
		job := createTestJobForTenant(t, js, tenantA, queueA, "demo.echo")
		jobsA = append(jobsA, job)
	}

	// 2. Tenant A claims 2 jobs (fills quota).
	claimedA, err := js.Claim(ctx, store.ClaimParams{
		Queues:            []string{queueA},
		WorkerID:          "worker-A",
		MaxJobs:           10,
		LeaseTTL:          30 * time.Second,
		TenantMaxInflight: tenantMaxInflight,
	})
	if err != nil {
		t.Fatalf("tenant A claim: %v", err)
	}

	// Should only get 2 jobs (limited by tenant quota).
	if len(claimedA) != tenantMaxInflight {
		t.Fatalf("expected tenant A to claim %d jobs (quota), got %d", tenantMaxInflight, len(claimedA))
	}
	t.Logf("Tenant A claimed %d jobs (quota = %d)", len(claimedA), tenantMaxInflight)

	// 3. Tenant A tries to claim more - should get 0 (quota full).
	claimedAMore, err := js.Claim(ctx, store.ClaimParams{
		Queues:            []string{queueA},
		WorkerID:          "worker-A-2",
		MaxJobs:           10,
		LeaseTTL:          30 * time.Second,
		TenantMaxInflight: tenantMaxInflight,
	})
	if err != nil {
		t.Fatalf("tenant A second claim: %v", err)
	}

	if len(claimedAMore) != 0 {
		t.Errorf("expected tenant A to claim 0 more jobs (quota full), got %d", len(claimedAMore))
	} else {
		t.Log("Tenant A quota full: cannot claim more jobs")
	}

	// 4. Tenant B submits 1 job (different tenant, same queue).
	tenantB := "tenant-B-" + uuid.New().String()[:8]
	jobB := createTestJobForTenant(t, js, tenantB, queueA, "demo.echo")

	// 5. Tenant B can claim (not affected by tenant A's quota).
	claimedB, err := js.Claim(ctx, store.ClaimParams{
		Queues:            []string{queueA},
		WorkerID:          "worker-B",
		MaxJobs:           10,
		LeaseTTL:          30 * time.Second,
		TenantMaxInflight: tenantMaxInflight,
	})
	if err != nil {
		t.Fatalf("tenant B claim: %v", err)
	}

	// Tenant B should get its job.
	foundB := false
	for _, j := range claimedB {
		if j.ID == jobB.ID {
			foundB = true
			break
		}
	}

	if !foundB {
		t.Error("AT-10 FAILED: tenant B could not claim its job while tenant A quota was full")
	} else {
		t.Log("AT-10 PASSED: tenant B can claim within its own quota, isolated from tenant A")
	}

	// 6. Verify tenant A's third job is still ready (not claimed).
	gotA3, err := js.GetByID(ctx, tenantA, jobsA[2].ID)
	if err != nil {
		t.Fatalf("get tenant A job 3: %v", err)
	}
	if gotA3.State != domain.StateReady {
		t.Errorf("expected tenant A job 3 to still be ready, got %s", gotA3.State)
	}
}

// TestTenantQuotaRelease verifies that when a tenant's running jobs complete,
// the quota is freed and new jobs can be claimed.
func TestTenantQuotaRelease(t *testing.T) {
	js := setupStore(t)
	ctx := context.Background()

	tenantMaxInflight := 1
	tenant := "tenant-release-" + uuid.New().String()[:8]
	queue := "quota-release-queue"

	// Submit 2 jobs.
	job1 := createTestJobForTenant(t, js, tenant, queue, "demo.echo")
	job2 := createTestJobForTenant(t, js, tenant, queue, "demo.echo")

	// Claim 1 job (fills quota).
	claimed, err := js.Claim(ctx, store.ClaimParams{
		Queues:            []string{queue},
		WorkerID:          "worker-1",
		MaxJobs:           10,
		LeaseTTL:          30 * time.Second,
		TenantMaxInflight: tenantMaxInflight,
	})
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("expected 1 claimed, got %d", len(claimed))
	}

	// Complete the job (frees quota).
	err = js.Complete(ctx, claimed[0].ID, "worker-1", claimed[0].FencingToken, "", 100)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Now the second job can be claimed.
	claimed2, err := js.Claim(ctx, store.ClaimParams{
		Queues:            []string{queue},
		WorkerID:          "worker-2",
		MaxJobs:           10,
		LeaseTTL:          30 * time.Second,
		TenantMaxInflight: tenantMaxInflight,
	})
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}

	found := false
	for _, j := range claimed2 {
		if j.ID == job2.ID {
			found = true
			break
		}
	}

	if !found {
		t.Error("expected job2 to be claimable after quota release")
	} else {
		t.Log("Quota release works: job2 claimable after job1 completed")
	}

	_ = job1 // used above
}

// blockingWaiter is a PollWaiter stub that blocks until ctx expires and
// never reports a notification.
type blockingWaiter struct{}

// WaitForNotification implements gatewaygrpc.PollWaiter.
func (blockingWaiter) WaitForNotification(ctx context.Context) bool {
	<-ctx.Done()
	return false
}

// TestTenantQuotaViaGatewayPoll verifies FR-302 through the Gateway Poll RPC
// path: the tenant quota configured on the WorkerService must be enforced
// during claim. This is a wiring regression test (the store-level quota check
// alone does not prove the Gateway passes TenantMaxInflight).
func TestTenantQuotaViaGatewayPoll(t *testing.T) {
	js := setupStore(t)
	ctx := context.Background()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := gatewaygrpc.NewWorkerService(js, blockingWaiter{}, 30*time.Second, 1, logger, nil)

	tenantA := "tenant-gw-A-" + uuid.New().String()[:8]
	tenantB := "tenant-gw-B-" + uuid.New().String()[:8]
	queue := "gw-quota-queue"

	// Tenant A submits 2 jobs; the WorkerService quota is 1.
	_ = createTestJobForTenant(t, js, tenantA, queue, "demo.echo")
	jobA2 := createTestJobForTenant(t, js, tenantA, queue, "demo.echo")

	// 1. First Poll claims exactly 1 job (tenant quota).
	resp, err := svc.Poll(ctx, &workerv1.PollRequest{
		WorkerId: "gw-poll-worker",
		MaxJobs:  5,
		Queues:   []string{queue},
	})
	if err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if len(resp.Jobs) != 1 {
		t.Fatalf("expected 1 claimed job (tenant quota), got %d", len(resp.Jobs))
	}

	// 2. Quota full: a second Poll must not claim tenant A's remaining job
	// before the short deadline expires.
	pollCtx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()
	resp2, err := svc.Poll(pollCtx, &workerv1.PollRequest{
		WorkerId: "gw-poll-worker-2",
		MaxJobs:  5,
		Queues:   []string{queue},
	})
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if len(resp2.Jobs) != 0 {
		t.Fatalf("expected 0 claimed jobs (tenant quota full), got %d", len(resp2.Jobs))
	}

	// 3. Tenant B is unaffected by tenant A's quota.
	jobB := createTestJobForTenant(t, js, tenantB, queue, "demo.echo")
	resp3, err := svc.Poll(ctx, &workerv1.PollRequest{
		WorkerId: "gw-poll-worker-3",
		MaxJobs:  5,
		Queues:   []string{queue},
	})
	if err != nil {
		t.Fatalf("third poll: %v", err)
	}
	foundB := false
	for _, j := range resp3.Jobs {
		if j.JobId == jobB.ID {
			foundB = true
			break
		}
	}
	if !foundB {
		t.Error("expected tenant B job to be claimable while tenant A quota is full")
	}

	// 4. Tenant A's second job must still be ready (unclaimed).
	gotA2, err := js.GetByID(ctx, tenantA, jobA2.ID)
	if err != nil {
		t.Fatalf("get tenant A job 2: %v", err)
	}
	if gotA2.State != domain.StateReady {
		t.Errorf("expected tenant A job 2 to still be ready, got %s", gotA2.State)
	}
}

// createTestJobForTenant creates a ready job for a specific tenant.
func createTestJobForTenant(t *testing.T, s store.JobStore, tenantID, queue, jobType string) *domain.Job {
	t.Helper()
	id := uuid.New().String()
	pastRunAt := time.Now().Add(-1 * time.Second)
	job, err := domain.NewJob(id, domain.NewJobParams{
		TenantID: tenantID,
		Queue:    queue,
		Type:     jobType,
		Payload:  []byte(`{"test":true}`),
		RunAt:    &pastRunAt,
	}, time.Now())
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	_, err = s.Enqueue(context.Background(), job)
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	return job
}
