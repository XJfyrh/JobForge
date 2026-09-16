package integration

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/google/uuid"
	agentrun "github.com/xjfyrh/jobforge/internal/run"
	runpostgres "github.com/xjfyrh/jobforge/internal/run/postgres"
)

func supportInspectionRequest(t *testing.T, h *runHarness) runpostgres.SupportInspectionRequest {
	t.Helper()
	request := runpostgres.SupportInspectionRequest{ProfileID: h.Profile.ID, WorkerID: h.Principal}
	rows, err := h.Pool.Query(h.Ctx, `select account_id,scope,scope_key,valid_from,valid_until,
		jsonb_build_object('chat',limit_chat,'logical_tools',limit_logical_tools,'query_embedding',limit_query_embedding,
		'profile_metadata_http',limit_profile_metadata_http,'business_tool_http',limit_business_tool_http,
		'physical_http',limit_physical_http,'protocol_corrections',limit_protocol_corrections,'tokens',limit_tokens,'cost_microyuan',limit_cost_microyuan)
		from budget_accounts where scope in ('batch','tenant') order by scope,scope_key`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var budget agentrun.BudgetSpec
		var limits []byte
		if err := rows.Scan(&budget.ID, &budget.Scope, &budget.Key, &budget.ValidFrom, &budget.ValidUntil, &limits); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(limits, &budget.Limits); err != nil {
			t.Fatal(err)
		}
		request.Budgets = append(request.Budgets, budget)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	rows, err = h.Pool.Query(h.Ctx, "select tenant_id,batch_account_id,tenant_account_id from budget_batch_tenants order by tenant_id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var binding runpostgres.SupportBudgetBinding
		if err := rows.Scan(&binding.TenantID, &binding.BatchAccountID, &binding.TenantAccountID); err != nil {
			t.Fatal(err)
		}
		request.Bindings = append(request.Bindings, binding)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return request
}

func TestRunSupportInspectPreservesOriginalAccountsAndZeroUsageHistory(t *testing.T) {
	h := setupSupportProfileHarness(t, "support-inspection")
	request := supportInspectionRequest(t, h)
	fresh, err := h.Store.InspectSupport(h.Ctx, request)
	if err != nil || !fresh.MatchesConfig || !fresh.MigrationsReady || len(fresh.Accounts) != 3 || fresh.History.Runs != 0 || fresh.History.WorkerSessions != 0 {
		t.Fatalf("fresh prepared inspection: %+v %v", fresh, err)
	}
	supportProfileCapture(t, h, "submit", "submit-history", "", true)
	r := h.submit(t, "tenant-north", "history")
	if _, err := h.Store.Cancel(h.Ctx, "tenant-north", r.ID, "cancel-before-dispatch"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Store.Register(h.Ctx, h.Principal, uuid.NewString(), h.Profile.ExecutorVersion); err != nil {
		t.Fatal(err)
	}
	history, err := h.Store.InspectSupport(h.Ctx, request)
	if err != nil || !history.MatchesConfig || history.History.BusinessRequests != 1 || history.History.Runs != 1 || history.History.Calls != 0 ||
		history.History.WorkerSessions != 1 || history.History.WorkerStartups != 1 {
		t.Fatalf("zero-usage history lost: %+v %v", history, err)
	}
	if !reflect.DeepEqual(history.Accounts, fresh.Accounts) {
		t.Fatal("inspection/history changed original zero-usage budgets")
	}
	batchID := request.Bindings[0].BatchAccountID
	if _, err := h.Pool.Exec(h.Ctx, `update budget_accounts set used_tokens=17,known_tokens=7,held_tokens=10,
		used_cost_microyuan=23,known_cost_microyuan=11,held_cost_microyuan=12,frozen=true,batch_stop_code='PROVIDER_IDENTITY_INVALID' where account_id=$1`, batchID); err != nil {
		t.Fatal(err)
	}
	changed, err := h.Store.InspectSupport(h.Ctx, request)
	if err != nil || !changed.MatchesConfig {
		t.Fatal("mutable facts were confused with immutable setup", err)
	}
	if changed.Accounts[0].KnownTokens != 7 || changed.Accounts[0].HeldTokens != 10 || changed.Accounts[0].KnownCostMicroyuan != 11 ||
		changed.Accounts[0].HeldCostMicroyuan != 12 || !changed.Accounts[0].Frozen || changed.Accounts[0].BatchStopCode != agentrun.BatchStopProviderIdentityInvalid {
		t.Fatalf("actual budget facts not preserved: %+v", changed.Accounts[0])
	}
	again, err := h.Store.InspectSupport(h.Ctx, request)
	if err != nil || !reflect.DeepEqual(again.Accounts, changed.Accounts) {
		t.Fatal("inspection reset budget", err)
	}
	request.Budgets[0].Limits.Tokens++
	mismatch, err := h.Store.InspectSupport(h.Ctx, request)
	if err != nil || mismatch.MatchesConfig {
		t.Fatal("different immutable budget accepted", err)
	}
}

func TestRunSupportInspectMissingSetupDoesNotBootstrap(t *testing.T) {
	h := setupSupportProfileHarness(t, "support-inspection-missing")
	request := supportInspectionRequest(t, h)
	if _, err := h.Pool.Exec(h.Ctx, "delete from agent_profiles where profile_id=$1", h.Profile.ID); err != nil {
		t.Fatal(err)
	}
	missing, err := h.Store.InspectSupport(h.Ctx, request)
	if err != nil || missing.Profile.Registered || missing.MatchesConfig {
		t.Fatal("missing profile registered by inspection", err)
	}
	if _, err := h.Pool.Exec(h.Ctx, "delete from schema_migrations where version=24"); err != nil {
		t.Fatal(err)
	}
	missing, err = h.Store.InspectSupport(h.Ctx, request)
	if err != nil || missing.MigrationsReady || missing.MatchesConfig || len(missing.Accounts) != 0 {
		t.Fatal("missing migration repaired by inspection", err)
	}
	var count int
	if err := h.Pool.QueryRow(h.Ctx, "select count(*) from schema_migrations where version=24").Scan(&count); err != nil || count != 0 {
		t.Fatal("inspection migrated database", err)
	}
}

func TestRunSupportInspectCountsCallsThroughOriginalBatchAccount(t *testing.T) {
	h := setupRunHarness(t)
	claimed := ledgerAtStep(t, h, "tenant-a", "inspection-calls", "model_proposal")
	ledgerReserve(t, h, ledgerRequest(h, claimed, agentrun.SubcallChat, ""))
	request := supportInspectionRequest(t, h)
	result, err := h.Store.InspectSupport(h.Ctx, request)
	var calls int64
	if countErr := h.Pool.QueryRow(h.Ctx, "select count(*) from physical_calls where run_id=$1", claimed.Lease.RunID).Scan(&calls); countErr != nil {
		t.Fatal(countErr)
	}
	if err != nil || !result.MatchesConfig || result.History.Runs != 1 || result.History.Calls != calls || calls == 0 {
		t.Fatalf("original batch account calls not visible: %+v %v", result, err)
	}
}
