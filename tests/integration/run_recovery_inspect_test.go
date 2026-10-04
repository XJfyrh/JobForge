package integration

import (
	"testing"

	"github.com/google/uuid"
)

func TestRunRecoveryInspectionRequires26AndKeepsEachPrincipalHistory(t *testing.T) {
	h := setupRecoveryHarness(t)
	request := supportInspectionRequest(t, h)
	first, err := h.Store.InspectSupport(h.Ctx, request)
	if err != nil || !first.MigrationsReady || !first.MatchesConfig {
		t.Fatal("fresh S3 inspection failed", err)
	}
	if _, err := h.Pool.Exec(h.Ctx, "delete from schema_migrations where version=26"); err != nil {
		t.Fatal(err)
	}
	missing, err := h.Store.InspectSupport(h.Ctx, request)
	if err != nil || missing.MigrationsReady || missing.MatchesConfig {
		t.Fatal("S3 accepted a missing migration 26")
	}
	if _, err := h.Pool.Exec(h.Ctx, "insert into schema_migrations(version,name) values(26,'attempt_recovery_proof')"); err != nil {
		t.Fatal(err)
	}
	for _, worker := range h.Options.Workers {
		if _, err := h.Store.Register(h.Ctx, worker.ID, uuid.NewString(), h.Profile.ExecutorVersion); err != nil {
			t.Fatal(err)
		}
		request.WorkerID = worker.ID
		observed, err := h.Store.InspectSupport(h.Ctx, request)
		if err != nil || observed.History.WorkerSessions != 1 || observed.History.WorkerStartups != 1 || observed.History.Runs != 0 {
			t.Fatal("inspection lost replacement principal or double-counted batch", err)
		}
	}
}
