package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store"
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
		Queue:             queueA,
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
		Queue:             queueA,
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
		Queue:             queueA,
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
		Queue:             queue,
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
		Queue:             queue,
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
