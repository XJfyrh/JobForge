package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestRunRecoveryQueryPlansUnderOriginalBatchLock(t *testing.T) {
	h := setupRecoveryHarness(t)
	old := recoveryAtDecision(t, h, "query-plan")
	recoveryChat(t, h, old, true)
	if _, err := h.Store.FailExecution(h.Ctx, h.Principal, old.Lease, currentRunStep(old), "TIMEOUT"); err != nil {
		t.Fatal(err)
	}
	recoveryReady(t, h, old.Lease.RunID, 3*time.Second)
	current := h.claim(t)
	call, _ := recoveryChat(t, h, current, true)
	recoveryCommit(t, h, &current, recoveryToolDecision(t, current, call.PhysicalCallID, "get_order"))
	// Extract the exact production SELECTs, including their actual projection.
	// Diagnostics contain plans/timing/buffers only, never protected row values.
	literal := func(file, pattern string) string {
		raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "run", "postgres", file))
		if err != nil {
			t.Fatal(err)
		}
		match := regexp.MustCompile(pattern).FindSubmatch(raw)
		if len(match) != 2 {
			t.Fatal("production query extraction failed")
		}
		return string(match[1])
	}
	columns := literal("ledger.go", "(?s)const callColumns = `(.+?)`")
	guard := "select " + columns + literal("provider_audit_guard.go", "(?s)rows, err := tx.Query\\(ctx, \"select \"\\+callColumns\\+`(.+?)`, batchID\\)")
	history := literal("recovery_guard.go", "(?s)rows, err := tx.Query\\(ctx, `(.+?)`, batchID\\)")
	tx, err := h.Pool.Begin(h.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(h.Ctx) }()
	batch := current.Checkpoint.Run.Budget.Batch.ID
	if _, err := tx.Exec(h.Ctx, "select account_id from budget_accounts where account_id=$1 for update", batch); err != nil {
		t.Fatal(err)
	}
	for _, query := range []struct{ name, sql string }{
		{"legacy_step_join", strings.Replace(guard, "s.step_id=c.step_id", "s.step_id=c.step_id and s.attempt_no=c.attempt_no", 1)},
		{"s3_step_join", guard},
		{"s3_recovery_history", history},
	} {
		var raw []byte
		if err := tx.QueryRow(h.Ctx, "explain (analyze,buffers,format json) "+query.sql, batch).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var plan []map[string]any
		if json.Unmarshal(raw, &plan) != nil || len(plan) != 1 {
			t.Fatal("invalid actual PostgreSQL plan")
		}
		t.Logf("%s: %s", query.name, raw)
	}
}
