package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store"
	"github.com/xjfyrh/jobforge/internal/store/postgres"
	"github.com/xjfyrh/jobforge/migrations"
	workerv1 "github.com/xjfyrh/jobforge/proto/jobforge/worker/v1"
)

func claimResultJob(t *testing.T, js *postgres.JobStore) (*domain.Job, string) {
	t.Helper()
	queue := "result-" + uuid.NewString()
	job := createTestJob(t, js, queue, "demo.echo")
	owner := "result-worker"
	claimed, err := js.Claim(t.Context(), store.ClaimParams{Queues: []string{queue}, WorkerID: owner, MaxJobs: 1, LeaseTTL: time.Minute})
	if err != nil || len(claimed.Jobs) != 1 {
		t.Fatalf("claim %s: %v", job.ID, err)
	}
	return claimed.Jobs[0], owner
}

func TestAT34ResultFirstCommitWins(t *testing.T) {
	js := setupStore(t)
	job, owner := claimResultJob(t, js)
	const concurrent = 8
	results := make(chan *store.AttemptResult, concurrent)
	errs := make(chan error, concurrent)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range concurrent {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := js.CompleteAttempt(t.Context(), job.ID, owner, job.FencingToken, fmt.Sprintf("artifact:accepted-%d", i), 123)
			results <- result
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	changed := 0
	for result := range results {
		if result.Changed {
			changed++
		}
		if result.Queue != job.Queue || result.Type != job.Type || result.Outcome != "succeeded" || result.DurationMs != 123 {
			t.Fatalf("transaction metadata: %+v", result)
		}
	}
	if changed != 1 {
		t.Fatalf("changed=%d", changed)
	}
	got, err := js.GetByID(t.Context(), job.TenantID, job.ID)
	if err != nil || got.ResultRef == nil || !strings.HasPrefix(*got.ResultRef, "artifact:accepted-") {
		t.Fatalf("persisted result: %+v %v", got, err)
	}
	first := *got.ResultRef
	if _, err := js.CompleteAttempt(t.Context(), job.ID, owner, job.FencingToken, "artifact:replacement", 999); err != nil {
		t.Fatal(err)
	}
	got, _ = js.GetByID(t.Context(), job.TenantID, job.ID)
	if *got.ResultRef != first {
		t.Fatal("duplicate replaced first result")
	}
	var events int
	if err := testEnv.pool.QueryRow(t.Context(), "select count(*) from outbox_events where aggregate_id = $1 and event_type = 'job.succeeded'", job.ID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("success events=%d err=%v", events, err)
	}
	if _, err := js.GetByID(t.Context(), "foreign-tenant", job.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("foreign result lookup: %v", err)
	}
}

func TestAT34ResultBoundsAndCancel(t *testing.T) {
	js := setupStore(t)
	for _, ref := range []string{"", strings.Repeat("x", 2048), strings.Repeat("界", 682), "opaque:\u0085\u009f"} {
		job, owner := claimResultJob(t, js)
		if err := js.Complete(t.Context(), job.ID, owner, job.FencingToken, ref, 0); err != nil {
			t.Fatal(err)
		}
		got, _ := js.GetByID(t.Context(), job.TenantID, job.ID)
		if ref == "" && got.ResultRef != nil || ref != "" && (got.ResultRef == nil || *got.ResultRef != ref) {
			t.Fatalf("roundtrip %d bytes: %v", len(ref), got.ResultRef)
		}
	}
	job, owner := claimResultJob(t, js)
	for _, ref := range []string{strings.Repeat("x", 2049), strings.Repeat("界", 683), "bad\nref", "bad\x00", "bad\x1f", "bad\x7f", string([]byte{0xff})} {
		if _, err := js.CompleteAttempt(t.Context(), job.ID, owner, job.FencingToken, ref, 1); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Fatalf("invalid reference: %v", err)
		}
	}
	if err := js.Cancel(t.Context(), job.TenantID, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := js.CompleteAttempt(t.Context(), job.ID, owner, job.FencingToken, "artifact:cancelled", 1); !errors.Is(err, domain.ErrCancelRequested) {
		t.Fatalf("cancel result write: %v", err)
	}
	if _, err := js.CompleteAttempt(t.Context(), job.ID, "foreign", job.FencingToken, "artifact:foreign", 1); !errors.Is(err, domain.ErrStaleLease) {
		t.Fatalf("foreign cancelling write: %v", err)
	}
	result, err := js.FailAttempt(t.Context(), job.ID, owner, job.FencingToken, "CANCELLED", "cancelled", true, 1)
	if err != nil || result.State != domain.StateCancelled || result.Outcome != "cancelled" {
		t.Fatalf("cancel ack: %+v %v", result, err)
	}
	got, _ := js.GetByID(t.Context(), job.TenantID, job.ID)
	if got.ResultRef != nil {
		t.Fatal("cancelled job has a result")
	}
}

func TestAT34StaleReportsNeverBecomeDuplicateACK(t *testing.T) {
	service, js := newWorkerContractService(t)
	job, owner := claimResultJob(t, js)
	request := &workerv1.FailRequest{JobId: job.ID, WorkerId: owner, FencingToken: job.FencingToken, ErrorCode: "TEMPORARY", Retryable: true}
	first, err := service.Fail(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Fail(t.Context(), request)
	if err != nil || first.State != second.State || !first.NextRetryAt.AsTime().Equal(second.NextRetryAt.AsTime()) {
		t.Fatalf("duplicate fail ACK: %v %v %v", first, second, err)
	}
	if _, err := testEnv.pool.Exec(t.Context(), "update jobs set state='ready', run_at=now() - interval '1 second' where id=$1", job.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err := js.Claim(t.Context(), store.ClaimParams{Queues: []string{job.Queue}, WorkerID: "new-owner", MaxJobs: 1, LeaseTTL: time.Minute})
	if err != nil || len(claimed.Jobs) != 1 {
		t.Fatal(err)
	}
	if err := js.Complete(t.Context(), job.ID, "new-owner", claimed.Jobs[0].FencingToken, "artifact:new", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := js.FailAttempt(t.Context(), job.ID, owner, job.FencingToken, "TEMPORARY", "late", true, 1); !errors.Is(err, domain.ErrStaleLease) {
		t.Fatalf("late fail: %v", err)
	}
	if _, err := js.CompleteAttempt(t.Context(), job.ID, owner, job.FencingToken, "artifact:old", 1); !errors.Is(err, domain.ErrStaleLease) {
		t.Fatalf("late complete: %v", err)
	}
}

func TestAT34ResultRollbackWithOutbox(t *testing.T) {
	js := setupStore(t)
	job, owner := claimResultJob(t, js)
	constraint := pgx.Identifier{"result_rollback_" + strings.ReplaceAll(uuid.NewString(), "-", "")}.Sanitize()
	// This disposable integration database constraint creates a real failure
	// after job/attempt/quota writes; no production crash hook is introduced.
	ddl := fmt.Sprintf("alter table outbox_events add constraint %s check (aggregate_id <> '%s'::uuid)", constraint, job.ID)
	if _, err := testEnv.pool.Exec(t.Context(), ddl); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testEnv.pool.Exec(context.Background(), "alter table outbox_events drop constraint "+constraint)
	})
	if err := js.Complete(t.Context(), job.ID, owner, job.FencingToken, "artifact:rollback", 1); err == nil {
		t.Fatal("expected outbox write failure")
	}
	got, _ := js.GetByID(t.Context(), job.TenantID, job.ID)
	attempts, err := js.ListAttempts(t.Context(), job.TenantID, job.ID)
	if err != nil || got.State != domain.StateRunning || got.ResultRef != nil || attempts[0].FinishedAt != nil {
		t.Fatalf("partial commit: job=%+v attempts=%+v err=%v", got, attempts, err)
	}
}

func TestAT34Migration0020(t *testing.T) {
	tx, err := testEnv.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	// A temporary jobs table shadows the real table and models an existing
	// pre-0020 row. Up/down/up and SQL guards run against actual PostgreSQL.
	if _, err := tx.Exec(t.Context(), "create temporary table jobs (state text not null); insert into jobs values ('ready')"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"0020_add_job_result_ref.up.sql", "0020_add_job_result_ref.down.sql", "0020_add_job_result_ref.up.sql", "0022_align_result_ref_controls.up.sql", "0022_align_result_ref_controls.down.sql", "0022_align_result_ref_controls.up.sql"} {
		content, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), string(content)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	var ref *string
	if err := tx.QueryRow(t.Context(), "select result_ref from jobs").Scan(&ref); err != nil || ref != nil {
		t.Fatalf("legacy reference: %v %v", ref, err)
	}
	if _, err := tx.Exec(t.Context(), "update jobs set state='succeeded', result_ref=$1", "opaque:\u0085"); err != nil {
		t.Fatal("C1 is not excluded by C0/DEL contract:", err)
	}
	for _, ref := range []string{"", "bad\nref", "bad\x7f", strings.Repeat("界", 683)} {
		if _, err := tx.Exec(t.Context(), "savepoint invalid_ref"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), "update jobs set state='succeeded', result_ref=$1", ref); err == nil {
			t.Fatal("database accepted invalid ref")
		}
		if _, err := tx.Exec(t.Context(), "rollback to savepoint invalid_ref"); err != nil {
			t.Fatal(err)
		}
	}
}
