//go:build linux

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xjfyrh/jobforge/internal/business"
	agentrun "github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/run/httpapi"
	runpostgres "github.com/xjfyrh/jobforge/internal/run/postgres"
)

type restoredCapture struct{ calls atomic.Int32 }

func (c *restoredCapture) Capture(_ context.Context, _, _, _ string) (agentrun.SnapshotBinding, error) {
	c.calls.Add(1)
	return agentrun.SnapshotBinding{}, agentrun.ErrDependencyUnavailable
}

// Only an explicitly supplied, isolated restored copy is aged. All action
// replays use a NOLOGIN receipt reader with read-only transactions. No model,
// Worker, scanner or writer pool is activated.
func TestRunS5RestoredBusinessReceiptReplay(t *testing.T) {
	dsn, path := os.Getenv("JOBFORGE_S5_RESTORE_BUSINESS_DSN"), os.Getenv("JOBFORGE_S5_RESTORE_ACTIONS")
	if dsn == "" || path == "" || os.Getenv("JOBFORGE_TEST_WORKER_HELPER") != "1" {
		t.Skip("requires explicitly owned isolated business restore and original signed actions")
	}
	admin, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal("restored business database unavailable")
	}
	defer admin.Close()
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("restore configuration")
	}
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `set role jobforge_business_receipt_reader; set search_path=pg_catalog; set default_transaction_read_only=on`)
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal("restored receipt reader unavailable")
	}
	defer pool.Close()
	store := business.NewStore(pool)
	if err := store.CheckActionRole(t.Context(), false); err != nil {
		t.Fatal("restored receipt role boundary", err)
	}
	raw, err := os.ReadFile(path)
	var actions []business.SignedAction
	if err != nil || json.Unmarshal(raw, &actions) != nil || len(actions) == 0 {
		t.Fatal("original backed signed actions unavailable")
	}
	snapshot := func() []byte {
		t.Helper()
		var raw []byte
		if err := admin.QueryRow(t.Context(), `select jsonb_build_object(
		 'tickets',(select jsonb_agg(to_jsonb(t) order by tenant_id,ticket_id) from business.tickets t),
		 'snapshots',(select jsonb_agg(to_jsonb(s) order by tenant_id,snapshot_id) from business.snapshots s),
		 'receipts',(select jsonb_agg(to_jsonb(r) order by tenant_id,operation_id) from business.ticket_resolutions r))`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	replay := func() {
		t.Helper()
		before := snapshot()
		for _, action := range actions {
			first, err := store.Receipt(t.Context(), action.Authorization.TenantID, action.Authorization.OperationID)
			if err != nil || first.Validate(action) != nil {
				t.Fatal("restored first receipt identity")
			}
			repeated, err := store.ApplyResolution(t.Context(), action.Authorization.TenantID, action, nil)
			if err != nil || repeated != first {
				t.Fatal("original signed action did not reuse first receipt", err)
			}
			changed := action
			changed.Parameters.Summary = "Changed content"
			if _, err := store.ApplyResolution(t.Context(), action.Authorization.TenantID, changed, nil); !errors.Is(err, business.ErrActionConflict) {
				t.Fatal("changed old operation accepted", err)
			}
			changed = action
			changed.Authorization.OperationID, changed.Authorization.BusinessRequestID = uuid.NewString(), uuid.NewString()
			if _, err := store.ApplyResolution(t.Context(), action.Authorization.TenantID, changed, nil); !errors.Is(err, business.ErrActionForbidden) {
				t.Fatal("restored environment accepted new action", err)
			}
		}
		if !bytes.Equal(before, snapshot()) {
			t.Fatal("receipt replay changed restored business facts or identities")
		}
	}
	replay()
	// Controlled ageing changes relational test timestamps only; the original
	// receipt JSON, signature and business effect remain byte-identical. This is
	// a retention boundary mechanism test, not a month-old live-model result.
	if _, err := admin.Exec(t.Context(), `begin;
	 alter table business.ticket_resolutions disable trigger resolution_immutable;
	 update business.ticket_resolutions set applied_at=clock_timestamp()-interval '31 days',retain_until=clock_timestamp()-interval '1 day';
	 alter table business.ticket_resolutions enable trigger resolution_immutable;
	 commit;`); err != nil {
		t.Fatal("controlled ageing of owned restored receipt copy", err)
	}
	replay()
	if _, err := pool.Exec(t.Context(), `delete from business.ticket_resolutions`); err == nil {
		t.Fatal("receipt reader can erase old dedupe identity")
	}
	t.Log("original signed actions reused immutable first receipts before/after controlled 31-day ageing; changed identities denied; zero new effects; receipt reader remains read-only")
}

// This explicit exercise bypasses suite cleanup and enters only the new
// isolated restored control DB. No Worker, scanner or writer is constructed.
func TestRunS5RestoredDisabledSDKReplay(t *testing.T) {
	dsn, dir := os.Getenv("JOBFORGE_S5_RESTORE_DSN"), os.Getenv("JOBFORGE_S5_REVIEW_DIR")
	if dsn == "" || dir == "" || os.Getenv("JOBFORGE_TEST_WORKER_HELPER") != "1" {
		t.Skip("requires explicit fresh disabled restore and installed SDK")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal("restored control database unavailable")
	}
	defer pool.Close()
	store, err := runpostgres.New(pool, runpostgres.Options{})
	if err != nil {
		t.Fatal(err)
	}
	capture := &restoredCapture{}
	service, err := agentrun.NewService(store, capture, []string{"tenant-north"})
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]string
	raw, err := os.ReadFile(filepath.Join(dir, "review-keys.json"))
	if err != nil || json.Unmarshal(raw, &keys) != nil {
		t.Fatal("private restored review credentials unavailable")
	}
	router, err := httpapi.NewRouter(service, map[string]httpapi.Identity{
		keys["operator"]: {TenantID: "tenant-north", Role: "operator"},
		keys["approver"]: {TenantID: "tenant-north", Role: "approver", ActorID: "s5-reviewer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func() []byte {
		t.Helper()
		var raw []byte
		err := pool.QueryRow(t.Context(), `select jsonb_build_object(
			'runs',(select jsonb_agg(to_jsonb(r) order by run_id) from runs r),
			'budgets',(select jsonb_agg(to_jsonb(a) order by account_id) from budget_accounts a),
			'calls',(select jsonb_agg(to_jsonb(c) order by physical_call_id) from physical_calls c),
			'operations',(select jsonb_agg(to_jsonb(o) order by operation_id) from run_operations o),
			'approvals',(select jsonb_agg(to_jsonb(a) order by tenant_id,run_id) from run_approvals a),
			'steps',(select jsonb_agg(to_jsonb(s) order by run_id,sequence) from run_steps s),
			'actions',(select jsonb_agg(to_jsonb(a) order by operation_id) from action_authorizations a),
			'receipts',(select jsonb_agg(to_jsonb(r) order by operation_id) from action_receipt_views r))`).Scan(&raw)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	before := snapshot()
	var decisions []byte
	if err := pool.QueryRow(t.Context(), `select coalesce(jsonb_agg(jsonb_build_object(
		'run_id',a.run_id,'status',a.status,'proposal_hash',a.proposal_hash,'key',o.operation_key)),'[]')
		from run_approvals a join run_operations o on o.operation_id=a.decision_operation_id
		where a.status in ('approved','rejected')`).Scan(&decisions); err != nil {
		t.Fatal(err)
	}
	decisionFile := filepath.Join(t.TempDir(), "accepted-decisions.json")
	if err := os.WriteFile(decisionFile, decisions, 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(router)
	defer server.Close()
	command := exec.Command("/usr/local/bin/python", "-c", s5RestoredSDK, dir, server.URL, decisionFile)
	command.Env = os.Environ()
	if _, err := command.Output(); err != nil {
		t.Fatal("installed SDK restored read/replay failed; protected output withheld")
	}
	if capture.calls.Load() != 0 || !bytes.Equal(before, snapshot()) {
		t.Fatal("restored replay captured, charged, dispatched or changed execution identities")
	}
	t.Log("installed SDK read and accepted submit/approval replay matched; new submit disabled; zero captures; all control budgets/calls/operations/actions/receipts unchanged")
}

const s5RestoredSDK = `
import json,pathlib,sys
from jobforge import RunClient, ProfileUnavailableError
root=pathlib.Path(sys.argv[1]); keys=json.loads((root/'review-keys.json').read_bytes()); m=json.loads((root/'environment.json').read_bytes())
with RunClient(sys.argv[2],keys['operator']) as client:
 for case,run_id in m['runs'].items():
  view=client.get(run_id)
  result=client.submit(ticket_id='ticket-1',business_request_key='s5-review-'+case,profile_id=m['profile_id'],budget_batch_id=m['batch_id'],idempotency_key='s5-review-'+case)
  assert result.reused and result.run.run_id==run_id and result.run.budget==view.budget
  client.effect(run_id); client.steps(run_id); client.calls(run_id)
 try:
  client.submit(ticket_id='ticket-1',business_request_key='restore-must-not-start',profile_id=m['profile_id'],budget_batch_id=m['batch_id'],idempotency_key='restore-must-not-start')
 except ProfileUnavailableError:
  pass
 else:
  raise AssertionError('disabled restored profile accepted new execution')
with RunClient(sys.argv[2],keys['approver']) as client:
 for d in json.loads(pathlib.Path(sys.argv[3]).read_bytes()):
  decision='approve' if d['status']=='approved' else 'reject'
  response=client.decide_approval(d['run_id'],decision,d['proposal_hash'],idempotency_key=d['key'])
  assert response.reused and response.approval.status==d['status']
`
