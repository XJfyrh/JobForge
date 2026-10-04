package integration

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
	runpostgres "github.com/xjfyrh/jobforge/internal/run/postgres"
)

func TestRunRecoveryLegacyPendingCallCannotUseNewProfilePolicy(t *testing.T) {
	for _, schema := range []int{1, 2} {
		t.Run(string(rune('0'+schema)), func(t *testing.T) {
			h := setupRecoveryHarness(t)
			s3 := h.Profile
			d, err := agentrun.DecodeSupportDefinition(s3.Definition)
			if err != nil {
				t.Fatal(err)
			}
			if schema == 1 {
				raw, err := os.ReadFile(filepath.Join("..", "..", "api", "support", "profile-v1", "fixtures.json"))
				if err != nil {
					t.Fatal(err)
				}
				var fixture struct {
					Profile agentrun.Profile `json:"profile"`
				}
				if json.Unmarshal(raw, &fixture) != nil {
					t.Fatal("invalid historical fixture")
				}
				d, err = agentrun.DecodeSupportDefinition(fixture.Profile.Definition)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				d.SchemaVersion, d.Program.RecoveryPolicy = 2, ""
			}
			legacy, err := agentrun.BuildSupportProfile("recovery-mixed-legacy", d)
			if err != nil {
				t.Fatal(err)
			}
			legacy.Executable = true
			h.Options.Profiles = append(h.Options.Profiles, legacy)
			h.Options.Workers = append(h.Options.Workers, agentrun.WorkerConfig{ID: "mixed-legacy-worker", Tenants: []string{"tenant-north"}, ProfileIDs: []string{legacy.ID}, Capacity: 1})
			h.Store, err = runpostgres.New(h.Pool, h.Options)
			if err != nil {
				t.Fatal(err)
			}
			if err := h.Store.EnsureProfiles(h.Ctx); err != nil {
				t.Fatal(err)
			}
			h.Service, err = agentrun.NewService(h.Store, h.Capture, []string{"tenant-north", "tenant-south"})
			if err != nil {
				t.Fatal(err)
			}
			h.Profile, h.Principal = legacy, "mixed-legacy-worker"
			h.Session, err = h.Store.Register(h.Ctx, h.Principal, uuid.NewString(), legacy.ExecutorVersion)
			if err != nil {
				t.Fatal(err)
			}
			supportProfileCapture(t, h, "submit", "submit-mixed-legacy", "", true)
			h.submit(t, "tenant-north", "mixed-legacy")
			old := h.claim(t)
			for !agentrun.IsModelStep(currentRunStep(old).Kind) {
				recoveryCommit(t, h, &old, supportStorageResult(t, h, old, "proposal"))
			}
			recoveryChat(t, h, old, true)
			if _, err := h.Store.FailExecution(h.Ctx, h.Principal, old.Lease, currentRunStep(old), "TIMEOUT"); err != nil {
				t.Fatal(err)
			}
			var proof bool
			if err := h.Pool.QueryRow(h.Ctx, "select recovery_step is not null or recovery_ordinal is not null from run_attempts where run_id=$1", old.Lease.RunID).Scan(&proof); err != nil || proof {
				t.Fatal("legacy closure was silently upgraded")
			}
			h.Profile, h.Principal = s3, h.Options.Workers[0].ID
			h.Session, err = h.Store.Register(h.Ctx, h.Principal, uuid.NewString(), s3.ExecutorVersion)
			if err != nil {
				t.Fatal(err)
			}
			supportProfileCapture(t, h, "submit", "submit-mixed-s3", "", true)
			next := h.submit(t, "tenant-north", "mixed-s3")
			before, err := h.Store.Get(h.Ctx, next.TenantID, next.ID)
			if err != nil {
				t.Fatal(err)
			}
			claim, err := h.Store.Claim(h.Ctx, h.Principal, h.Session.ID)
			if !errors.Is(err, agentrun.ErrBudgetExhausted) || claim != nil {
				t.Fatalf("schema %d call used S3 policy: %v", schema, err)
			}
			after, err := h.Store.Get(h.Ctx, next.TenantID, next.ID)
			if err != nil || !reflect.DeepEqual(before.Budget, after.Budget) || after.AttemptNo != 0 {
				t.Fatal("mixed-profile rejection charged or claimed")
			}
		})
	}
}

func TestRunRecoveryConcurrentClaimReserveAndOldReportReplay(t *testing.T) {
	h := setupRecoveryHarness(t)
	old := recoveryAtDecision(t, h, "race-old")
	_, report := recoveryChat(t, h, old, true)
	if _, err := h.Store.FailExecution(h.Ctx, h.Principal, old.Lease, currentRunStep(old), "TIMEOUT"); err != nil {
		t.Fatal(err)
	}
	recoveryReady(t, h, old.Lease.RunID, 3*time.Second)
	supportProfileCapture(t, h, "submit", "submit-race-other", "", true)
	other := h.submit(t, "tenant-north", "race-other")
	second, err := h.Store.Register(h.Ctx, h.Options.Workers[1].ID, uuid.NewString(), h.Profile.ExecutorVersion)
	if err != nil {
		t.Fatal(err)
	}
	sessions := []agentrun.Session{h.Session, second}
	claims := make([]*agentrun.ClaimedRun, 2)
	failures := make([]error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range claims {
		wg.Go(func() { <-start; claims[i], failures[i] = h.Store.Claim(h.Ctx, sessions[i].WorkerID, sessions[i].ID) })
	}
	close(start)
	wg.Wait()
	var current agentrun.ClaimedRun
	winners, blocked := 0, 0
	for i, claim := range claims {
		if claim != nil && failures[i] == nil && claim.Lease.RunID == old.Lease.RunID {
			winners++
			current = *claim
			h.Principal, h.Session = sessions[i].WorkerID, sessions[i]
		} else if claim == nil && errors.Is(failures[i], agentrun.ErrBudgetExhausted) {
			blocked++
		} else {
			t.Fatalf("unexpected concurrent claim: %v", failures[i])
		}
	}
	if winners != 1 || blocked != 1 {
		t.Fatal("same-step exemption escaped to another Run")
	}
	newCall := ledgerRequest(h, current, agentrun.SubcallChat, "")
	ledgerReserve(t, h, newCall)
	before := ledgerView(t, h, current.Lease)
	var replay agentrun.ReserveCallResponse
	failures = make([]error, 3)
	start = make(chan struct{})
	wg.Go(func() { <-start; replay, failures[0] = h.Store.ReserveCall(h.Ctx, h.Principal, newCall) })
	wg.Go(func() { <-start; _, failures[1] = h.Store.SettleUsage(h.Ctx, old.Lease.WorkerID, report) })
	// The losing principal may request another Run but cannot cross this batch's pending chat.
	loser := sessions[0]
	if loser.WorkerID == h.Principal {
		loser = sessions[1]
	}
	wg.Go(func() { <-start; _, failures[2] = h.Store.Claim(h.Ctx, loser.WorkerID, loser.ID) })
	close(start)
	wg.Wait()
	if failures[0] != nil || replay.NewlyReserved || failures[1] != nil || !errors.Is(failures[2], agentrun.ErrBudgetExhausted) {
		t.Fatalf("concurrent replay/claim: %v", failures)
	}
	var active string
	if err := h.Pool.QueryRow(h.Ctx, "select active_call_id::text from runs where run_id=$1", current.Lease.RunID).Scan(&active); err != nil || active != newCall.PhysicalCallID || !reflect.DeepEqual(before.Budget, ledgerView(t, h, current.Lease).Budget) {
		t.Fatal("old report or duplicate reservation cleared/charged new active call")
	}
	view, err := h.Store.Get(h.Ctx, other.TenantID, other.ID)
	if err != nil || view.AttemptNo != 0 {
		t.Fatal("other Run acquired unauthorized attempt")
	}
}

func TestRunRecoveryRejectsMissingGapAndDuplicateOrdinals(t *testing.T) {
	h := setupRecoveryHarness(t)
	current := recoveryAtDecision(t, h, "ordinal-proof")
	recoveryChat(t, h, current, true)
	for ordinal := int64(1); ordinal <= 2; ordinal++ {
		if _, err := h.Store.FailExecution(h.Ctx, h.Principal, current.Lease, currentRunStep(current), "TIMEOUT"); err != nil {
			t.Fatal(err)
		}
		recoveryReady(t, h, current.Lease.RunID, 4*time.Second)
		if ordinal == 1 {
			current = h.claim(t)
		}
	}
	for _, mutation := range []string{
		"update run_attempts set recovery_step=null,recovery_ordinal=null where run_id=$1 and attempt_no=1",
		"update run_attempts set recovery_ordinal=3 where run_id=$1 and attempt_no=1",
		"update run_attempts set recovery_ordinal=1 where run_id=$1 and attempt_no=2",
	} {
		tx, err := h.Pool.Begin(h.Ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(h.Ctx, mutation, current.Lease.RunID); err != nil {
			_ = tx.Rollback(h.Ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(h.Ctx); err != nil {
			t.Fatal(err)
		}
		claim, err := h.Store.Claim(h.Ctx, h.Principal, h.Session.ID)
		if !errors.Is(err, agentrun.ErrBudgetExhausted) || claim != nil {
			t.Fatal("noncontinuous proof authorized recovery")
		}
		// Restore only proof corruption, never production lease/backoff/session time.
		step, _ := json.Marshal(currentRunStep(current))
		if _, err := h.Pool.Exec(h.Ctx, "update run_attempts set recovery_step=$2,recovery_ordinal=attempt_no where run_id=$1", current.Lease.RunID, step); err != nil {
			t.Fatal(err)
		}
	}
	if resumed := h.claim(t); resumed.Lease.AttemptNo != 3 {
		t.Fatal("restored proof did not recover")
	}
}
