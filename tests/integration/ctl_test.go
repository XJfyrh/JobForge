package integration

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	apihttp "github.com/xjfyrh/jobforge/internal/api/http"
	"github.com/xjfyrh/jobforge/internal/config"
	"github.com/xjfyrh/jobforge/internal/ctl"
	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store"
	"github.com/xjfyrh/jobforge/internal/store/postgres"
	workerv1 "github.com/xjfyrh/jobforge/proto/jobforge/worker/v1"
)

// These tests verify the jobforge ctl operational CLI (PRD v0.2 FR-620/621,
// AT-16) against the real HTTP router and a real PostgreSQL instance.

const ctlTestAPIKey = "ctl-test-key"

// setupCtlServer starts an httptest server with the production router stack
// (auth middleware included) and returns a matching ctl client.
func setupCtlServer(t *testing.T) (*httptest.Server, *ctl.Client, *postgres.JobStore) {
	t.Helper()

	js := setupStore(t)
	cfg := &config.Config{
		APIKeys:        map[string]string{ctlTestAPIKey: "test-tenant"},
		QueueSoftLimit: 10000,
		QueueHardLimit: 50000,
	}
	router := apihttp.NewRouter(js, js, cfg, testLogger(t), nil)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	return server, ctl.NewClient(server.URL, ctlTestAPIKey), js
}

// reanchorRunAt sets run_at using the PostgreSQL clock so claim's
// run_at <= now() predicate is immune to Docker/WSL2 host clock drift.
func reanchorRunAt(t *testing.T, jobID string) {
	t.Helper()
	if _, err := testEnv.pool.Exec(context.Background(),
		`update jobs set run_at = now() - interval '10 seconds' where id = $1`, jobID); err != nil {
		t.Fatalf("re-anchor run_at: %v", err)
	}
}

// TestCtlUnauthorized verifies that requests without a valid API key are
// rejected with the UNAUTHORIZED error code (401).
func TestCtlUnauthorized(t *testing.T) {
	server, _, _ := setupCtlServer(t)
	badClient := ctl.NewClient(server.URL, "wrong-key")

	_, err := badClient.List(context.Background(), ctl.ListOptions{})
	if err == nil {
		t.Fatal("expected unauthorized error")
	}
	var apiErr *ctl.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *ctl.APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 401 || apiErr.Code != "UNAUTHORIZED" {
		t.Fatalf("expected 401 UNAUTHORIZED, got %d %s", apiErr.StatusCode, apiErr.Code)
	}
}

// TestCtlListFilterAndPagination verifies FR-620 list: queue/state filtering
// and keyset pagination with cursor.
func TestCtlListFilterAndPagination(t *testing.T) {
	_, client, js := setupCtlServer(t)
	ctx := context.Background()

	// Three ready jobs in a dedicated queue.
	for i := 0; i < 3; i++ {
		job := createTestJob(t, js, "ctl-list", "demo.echo")
		reanchorRunAt(t, job.ID)
	}

	// Queue filter returns exactly the three jobs.
	res, err := client.List(ctx, ctl.ListOptions{Queue: "ctl-list"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(res.Jobs) != 3 {
		t.Fatalf("expected 3 jobs, got %d", len(res.Jobs))
	}
	for i := range res.Jobs {
		if res.Jobs[i].State != "ready" {
			t.Fatalf("expected ready, got %s", res.Jobs[i].State)
		}
	}

	// Pagination: limit 2 yields a cursor; following it returns the rest.
	page1, err := client.List(ctx, ctl.ListOptions{Queue: "ctl-list", Limit: 2})
	if err != nil {
		t.Fatalf("list page1: %v", err)
	}
	if len(page1.Jobs) != 2 || page1.NextCursor == "" {
		t.Fatalf("expected 2 jobs with cursor, got %d jobs cursor=%q",
			len(page1.Jobs), page1.NextCursor)
	}
	page2, err := client.List(ctx, ctl.ListOptions{Queue: "ctl-list", Limit: 2, Cursor: page1.NextCursor})
	if err != nil {
		t.Fatalf("list page2: %v", err)
	}
	if len(page2.Jobs) != 1 {
		t.Fatalf("expected 1 remaining job, got %d", len(page2.Jobs))
	}

	// State filter excludes ready jobs.
	none, err := client.List(ctx, ctl.ListOptions{Queue: "ctl-list", State: "succeeded"})
	if err != nil {
		t.Fatalf("list state filter: %v", err)
	}
	if len(none.Jobs) != 0 {
		t.Fatalf("expected 0 succeeded jobs, got %d", len(none.Jobs))
	}
}

// TestCtlGetAttempts verifies FR-620 get: details plus attempt timeline.
func TestCtlGetAttempts(t *testing.T) {
	_, client, js := setupCtlServer(t)
	ctx := context.Background()

	job := createTestJob(t, js, "ctl-get", "demo.echo")
	reanchorRunAt(t, job.ID)

	claimed, err := js.Claim(ctx, store.ClaimParams{
		Queue:    "ctl-get",
		WorkerID: "ctl-get-worker",
		MaxJobs:  1,
		LeaseTTL: time.Minute,
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim: %v", err)
	}
	if err := js.Complete(ctx, job.ID, "ctl-get-worker", claimed[0].FencingToken, "ok", 42); err != nil {
		t.Fatalf("complete: %v", err)
	}

	got, err := client.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != "succeeded" {
		t.Fatalf("expected succeeded, got %s", got.State)
	}
	if len(got.Attempts) != 1 {
		t.Fatalf("expected 1 attempt, got %d", len(got.Attempts))
	}
	a := got.Attempts[0]
	if a.Outcome != "succeeded" || a.WorkerID != "ctl-get-worker" {
		t.Fatalf("unexpected attempt: outcome=%s worker=%s", a.Outcome, a.WorkerID)
	}
	if a.DurationMs == nil || *a.DurationMs != 42 {
		t.Fatalf("expected duration 42ms, got %v", a.DurationMs)
	}
}

// TestCtlCancel verifies FR-620 cancel through the CLI client: a waiting
// job transitions to cancelled immediately.
func TestCtlCancel(t *testing.T) {
	_, client, js := setupCtlServer(t)
	ctx := context.Background()

	job := createTestJob(t, js, "ctl-cancel", "demo.echo")

	if err := client.Cancel(ctx, job.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	got, err := js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != domain.StateCancelled {
		t.Fatalf("expected cancelled, got %s", got.State)
	}
}

// TestCtlAT16RetryDLQ verifies AT-16: ctl retry on a dead job clones a new
// job_id for execution; the original terminal state stays immutable and the
// retry_of_job_id audit link is recorded.
func TestCtlAT16RetryDLQ(t *testing.T) {
	_, client, js := setupCtlServer(t)
	ctx := context.Background()

	// Drive a job into dead: claim then fail with a non-retryable error.
	job := createTestJob(t, js, "ctl-at16", "demo.fail")
	reanchorRunAt(t, job.ID)

	claimed, err := js.Claim(ctx, store.ClaimParams{
		Queue:    "ctl-at16",
		WorkerID: "ctl-at16-worker",
		MaxJobs:  1,
		LeaseTTL: time.Minute,
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim: %v", err)
	}
	if err := js.Fail(ctx, job.ID, "ctl-at16-worker", claimed[0].FencingToken,
		"BUSINESS_ERROR", "fatal failure", false, 10); err != nil {
		t.Fatalf("fail: %v", err)
	}

	orig, err := js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil || orig.State != domain.StateDead {
		t.Fatalf("expected dead, got %v (err=%v)", orig.State, err)
	}

	// The dead job shows up in a DLQ-style list.
	dlq, err := client.List(ctx, ctl.ListOptions{Queue: "ctl-at16", State: "dead"})
	if err != nil {
		t.Fatalf("list dead: %v", err)
	}
	if len(dlq.Jobs) != 1 || dlq.Jobs[0].ID != job.ID {
		t.Fatalf("expected the dead job in DLQ list, got %d jobs", len(dlq.Jobs))
	}

	// Manual retry through the CLI.
	res, err := client.Retry(ctx, job.ID)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if res.JobID == job.ID || res.JobID == "" {
		t.Fatalf("expected a new cloned job_id, got %q", res.JobID)
	}

	// Original job terminal state is immutable.
	origAfter, err := js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get original: %v", err)
	}
	if origAfter.State != domain.StateDead {
		t.Fatalf("original job state changed: %s", origAfter.State)
	}

	// Clone carries the audit link and is executable (ready).
	clone, err := js.GetByID(ctx, "test-tenant", res.JobID)
	if err != nil {
		t.Fatalf("get clone: %v", err)
	}
	if clone.RetryOfJobID == nil || *clone.RetryOfJobID != job.ID {
		t.Fatalf("clone missing retry_of_job_id audit link: %v", clone.RetryOfJobID)
	}
	if clone.State != domain.StateReady {
		t.Fatalf("clone expected ready, got %s", clone.State)
	}
	if clone.Queue != orig.Queue || clone.Type != orig.Type {
		t.Fatalf("clone lost queue/type: %s/%s", clone.Queue, clone.Type)
	}
}

// TestCtlRetryTerminalGuard verifies that retrying a succeeded job is
// rejected with ALREADY_TERMINAL (409), surfaced as *ctl.APIError.
func TestCtlRetryTerminalGuard(t *testing.T) {
	_, client, js := setupCtlServer(t)
	ctx := context.Background()

	job := createTestJob(t, js, "ctl-guard", "demo.echo")
	reanchorRunAt(t, job.ID)
	claimed, err := js.Claim(ctx, store.ClaimParams{
		Queue:    "ctl-guard",
		WorkerID: "ctl-guard-worker",
		MaxJobs:  1,
		LeaseTTL: time.Minute,
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim: %v", err)
	}
	if err := js.Complete(ctx, job.ID, "ctl-guard-worker", claimed[0].FencingToken, "ok", 1); err != nil {
		t.Fatalf("complete: %v", err)
	}

	_, err = client.Retry(ctx, job.ID)
	var apiErr *ctl.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *ctl.APIError, got %v", err)
	}
	if apiErr.Code != "ALREADY_TERMINAL" {
		t.Fatalf("expected ALREADY_TERMINAL, got %s", apiErr.Code)
	}
}

// TestCtlOutboxStatus verifies FR-621: the read-only backlog summary reports
// pending count and the oldest unpublished event time.
func TestCtlOutboxStatus(t *testing.T) {
	ctx := context.Background()

	before, err := ctl.QueryOutboxStatus(ctx, testEnv.dsn)
	if err != nil {
		t.Fatalf("outbox status before: %v", err)
	}

	insertOutboxEvent(t, uuid.New().String(), "job.succeeded")

	after, err := ctl.QueryOutboxStatus(ctx, testEnv.dsn)
	if err != nil {
		t.Fatalf("outbox status after: %v", err)
	}
	if after.Pending != before.Pending+1 {
		t.Fatalf("expected pending %d -> %d, got %d", before.Pending, before.Pending+1, after.Pending)
	}
	if after.OldestUnpublished == nil {
		t.Fatal("expected oldest_unpublished to be set")
	}
}

// TestCtlWorkersStatus verifies the workers-status operational query: every
// registered worker is listed with a staleness flag derived from the
// supplied threshold (fresh registration vs backdated heartbeat).
func TestCtlWorkersStatus(t *testing.T) {
	ctx := context.Background()
	js := setupStore(t)

	freshID := "ctl-ws-fresh-" + uuid.New().String()[:8]
	staleID := "ctl-ws-stale-" + uuid.New().String()[:8]
	for _, id := range []string{freshID, staleID} {
		err := js.RegisterWorker(ctx, &workerv1.RegisterRequest{
			WorkerId:       id,
			InstanceId:     "instance-ctl-ws",
			SupportedTypes: []string{"demo.echo"},
			Queues:         []string{"default"},
			Capacity:       1,
			Version:        "1.0.0-ctl-ws",
		}, uuid.New().String())
		if err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	// Age one worker beyond the threshold (PostgreSQL clock, drift-safe).
	if _, err := testEnv.pool.Exec(ctx,
		`update workers set last_heartbeat_at = now() - interval '2 hours' where worker_id = $1`,
		staleID); err != nil {
		t.Fatalf("backdate heartbeat: %v", err)
	}

	workers, err := ctl.QueryWorkers(ctx, testEnv.dsn, time.Hour)
	if err != nil {
		t.Fatalf("query workers: %v", err)
	}

	var freshSeen, staleSeen bool
	for _, w := range workers {
		switch w.WorkerID {
		case freshID:
			freshSeen = true
			if w.Stale {
				t.Error("freshly registered worker must not be flagged stale")
			}
		case staleID:
			staleSeen = true
			if !w.Stale {
				t.Error("backdated worker must be flagged stale")
			}
			if w.LastHeartbeatAt == nil {
				t.Error("stale worker must carry its last heartbeat timestamp")
			}
		}
	}
	if !freshSeen || !staleSeen {
		t.Fatalf("expected both workers in the registry listing (fresh=%v stale=%v)",
			freshSeen, staleSeen)
	}
}
