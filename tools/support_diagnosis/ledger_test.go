package diagnosis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xjfyrh/jobforge/internal/run"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("JOBFORGE_TEST_DSN")
	if dsn == "" {
		t.Skip("JOBFORGE_TEST_DSN required for isolated real PG diagnosis tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := "jobforge_s5_diagnosis_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "create database "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(ctx, "drop database "+pgx.Identifier{name}.Sanitize())
		admin.Close()
	})
	if err := Initialize(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := Initialize(ctx, pool); err != nil {
		t.Fatal("migration replay", err)
	}
	return pool
}

func TestApprovedAmendmentRetainsParentAndSkipsSettledPrefix(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	m := testManifest()
	m.SchemaVersion = 1
	bindTest(t, pool, &m)
	for i := range m.Trials {
		m.Trials[i] = Trial{ID: fmt.Sprintf("case-%03d", i+1), RequestSHA256: strings.Repeat("b", 64)}
	}
	raw := encoded(t, m)
	parent, err := OpenLedger(ctx, pool, m, raw)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := parent.NextBatch(ctx)
	if err != nil {
		parent.Close()
		t.Fatal(err)
	}
	for i, trial := range m.Trials[:15] {
		if err := parent.Reserve(ctx, batch, trial, i+1); err != nil {
			parent.Close()
			t.Fatal(err)
		}
		r := ParseReport(encoded(t, responseFixture(false)), 200, "B", trial.ID)
		if i == 14 {
			r.Outcome, r.Stop = "thinking_not_observed", "thinking_not_observed"
		}
		if err := parent.Settle(ctx, trial, r, 1); err != nil {
			parent.Close()
			t.Fatal(err)
		}
	}
	parent.Close()
	amendment := m
	amendment.SchemaVersion, amendment.ExperimentID = 2, "test-amendment"
	amendment.Parent = &Parent{ExperimentID: m.ExperimentID, ManifestSHA256: Digest(raw), CompletedTrials: 15}
	wrong := amendment
	wrong.PriorCost++
	if l, err := OpenLedger(ctx, pool, wrong, encoded(t, wrong)); err == nil {
		l.Close()
		t.Fatal("changed financial base accepted")
	}
	l, err := OpenLedger(ctx, pool, amendment, encoded(t, amendment))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, trial := range m.Trials[:15] {
		if done, err := l.Settled(ctx, trial.ID); err != nil || !done {
			t.Fatal("parent observation lost", err)
		}
	}
	batch, err = l.NextBatch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Reserve(ctx, batch, m.Trials[0], 1); err == nil {
		t.Fatal("parent trial could be resent")
	}
	trial := m.Trials[15]
	if err := l.Reserve(ctx, batch, trial, 16); err != nil {
		t.Fatal(err)
	}
	if err := l.Settle(ctx, trial, ParseReport(encoded(t, responseFixture(false)), 200, "B", trial.ID), 1); err != nil {
		t.Fatal(err)
	}
	summary, err := l.Summary(ctx)
	if err != nil || summary["diagnosis_calls"] != int64(16) || summary["held_cost_microyuan"] != int64(0) {
		t.Fatal("amendment lost cumulative usage", err)
	}
	var count int
	var stop string
	if err := pool.QueryRow(ctx, `select stopped_reason,(select count(*) from diagnosis_calls where experiment_id=$1) from diagnosis_experiments where experiment_id=$1`, m.ExperimentID).Scan(&stop, &count); err != nil || stop != "thinking_not_observed" || count != 15 {
		t.Fatal("parent changed", err)
	}
	snapshot, err := l.Export(ctx)
	var exported struct {
		Parent *Parent          `json:"parent"`
		Calls  []map[string]any `json:"calls"`
	}
	if err != nil || json.Unmarshal(snapshot, &exported) != nil || exported.Parent == nil || exported.Parent.ManifestSHA256 != Digest(raw) || len(exported.Calls) != 16 {
		t.Fatal("amendment export lost parent bindings", err)
	}
}

func testManifest() Manifest {
	return Manifest{ExperimentID: "test-experiment", PriorCost: 4201411, CostCap: 20000000, BatchCap: 4000000,
		Sources:        map[string]string{},
		MaxInputTokens: maxInputTokens, MaxOutputTokens: maxOutputTokens, Trials: make([]Trial, 240),
		Pricing: run.Pricing{Hash: strings.Repeat("a", 64), Denominator: 1000000,
			InputMissMicroyuan: 2000000, InputHitMicroyuan: 40000, OutputMicroyuan: 8000000}}
}

func openTest(t *testing.T, pool *pgxpool.Pool, m Manifest) (*Ledger, []byte) {
	t.Helper()
	bindTest(t, pool, &m)
	raw, _ := json.Marshal(m)
	l, err := OpenLedger(context.Background(), pool, m, raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	return l, raw
}

func bindTest(t *testing.T, pool *pgxpool.Pool, m *Manifest) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), "select current_database(), database_id from diagnosis_database_identity where singleton").Scan(&m.DatabaseName, &m.DatabaseID); err != nil {
		t.Fatal(err)
	}
}

func TestLedgerSingleSenderReplayAndConflict(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	l, raw := openTest(t, pool, testManifest())
	if second, err := OpenLedger(ctx, pool, l.m, raw); err == nil {
		second.Close()
		t.Fatal("second sender acquired database lock")
	}
	batch, err := l.NextBatch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	trial := Trial{ID: "case-A-1", RequestSHA256: strings.Repeat("b", 64)}
	if err := l.Reserve(ctx, batch, trial, 1); err != nil {
		t.Fatal(err)
	}
	other := Trial{ID: "case-B-1", RequestSHA256: strings.Repeat("c", 64)}
	if err := l.Reserve(ctx, batch, other, 2); err == nil {
		t.Fatal("second active call allowed")
	}
	r := ParseReport(encoded(t, responseFixture(false)), 200, "A", trial.ID)
	if err := l.Settle(ctx, trial, r, 1); err != nil {
		t.Fatal(err)
	}
	if err := l.Settle(ctx, trial, r, 1); err != nil {
		t.Fatal("identical settlement replay", err)
	}
	exported, err := l.Export(ctx)
	var snapshot struct {
		DatabaseID string `json:"database_id"`
		Calls      []struct {
			RequestSHA256  string `json:"request_sha256"`
			ResponseSHA256 string `json:"response_sha256"`
			ReportSHA256   string `json:"report_sha256"`
		} `json:"calls"`
	}
	if err != nil || json.Unmarshal(exported, &snapshot) != nil || snapshot.DatabaseID != l.m.DatabaseID || len(snapshot.Calls) != 1 || snapshot.Calls[0].RequestSHA256 != trial.RequestSHA256 || snapshot.Calls[0].ResponseSHA256 != r.ResponseSHA256 || snapshot.Calls[0].ReportSHA256 != Digest(encoded(t, r)) {
		t.Fatal("PG export lost bindings", err)
	}
	if err := l.Reserve(ctx, batch, trial, 1); err == nil {
		t.Fatal("same trial resent")
	}
	summary, err := l.Summary(ctx)
	if err != nil || summary["diagnosis_calls"] != int64(1) || summary["held_cost_microyuan"] != int64(0) {
		t.Fatalf("ledger totals: %+v %v", summary, err)
	}
	r.Outcome = "different"
	if err := l.Settle(ctx, trial, r, 1); err == nil {
		t.Fatal("conflicting report accepted")
	}
	if err := l.Reserve(ctx, batch, other, 2); err == nil {
		t.Fatal("conflict did not stop future sends")
	}
}

func TestLedgerDatabaseIdentityCannotBeReplayed(t *testing.T) {
	pool := testPool(t)
	m := testManifest()
	bindTest(t, pool, &m)
	m.DatabaseID = uuid.NewString()
	if l, err := OpenLedger(context.Background(), pool, m, encoded(t, m)); err == nil {
		l.Close()
		t.Fatal("wrong database UUID accepted")
	}
	bindTest(t, pool, &m)
	m.DatabaseName += "_other"
	if l, err := OpenLedger(context.Background(), pool, m, encoded(t, m)); err == nil {
		l.Close()
		t.Fatal("wrong database name accepted")
	}
}

func TestLedgerUnknownRetainsHoldAndBlocksAllBatches(t *testing.T) {
	pool := testPool(t)
	l, _ := openTest(t, pool, testManifest())
	ctx := context.Background()
	batch, err := l.NextBatch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	trial := Trial{ID: "case-A-1", RequestSHA256: strings.Repeat("b", 64)}
	if err := l.Reserve(ctx, batch, trial, 1); err != nil {
		t.Fatal(err)
	}
	if err := l.Settle(ctx, trial, Report{Outcome: "transport_unknown", Stop: "transport_unknown"}, 1); err != nil {
		t.Fatal(err)
	}
	upper, _ := l.upper()
	summary, err := l.Summary(ctx)
	if err != nil || summary["held_cost_microyuan"] != upper.CostMicroyuan || summary["known_cost_microyuan"] != int64(0) {
		t.Fatalf("hold lost: %+v %v", summary, err)
	}
	if _, err := l.NextBatch(ctx); err == nil {
		t.Fatal("new batch bypassed unknown")
	}
}

func TestLedgerCumulativeCostPreventsAnotherReservation(t *testing.T) {
	pool := testPool(t)
	m := testManifest()
	m.PriorCost = 20000000 - 2228224
	l, _ := openTest(t, pool, m)
	ctx := context.Background()
	batch, err := l.NextBatch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	trial := Trial{ID: "case-A-1", RequestSHA256: strings.Repeat("b", 64)}
	if err := l.Reserve(ctx, batch, trial, 1); err != nil {
		t.Fatal(err)
	}
	if err := l.Settle(ctx, trial, ParseReport(encoded(t, responseFixture(false)), 200, "A", trial.ID), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := l.NextBatch(ctx); !errors.Is(err, run.ErrBudgetExhausted) {
		t.Fatalf("global cap ignored: %v", err)
	}
}

func TestLedgerCrashReservationCannotBeReplayed(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	m := testManifest()
	bindTest(t, pool, &m)
	raw, _ := json.Marshal(m)
	l, err := OpenLedger(ctx, pool, m, raw)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := l.NextBatch(ctx)
	if err != nil {
		l.Close()
		t.Fatal(err)
	}
	if err := l.Reserve(ctx, batch, Trial{ID: "case-A-1", RequestSHA256: strings.Repeat("b", 64)}, 1); err != nil {
		l.Close()
		t.Fatal(err)
	}
	l.Close()
	if next, err := OpenLedger(ctx, pool, m, raw); err == nil {
		next.Close()
		t.Fatal("crash hold allowed replay")
	}
	var held int64
	if err := pool.QueryRow(ctx, "select held_cost_microyuan from budget_accounts where account_id=$1", batch).Scan(&held); err != nil || held != 2228224 {
		t.Fatalf("crash hold %d: %v", held, err)
	}
}
