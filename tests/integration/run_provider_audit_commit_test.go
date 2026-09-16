package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
	runpostgres "github.com/xjfyrh/jobforge/internal/run/postgres"
)

func TestRunProviderAuditCommitOnlyAuditedProfilesLockAccounts(t *testing.T) {
	for _, audited := range []bool{false, true} {
		name := "legacy"
		if audited {
			name = "audited"
		}
		t.Run(name, func(t *testing.T) {
			var h *runHarness
			if audited {
				h = setupAuditHarness(t)
			} else {
				h = setupRunHarness(t)
			}
			h.submit(t, "tenant-a", "commit-account-lock")
			claimed := h.claim(t)
			request := checkpointCommitRequest(t, claimed, checkpointFixtureResult(t, h, claimed, "proposal", false))
			blocker, err := h.Pool.Begin(h.Ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = blocker.Rollback(h.Ctx) }()
			var blockerPID int32
			if err := blocker.QueryRow(h.Ctx, "select pg_backend_pid()").Scan(&blockerPID); err != nil {
				t.Fatal(err)
			}
			if _, err := blocker.Exec(h.Ctx, "select account_id from budget_accounts where scope='batch' for update"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(h.Ctx, 5*time.Second)
			defer cancel()
			if !audited {
				if _, err := h.Store.CommitStep(ctx, h.Principal, request); err != nil {
					t.Fatalf("legacy commit waited for unrelated account lock: %v", err)
				}
				return
			}
			finished := make(chan error, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, err := h.Store.CommitStep(ctx, h.Principal, request)
				finished <- err
			}()
			defer func() {
				cancel()
				<-done
			}()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				var waiting bool
				if err := h.Pool.QueryRow(ctx, `select exists(select 1 from pg_stat_activity
					where datname=current_database() and $1=any(pg_blocking_pids(pid)))`, blockerPID).Scan(&waiting); err != nil {
					t.Fatalf("audited commit did not reach account lock: %v", err)
				}
				if waiting {
					break
				}
				select {
				case <-ticker.C:
				case <-ctx.Done():
					t.Fatal("audited commit did not wait for account lock")
				}
			}
			if _, err := blocker.Exec(h.Ctx, "update budget_accounts set frozen=true where scope='batch'"); err != nil {
				t.Fatal(err)
			}
			if err := blocker.Commit(h.Ctx); err != nil {
				t.Fatal(err)
			}
			err = <-finished
			if !errors.Is(err, agentrun.ErrBudgetExhausted) {
				t.Fatalf("audited commit missed freeze committed during lock wait: %v", err)
			}
			checkpoint, err := h.Store.GetCheckpoint(h.Ctx, h.Principal, claimed.Lease)
			if err != nil || len(checkpoint.Steps) != 0 || checkpoint.Run.CursorVersion != 0 {
				t.Fatalf("rejected audited commit advanced checkpoint: %+v %v", checkpoint, err)
			}
		})
	}
}

func TestRunProviderAuditCommitReplaySurvivesDisabledProfile(t *testing.T) {
	for _, audited := range []bool{false, true} {
		name := "legacy"
		if audited {
			name = "audited"
		}
		t.Run(name, func(t *testing.T) {
			var h *runHarness
			if audited {
				h = setupAuditHarness(t)
			} else {
				h = setupRunHarness(t)
			}
			h.submit(t, "tenant-a", "commit-disabled-profile")
			claimed := h.claim(t)
			first, _ := checkpointAdvance(t, h, &claimed, "proposal", false)
			next := checkpointCommitRequest(t, claimed, checkpointFixtureResult(t, h, claimed, "proposal", false))
			options := h.Options
			options.Profiles = append([]agentrun.Profile(nil), options.Profiles...)
			options.Profiles[0].Executable = false
			offline, err := runpostgres.New(h.Pool, options)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := offline.CommitStep(h.Ctx, h.Principal, first)
			if err != nil || replay.AcceptedStep.CommitHash != first.CommitHash {
				t.Fatalf("disabled profile rejected accepted commit: %+v %v", replay, err)
			}
			if _, err := offline.CommitStep(h.Ctx, h.Principal, next); !errors.Is(err, agentrun.ErrProfileUnavailable) {
				t.Fatalf("disabled profile authorized new commit: %v", err)
			}
			if _, err := h.Pool.Exec(h.Ctx, "update budget_accounts set frozen=true where scope='batch'"); err != nil {
				t.Fatal(err)
			}
			_, err = offline.CommitStep(h.Ctx, h.Principal, first)
			if audited && !errors.Is(err, agentrun.ErrBudgetExhausted) || !audited && err != nil {
				t.Fatalf("accepted replay changed original profile freeze policy: %v", err)
			}
			stale := first
			stale.Lease.FencingToken++
			if _, err := offline.CommitStep(h.Ctx, h.Principal, stale); !errors.Is(err, agentrun.ErrStaleLease) {
				t.Fatalf("batch status preceded stale execution rejection: %v", err)
			}
		})
	}
}
