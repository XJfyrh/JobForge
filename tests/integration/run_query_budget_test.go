package integration

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

func TestRunQueryPageBudgetMatchesGet(t *testing.T) {
	h := setupRunHarness(t)
	claimed := ledgerAtStep(t, h, "tenant-a", "page-ledger", "model_proposal")
	request := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
	ledgerReserve(t, h, request)
	// Include both a held call and unrelated empty Runs/accounts in the page.
	for i := range 22 {
		h.submit(t, "tenant-a", fmt.Sprintf("page-%02d", i))
	}
	h.submit(t, "tenant-b", "page-foreign")
	assertPage := func() {
		t.Helper()
		filter := agentrun.ListFilter{Limit: 20}
		count := 0
		for {
			page, err := h.Store.List(h.Ctx, "tenant-a", filter)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range page.Items {
				get, err := h.Store.Get(h.Ctx, "tenant-a", row.ID)
				if err != nil || !reflect.DeepEqual(row.Budget, get.Budget) || row.TenantID != "tenant-a" {
					t.Fatalf("page/Get budget mismatch: %s %v", row.ID, err)
				}
				count++
			}
			if page.NextCursor == nil {
				break
			}
			filter.Cursor = *page.NextCursor
		}
		if count != 23 {
			t.Fatalf("page count: %d", count)
		}
	}
	assertPage()
	ledgerObserve(t, h, request, "response", "accepted", ptrUsage(ledgerUsage(20, 10)))
	assertPage()
	// Compare the two production query paths over the same synthetic 20-Run data.
	const iterations = 10
	page, _ := h.Store.List(h.Ctx, "tenant-a", agentrun.ListFilter{Limit: 20})
	started := time.Now()
	for range iterations {
		for _, row := range page.Items {
			if _, err := h.Store.Get(h.Ctx, "tenant-a", row.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	gets := time.Since(started)
	started = time.Now()
	for range iterations {
		if _, err := h.Store.List(h.Ctx, "tenant-a", agentrun.ListFilter{Limit: 20}); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("20 Runs x %d: individual Get=%s; batched List=%s (includes page query)", iterations, gets, time.Since(started))
}

func ptrUsage(value agentrun.UsageReport) *agentrun.UsageReport { return &value }
