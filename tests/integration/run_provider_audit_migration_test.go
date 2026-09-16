package integration

import (
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/xjfyrh/jobforge/internal/migrate"
	agentrun "github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/migrations"
)

func TestRunProviderAuditMigrationDownReapplyPreservesLegacyAccounting(t *testing.T) {
	h := setupRunHarness(t)
	claimed := ledgerAtStep(t, h, "tenant-a", "audit-migration", "model_proposal")
	request := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
	ledgerReserve(t, h, request)
	usage := ledgerUsage(10, 5)
	ledgerObserve(t, h, request, "response", "accepted", &usage)
	before := ledgerView(t, h, claimed.Lease)
	for range 2 {
		content, err := migrations.FS.ReadFile("0024_provider_audit_report.down.sql")
		if err != nil {
			t.Fatal(err)
		}
		tx, err := h.Pool.Begin(h.Ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(h.Ctx, string(content)); err != nil {
			_ = tx.Rollback(h.Ctx)
			t.Fatal(err)
		}
		if _, err := tx.Exec(h.Ctx, "delete from schema_migrations where version=24"); err != nil {
			_ = tx.Rollback(h.Ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(h.Ctx); err != nil {
			t.Fatal(err)
		}
		if err := migrate.New(h.Pool, testLogger(t)).Up(h.Ctx); err != nil {
			t.Fatal(err)
		}
		after := ledgerView(t, h, claimed.Lease)
		if !reflect.DeepEqual(before.Budget, after.Budget) || before.CursorVersion != after.CursorVersion || !before.UpdatedAt.Equal(after.UpdatedAt) {
			t.Fatal("migration changed historical metering or execution")
		}
		view := auditCallView(t, h, request)
		if view.ReportHash != nil || view.AuditHash != nil || view.ProviderAudit != nil || view.AuditStatus != "legacy_not_collected" ||
			!view.UsageKnown || view.KnownTokens != 15 || view.SettledUsage == nil || *view.SettledUsage != usage || view.HTTPStatus != nil {
			t.Fatalf("re-up fabricated audit or lost legacy facts: %+v", view)
		}
		if response := auditSettle(t, h, agentrun.SettleUsageRequest{Lease: claimed.Lease, PhysicalCallID: request.PhysicalCallID, Usage: &usage}); response.NewlySettled {
			t.Fatal("re-up replay settled legacy usage twice")
		}
	}
}

func TestRunProviderAuditMigrationRejectsPartialReportsAndWrongScope(t *testing.T) {
	h := setupAuditHarness(t)
	claimed := auditAtModelStep(t, h, "tenant-a", "audit-constraints")
	request := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
	ledgerReserve(t, h, request)
	for _, query := range []string{
		"update physical_calls set report_hash=repeat('a',64) where physical_call_id=$1",
		"update physical_calls set call_report='{}'::jsonb,report_recorded_at=clock_timestamp() where physical_call_id=$1",
		"update physical_calls set report_hash=repeat('a',64),call_report='{}'::jsonb,report_recorded_at=clock_timestamp() where physical_call_id=$1",
		"update physical_calls set report_hash=repeat('a',64),call_report='{\"usage\":null,\"provider_audit\":null}'::jsonb,report_recorded_at=clock_timestamp() where physical_call_id=$1",
		"update physical_calls set report_conflict_hash=repeat('a',64) where physical_call_id=$1",
		"update physical_calls set execution_binding_hash='INVALID' where physical_call_id=$1",
		"update physical_calls set observation_http_status=200 where physical_call_id=$1",
	} {
		_, err := h.Pool.Exec(h.Ctx, query, request.PhysicalCallID)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("incoherent report not rejected by CHECK: %v", err)
		}
	}
	before := ledgerView(t, h, claimed.Lease)
	if _, err := h.Pool.Exec(h.Ctx, "update budget_accounts set frozen=true,batch_stop_code='REPORT_CONFLICT' where account_id=$1", before.Budget.Family.ID); err == nil {
		t.Fatal("batch reason accepted on family account")
	}
	if _, err := h.Pool.Exec(h.Ctx, "update budget_accounts set batch_stop_code='REPORT_CONFLICT' where account_id=$1", before.Budget.Batch.ID); err == nil {
		t.Fatal("batch reason accepted without frozen state")
	}
	if !reflect.DeepEqual(before.Budget, ledgerView(t, h, claimed.Lease).Budget) {
		t.Fatal("rejected constraint mutation changed ledger")
	}
}

func TestRunProviderAuditCallsLimitRejectsRatherThanTruncates(t *testing.T) {
	h := setupAuditHarness(t)
	claimed := auditAtModelStep(t, h, "tenant-a", "calls-bound")
	response, err := h.Store.Calls(h.Ctx, claimed.Lease.TenantID, claimed.Lease.RunID)
	if err != nil || len(response.Items) != 6 {
		t.Fatalf("initial calls: %+v %v", response, err)
	}
	// Corruption-shaped SQL fixtures test the read bound independently of the
	// normal reservation budget, which already disallows this many calls.
	copyRows := `insert into physical_calls select (jsonb_populate_record(null::physical_calls,
		to_jsonb(c)||jsonb_build_object('physical_call_id',gen_random_uuid()))).*
		from physical_calls c cross join generate_series(1,$2) where c.physical_call_id=$1`
	if _, err := h.Pool.Exec(h.Ctx, copyRows, response.Items[0].PhysicalCallID, 38); err != nil {
		t.Fatal(err)
	}
	response, err = h.Store.Calls(h.Ctx, claimed.Lease.TenantID, claimed.Lease.RunID)
	if err != nil || len(response.Items) != agentrun.MaxCallsPerRun {
		t.Fatalf("exactly 44 calls rejected: %d %v", len(response.Items), err)
	}
	if _, err := h.Pool.Exec(h.Ctx, copyRows, response.Items[0].PhysicalCallID, 1); err != nil {
		t.Fatal(err)
	}
	response, err = h.Store.Calls(h.Ctx, claimed.Lease.TenantID, claimed.Lease.RunID)
	if !errors.Is(err, agentrun.ErrInternal) || len(response.Items) != 0 {
		t.Fatalf("45 calls silently truncated: %d %v", len(response.Items), err)
	}
}
