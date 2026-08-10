package integration

import (
	"context"
	"strings"
	"testing"
)

// Fixture inserts for the index plan tests. Tenant, state and row count are
// bound parameters; the run_at placement is selected between two constant
// statements.
const insertIndexTestJobsPast = `
insert into jobs (id, tenant_id, queue, type, state, run_at)
select gen_random_uuid(), $1, 'idx-queue', 'demo.echo', $2, now() - interval '1 hour'
from generate_series(1, $3)`

const insertIndexTestJobsFuture = `
insert into jobs (id, tenant_id, queue, type, state, run_at)
select gen_random_uuid(), $1, 'idx-queue', 'demo.echo', $2, now() + interval '1 hour'
from generate_series(1, $3)`

// insertIndexTestJobs bulk-inserts n jobs in the given state for the tenant.
// runAtPast places run_at in the past (eligible for promotion) when true.
// Direct SQL keeps the fixture cheap at thousands of rows.
func insertIndexTestJobs(t *testing.T, tenant, state string, n int, runAtPast bool) {
	t.Helper()
	q := insertIndexTestJobsFuture
	if runAtPast {
		q = insertIndexTestJobsPast
	}
	_, err := testEnv.pool.Exec(context.Background(), q, tenant, state, n)
	if err != nil {
		t.Fatalf("bulk insert %d %s jobs: %v", n, state, err)
	}
}

// explainAnalyze runs EXPLAIN (ANALYZE) and returns the plan as one string.
func explainAnalyze(t *testing.T, q string) string {
	t.Helper()
	rows, err := testEnv.pool.Query(context.Background(), "explain (analyze) "+q)
	if err != nil {
		t.Fatalf("explain %q: %v", q, err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan explain row: %v", err)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain rows: %v", err)
	}
	return b.String()
}

// explainWithSeqScanOff runs EXPLAIN (ANALYZE) on a dedicated connection with
// enable_seqscan = off, forcing the planner to use an index when one can
// serve the query. This makes the index-coverage assertion deterministic
// regardless of table statistics in the shared test database.
func explainWithSeqScanOff(t *testing.T, q string) string {
	t.Helper()
	ctx := context.Background()
	conn, err := testEnv.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "set enable_seqscan = off"); err != nil {
		t.Fatalf("set enable_seqscan: %v", err)
	}
	rows, err := conn.Query(ctx, "explain (analyze) "+q)
	if err != nil {
		t.Fatalf("explain %q: %v", q, err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan explain row: %v", err)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain rows: %v", err)
	}
	return b.String()
}

// promoteInnerSelect mirrors the candidates CTE of promoteReady
// (scheduler_queries.go) without the locking clause, which EXPLAIN ANALYZE
// does not require to exercise the same index choice.
const promoteInnerSelect = `
select id from jobs
where state in ('scheduled', 'retry_wait') and run_at <= now()
order by run_at asc limit 100`

// TestPromoteScanUsesPartialIndex verifies migration 0011's
// idx_jobs_promote_ready serves the Scheduler's promote scan instead of a
// sequential scan over the whole jobs table.
func TestPromoteScanUsesPartialIndex(t *testing.T) {
	ctx := context.Background()

	insertIndexTestJobs(t, "idx-promote-tenant", "succeeded", 3000, true)
	insertIndexTestJobs(t, "idx-promote-tenant", "scheduled", 40, true)
	insertIndexTestJobs(t, "idx-promote-tenant", "retry_wait", 10, true)
	if _, err := testEnv.pool.Exec(ctx, "analyze jobs"); err != nil {
		t.Fatalf("analyze jobs: %v", err)
	}

	// Deterministic coverage: with sequential scans disabled, the promote
	// scan must be served by the partial index (and without a Sort node,
	// since run_at leads the index).
	plan := explainWithSeqScanOff(t, promoteInnerSelect)
	if !strings.Contains(plan, "idx_jobs_promote_ready") {
		t.Errorf("promote scan does not use idx_jobs_promote_ready:\n%s", plan)
	}
	if strings.Contains(plan, "Seq Scan") {
		t.Errorf("promote scan falls back to Seq Scan with the index present:\n%s", plan)
	}

	// Natural plan: realistic statistics must lead the planner to the same
	// index without forcing.
	plan = explainAnalyze(t, promoteInnerSelect)
	if !strings.Contains(plan, "idx_jobs_promote_ready") {
		t.Errorf("natural promote plan does not pick idx_jobs_promote_ready:\n%s", plan)
	}
}

// TestTenantInflightCountUsesPartialIndex verifies migration 0014's
// idx_jobs_tenant_inflight serves the inflight-caliber queries of the quota
// counter design (reconcile aggregates and AT-21 sampling; ADR-0007 §5).
// The running-only idx_jobs_tenant_running from migration 0012 is dropped by
// migration 0015 together with its consumer.
func TestTenantInflightCountUsesPartialIndex(t *testing.T) {
	ctx := context.Background()

	insertIndexTestJobs(t, "idx-quota-tenant", "succeeded", 3000, true)
	insertIndexTestJobs(t, "idx-quota-tenant", "running", 20, true)
	insertIndexTestJobs(t, "idx-quota-tenant", "cancelling", 10, true)
	if _, err := testEnv.pool.Exec(ctx, "analyze jobs"); err != nil {
		t.Fatalf("analyze jobs: %v", err)
	}

	const q = `select count(*) from jobs where tenant_id = 'idx-quota-tenant' and state in ('running', 'cancelling')`

	plan := explainWithSeqScanOff(t, q)
	if !strings.Contains(plan, "idx_jobs_tenant_inflight") {
		t.Errorf("inflight count does not use idx_jobs_tenant_inflight:\n%s", plan)
	}
	if strings.Contains(plan, "Seq Scan") {
		t.Errorf("inflight count falls back to Seq Scan with the index present:\n%s", plan)
	}

	plan = explainAnalyze(t, q)
	if !strings.Contains(plan, "idx_jobs_tenant_inflight") {
		t.Errorf("natural inflight plan does not pick idx_jobs_tenant_inflight:\n%s", plan)
	}
}
