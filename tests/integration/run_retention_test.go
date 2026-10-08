package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/run/httpapi"
)

func ageTerminalContent(t *testing.T, h *runHarness, id string) {
	t.Helper()
	// Only this newly-created owned database is aged. Original financial and
	// physical-call identities are kept, and no production clock is shortened.
	if _, err := h.Pool.Exec(h.Ctx, `update runs set
		created_at=clock_timestamp()-interval '9 days',
		terminal_at=clock_timestamp()-interval '8 days' where run_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(h.Ctx, `update budget_accounts set
		valid_from=clock_timestamp()-interval '10 days',valid_until=clock_timestamp()-interval '1 day'
		where scope='batch'`); err != nil {
		t.Fatal(err)
	}
}

func TestRunPurgedApprovalHTTPRejectsAcceptedApproveAndRejectReplay(t *testing.T) {
	for _, decision := range []string{"approve", "reject"} {
		t.Run(decision, func(t *testing.T) {
			h := setupApprovalHarness(t)
			r := approvalPending(t, h, "s5-purged-"+decision)
			view, err := h.Store.Approval(h.Ctx, r.TenantID, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			approveRun(t, h, r, decision)
			if decision == "approve" {
				claimed := h.claim(t)
				if _, err := h.Store.AuthorizeAction(h.Ctx, h.Principal, claimed.Lease, currentRunStep(claimed)); err != nil {
					t.Fatal(err)
				}
				if _, err := h.Store.Cancel(h.Ctx, r.TenantID, r.ID, "cancel-approved"); err != nil {
					t.Fatal(err)
				}
				if _, err := h.Store.AcknowledgeStop(h.Ctx, h.Principal, claimed.Lease); err != nil {
					t.Fatal(err)
				}
			}
			// These are synthetic approval/action identities; no business receipt
			// or cloud call is fabricated by this HTTP retention regression.
			identities := func() []byte {
				t.Helper()
				var raw []byte
				err := h.Pool.QueryRow(h.Ctx, `select jsonb_build_object(
					'approval',(select to_jsonb(a) from run_approvals a where run_id=$1),
					'operations',(select jsonb_agg(to_jsonb(o) order by operation_id) from run_operations o where source_run_id=$1),
					'events',(select jsonb_agg(to_jsonb(e) order by sequence) from run_events e where run_id=$1),
					'actions',(select jsonb_agg(to_jsonb(a) order by operation_id) from action_authorizations a where authorizing_run_id=$1),
					'calls',(select jsonb_agg(to_jsonb(c) order by physical_call_id) from physical_calls c where run_id=$1),
					'accounts',(select jsonb_agg(to_jsonb(a) order by account_id) from budget_accounts a))`, r.ID).Scan(&raw)
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}
			ageTerminalContent(t, h, r.ID)
			before := identities()
			if _, err := h.Store.CleanupTerminalContent(h.Ctx, 100, true); err != nil {
				t.Fatal(err)
			}
			router, err := httpapi.NewRouter(h.Service, map[string]httpapi.Identity{"review-key": {TenantID: r.TenantID, Role: "approver", ActorID: "synthetic-approver"}})
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(agentrun.ApprovalRequest{SchemaVersion: 1, Decision: decision, ProposalHash: view.ProposalHash})
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"", "approval-key", "new-alias"} {
				method := http.MethodPost
				if key == "" {
					method = http.MethodGet
				}
				request := httptest.NewRequest(method, "/v2/runs/"+r.ID+"/approval", bytes.NewReader(body))
				request.Header.Set("Authorization", "Bearer review-key")
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Idempotency-Key", key)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if response.Code != http.StatusGone || !bytes.Contains(response.Body.Bytes(), []byte(`"RESULT_EXPIRED"`)) {
					t.Fatalf("purged %s %s = %d %s", decision, method, response.Code, response.Body.String())
				}
			}
			if !bytes.Equal(before, identities()) {
				t.Fatal("purge/replay changed decision, actor, operations, signed action, calls, events or budgets")
			}
		})
	}
}

func TestRunContentCleanupPreservesUnknownFrozenAndLateSettlement(t *testing.T) {
	h := setupRunHarness(t)
	claimed := ledgerAtStep(t, h, "tenant-a", "s5-retention-unknown", "model_proposal")
	request := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
	ledgerReserve(t, h, request)
	if _, err := h.Store.Cancel(h.Ctx, "tenant-a", claimed.Lease.RunID, "cancel-retention"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Store.AcknowledgeStop(h.Ctx, h.Principal, claimed.Lease); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(h.Ctx, "update budget_accounts set frozen=true"); err != nil {
		t.Fatal(err)
	}
	ageTerminalContent(t, h, claimed.Lease.RunID)
	before := ledgerView(t, h, claimed.Lease)
	if before.TerminalAt == nil || before.Budget.Family.HeldCostMicroyuan <= 0 {
		t.Fatal("missing terminal or held cost")
	}
	dry, err := h.Store.CleanupTerminalContent(h.Ctx, 100, false)
	if err != nil || len(dry.RunIDs) != 1 || dry.ContentBytes == 0 {
		t.Fatal("dry run", dry, err)
	}
	if view := ledgerView(t, h, claimed.Lease); view.ContentPurgedAt != nil {
		t.Fatal("dry run mutated content")
	}
	var wg sync.WaitGroup
	results := make(chan agentrun.CleanupResult, 2)
	failures := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			result, err := h.Store.CleanupTerminalContent(h.Ctx, 100, true)
			results <- result
			failures <- err
		})
	}
	wg.Wait()
	close(results)
	close(failures)
	total := 0
	for result := range results {
		total += len(result.RunIDs)
	}
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if total != 1 {
		t.Fatalf("concurrent cleanup applied %d times", total)
	}
	after := ledgerView(t, h, claimed.Lease)
	if after.ContentPurgedAt == nil || after.State != before.State || !after.TerminalAt.Equal(*before.TerminalAt) ||
		!reflect.DeepEqual(before.Budget, after.Budget) || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatal("cleanup changed execution or exposure")
	}
	var retainedCalls, expiredSteps int
	if err := h.Pool.QueryRow(h.Ctx, `select (select count(*) from physical_calls where run_id=$1),
		(select count(*) from run_steps where run_id=$1 and output is null)`, after.ID).Scan(&retainedCalls, &expiredSteps); err != nil || retainedCalls == 0 || expiredSteps == 0 {
		t.Fatal("thin identity/content split", err)
	}
	if _, err := h.Store.Steps(h.Ctx, after.TenantID, after.ID, 0, 100); !errors.Is(err, agentrun.ErrResultExpired) {
		t.Fatal("steps did not expire", err)
	}
	if _, err := h.Store.Result(h.Ctx, after.TenantID, after.ID); !errors.Is(err, agentrun.ErrResultExpired) {
		t.Fatal("result did not expire", err)
	}
	if _, err := h.Store.Approval(h.Ctx, after.TenantID, after.ID); !errors.Is(err, agentrun.ErrResultExpired) {
		t.Fatal("approval did not expire", err)
	}
	if _, err := h.Store.Steps(h.Ctx, "tenant-b", after.ID, 0, 100); !errors.Is(err, agentrun.ErrNotFound) {
		t.Fatal("expiry leaked cross-tenant existence", err)
	}
	usage := ledgerUsage(10, 2)
	settlement := agentrun.SettleUsageRequest{Lease: claimed.Lease, PhysicalCallID: request.PhysicalCallID, Usage: &usage}
	settled, err := h.Store.SettleUsage(h.Ctx, h.Principal, settlement)
	if err != nil || !settled.NewlySettled {
		t.Fatal("late usage lost identity after content cleanup", err)
	}
	view := ledgerView(t, h, claimed.Lease)
	if !view.Budget.Family.Frozen || view.Budget.Family.KnownCostMicroyuan == 0 || view.State != after.State || !view.ContentPurgedAt.Equal(*after.ContentPurgedAt) {
		t.Fatal("late usage changed execution or thawed account")
	}
	if _, err := h.Pool.Exec(h.Ctx, "update physical_calls set reserved_at=clock_timestamp()-interval '30 days' where physical_call_id=$1", request.PhysicalCallID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Store.SettleUsage(h.Ctx, h.Principal, settlement); !errors.Is(err, agentrun.ErrCallSettlementExpired) {
		t.Fatal("expired settlement reopened", err)
	}
	if _, err := h.Pool.Exec(h.Ctx, "delete from runs where run_id=$1", after.ID); err == nil {
		t.Fatal("foreign keys no longer preserve audit identity")
	}
}

func TestRunContentCleanupKeepsActivePendingAndAcceptedReplay(t *testing.T) {
	h := setupRunHarness(t)
	old := h.submit(t, "tenant-a", "s5-retention-key")
	if _, err := h.Store.Cancel(h.Ctx, old.TenantID, old.ID, "cancel-old"); err != nil {
		t.Fatal(err)
	}
	ageTerminalContent(t, h, old.ID)
	request := agentrun.SubmitRequest{SchemaVersion: 1, TicketID: "ticket-1", BusinessRequestKey: old.BusinessRequestKey, ProfileID: h.Profile.ID, BudgetBatchID: "contract-batch", RunTimeoutSeconds: 3600}
	beforeCapture := h.Capture.count()
	replay, err := h.Service.Submit(h.Ctx, old.TenantID, "replay-before-cleanup", request)
	if err != nil || !replay.Reused || replay.Run.ID != old.ID {
		t.Fatal("window expiry broke retained accepted replay", err)
	}
	if _, err := h.Store.CleanupTerminalContent(h.Ctx, 100, true); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"submit-s5-retention-key", "replay-before-cleanup", "new-submit-alias"} {
		if _, err := h.Service.Submit(h.Ctx, old.TenantID, key, request); !errors.Is(err, agentrun.ErrRequestExpired) {
			t.Fatal("old key recreated or hid expiration", key, err)
		}
	}
	if h.Capture.count() != beforeCapture {
		t.Fatal("expired replay captured a new snapshot")
	}
	if _, err := h.Service.Retry(h.Ctx, old.TenantID, old.ID, "expired-retry", agentrun.RetryRequest{SchemaVersion: 1, RunTimeoutSeconds: 3600}); err == nil {
		t.Fatal("expired identity recreated a retry")
	}
	// A second owned database exercises pending approval and active content.
	p := setupRunHarness(t)
	claimed := ledgerAtStep(t, p, "tenant-a", "s5-retention-pending", "submit_proposal")
	_, committed := checkpointAdvance(t, p, &claimed, "proposal", false)
	if committed.State != agentrun.AwaitingApproval {
		t.Fatal("pending fixture")
	}
	active := p.submit(t, "tenant-b", "s5-retention-active")
	if _, err := p.Pool.Exec(p.Ctx, `update runs set created_at=clock_timestamp()-interval '9 days'`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Pool.Exec(p.Ctx, `update budget_accounts set valid_from=clock_timestamp()-interval '10 days',valid_until=clock_timestamp()-interval '1 day' where scope='batch'`); err != nil {
		t.Fatal(err)
	}
	result, err := p.Store.CleanupTerminalContent(p.Ctx, 100, true)
	if err != nil || len(result.RunIDs) != 0 {
		t.Fatal("active or approval Run purged", result, err)
	}
	for id, tenant := range map[string]string{active.ID: "tenant-b", claimed.Lease.RunID: "tenant-a"} {
		r, err := p.Store.Get(p.Ctx, tenant, id)
		if err != nil || r.ContentPurgedAt != nil {
			t.Fatal("active content gone", err)
		}
	}
}
