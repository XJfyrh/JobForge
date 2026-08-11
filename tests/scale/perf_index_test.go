//go:build scale

package scale

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/xjfyrh/jobforge/internal/store"
)

var errTenantInflightIndexMissing = errors.New("tenant inflight index is missing")

const (
	tenantInflightIndexName      = "idx_jobs_tenant_inflight"
	tenantInflightIndexKey       = "tenant_id"
	tenantInflightIndexPredicate = "(state = ANY (ARRAY['running'::text, 'cancelling'::text]))"
	perfInflightFixtureQueue     = "perf-inflight-plan-q"
	perfInflightTargetTenant     = "perf-inflight-0"
	perfInflightFixtureRows      = 4096
	perfInflightFixtureTenants   = 32
)

type indexCatalogQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Fixture inserts for the index performance test. All rows are generated
// server-side; run_at is anchored to the PostgreSQL clock so Docker/WSL2
// host clock drift cannot hide rows from run_at <= now() predicates.
const insertPerfScheduledJobs = `
insert into jobs (id, tenant_id, queue, type, state, run_at)
select gen_random_uuid(), 'perf-tenant', 'perf-q', 'demo.echo', 'scheduled',
       now() - interval '1 hour'
from generate_series(1, $1)`

const insertPerfReadyJobs = `
insert into jobs (id, tenant_id, queue, type, state, run_at)
select gen_random_uuid(), 'perf-tenant', 'perf-claim-q', 'demo.echo', 'ready',
       now() - interval '1 hour'
from generate_series(1, $1)`

const insertPerfInflightJobs = `
insert into jobs (id, tenant_id, queue, type, state, run_at)
select gen_random_uuid(),
       'perf-inflight-' || (series.n % $2)::text,
       'perf-inflight-plan-q',
       'demo.echo',
       case when series.n % 2 = 0 then 'running' else 'cancelling' end,
       now() - interval '1 hour'
from generate_series(1, $1) as series(n)`

// Constant EXPLAIN statements for the at-scale index spot checks; kept as
// full literal statements so no SQL is assembled at runtime.
const explainPromoteScan = `
explain (analyze)
select id from jobs
where state in ('scheduled', 'retry_wait') and run_at <= now()
order by run_at asc limit 1000`

// explainInflightCount covers the inflight-caliber queries of the quota
// counter design (reconcile aggregates, AT-21 sampling; ADR-0007 §5). It
// replaced the pre-M1 running-only quota count, whose idx_jobs_tenant_running
// consumer was removed by the counter table (migration 0015).
const explainInflightCount = `explain (analyze)
select count(*) from jobs
where tenant_id = 'perf-inflight-0' and state in ('running', 'cancelling')`

const queryTenantInflightIndexContract = `
select i.indisvalid,
       i.indisready,
       i.indnkeyatts,
       i.indnatts,
       coalesce(pg_get_indexdef(i.indexrelid, 1, false), ''),
       coalesce(pg_get_expr(i.indpred, i.indrelid, false), ''),
       pg_get_indexdef(i.indexrelid)
from pg_catalog.pg_index i
join pg_catalog.pg_class idx on idx.oid = i.indexrelid
join pg_catalog.pg_class tbl on tbl.oid = i.indrelid
join pg_catalog.pg_namespace ns on ns.oid = tbl.relnamespace
where ns.nspname = 'public'
  and tbl.relname = 'jobs'
  and idx.relname = 'idx_jobs_tenant_inflight'`

const deletePerfJobAttempts = `
delete from job_attempts a
using jobs j
where a.job_id = j.id
  and j.queue in ('perf-q', 'perf-claim-q', 'perf-inflight-plan-q')`

const deletePerfJobs = `
delete from jobs
where queue in ('perf-q', 'perf-claim-q', 'perf-inflight-plan-q')`

// explainCounterUpsert plans the exact reservation statement used by Claim
// (ADR-0007 §2) against a probe tenant. EXPLAIN does not execute it; the
// plan's conflict-resolution line must reference the primary key constraint,
// proving the counter table and its conflict target exist.
const explainCounterUpsert = `explain
insert into tenant_quota_counters (tenant_id, inflight, updated_at)
values ('perf-counter-probe', 1, now())
on conflict (tenant_id) do update
set inflight = tenant_quota_counters.inflight + 1,
    updated_at = now()
where tenant_quota_counters.inflight + 1 <= 5
returning inflight`

// assertCounterTableUsable verifies tenant_quota_counters is present and its
// primary key serves the reservation upsert's conflict target.
func assertCounterTableUsable(t *testing.T) {
	t.Helper()
	rows, err := testEnv.pool.Query(context.Background(), explainCounterUpsert)
	if err != nil {
		t.Fatalf("explain counter upsert: %v", err)
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
	if plan := b.String(); !strings.Contains(plan, "tenant_quota_counters_pkey") {
		t.Fatalf("counter reservation does not target tenant_quota_counters_pkey:\n%s", plan)
	}
}

// TestScalePerfPromoteClaimLatency measures the two hot paths served by the
// migrations 0011/0014 partial indexes and reports p50/p95 latencies:
//
//   - Scheduler promote batches (idx_jobs_promote_ready) at scale;
//   - concurrent Claims with the tenant quota reservation active
//     (tenant_quota_counters conditional upsert; PRD v0.3 NFR-304 "reserved
//     but not blocking" caliber: the limit is high enough to never refuse a
//     claim while the pre-filter and reservation paths actually execute).
//
// Assertions stay structural (everything promoted/claimed exactly once,
// natural plans use the indexes); latency numbers are logged for comparison
// because dev hardware (Docker/WSL2) cannot sustain hard thresholds.
//
// NFR-304 baseline migration: the pre-quota Phase B (running-only count on
// idx_jobs_tenant_running) was archived in docs/benchmark.md ("M1 前基线")
// before this rewrite; comparisons use those archived rounds.
func TestScalePerfPromoteClaimLatency(t *testing.T) {
	ctx := context.Background()
	if err := cleanupPerfIndexFixtures(ctx); err != nil {
		t.Fatalf("clean stale perf-index fixtures: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanupPerfIndexFixtures(context.Background()); err != nil {
			t.Errorf("clean perf-index fixtures: %v", err)
		}
	})
	js := setupStore(t)
	ss := setupScaleSchedulerStore(t)
	assertTenantInflightIndexContractGate(t)

	n := envInt("JOBFORGE_SCALE_PERF_JOBS", 20000)
	t.Logf("PERF-INDEX: jobs per phase = %d, workers = %d", n, params.workers)

	// ---- Phase A: promote latency (idx_jobs_promote_ready) ----

	// Baseline: leftover eligible rows from earlier scale tests may be
	// promoted too; assertions use >= n.
	var baseline int
	if err := testEnv.pool.QueryRow(ctx,
		`select count(*) from jobs where state in ('scheduled', 'retry_wait') and run_at <= now()`).
		Scan(&baseline); err != nil {
		t.Fatalf("count baseline eligible jobs: %v", err)
	}

	if _, err := testEnv.pool.Exec(ctx, insertPerfScheduledJobs, n); err != nil {
		t.Fatalf("insert scheduled jobs: %v", err)
	}
	if _, err := testEnv.pool.Exec(ctx, "analyze jobs"); err != nil {
		t.Fatalf("analyze jobs: %v", err)
	}

	// Natural-plan spot check at scale: the promote scan must pick the
	// partial index without forcing.
	assertPlanUsesIndex(t, explainPromoteScan, "idx_jobs_promote_ready")

	const promoteBatch = 1000
	var promoteLatencies []time.Duration
	promotedTotal := 0
	promoteStart := time.Now()
	for {
		callStart := time.Now()
		count, err := ss.PromoteReady(ctx, promoteBatch)
		if err != nil {
			t.Fatalf("promote ready: %v", err)
		}
		promoteLatencies = append(promoteLatencies, time.Since(callStart))
		promotedTotal += count
		if count == 0 {
			break
		}
	}
	promoteElapsed := time.Since(promoteStart)

	if promotedTotal < n+baseline {
		t.Fatalf("promote missed jobs: promoted %d, want >= %d (n=%d + baseline=%d)",
			promotedTotal, n+baseline, n, baseline)
	}
	p50, p95 := latencyPercentiles(promoteLatencies)
	t.Logf("PERF-INDEX promote: batches=%d batch=%d promoted=%d total=%v p50=%v p95=%v",
		len(promoteLatencies), promoteBatch, promotedTotal, promoteElapsed.Round(time.Millisecond), p50, p95)

	// ---- Phase B: claim latency with quota reservation active (counter upsert) ----

	if _, err := testEnv.pool.Exec(ctx, insertPerfReadyJobs, n); err != nil {
		t.Fatalf("insert ready jobs: %v", err)
	}
	if _, err := testEnv.pool.Exec(
		ctx, insertPerfInflightJobs, perfInflightFixtureRows, perfInflightFixtureTenants,
	); err != nil {
		t.Fatalf("insert inflight plan fixture: %v", err)
	}
	t.Cleanup(func() {
		if _, err := testEnv.pool.Exec(
			context.Background(), "delete from jobs where queue = $1", perfInflightFixtureQueue,
		); err != nil {
			t.Errorf("clean inflight plan fixture: %v", err)
		}
	})
	if _, err := testEnv.pool.Exec(ctx, "analyze jobs"); err != nil {
		t.Fatalf("analyze jobs: %v", err)
	}

	var targetInflight int
	if err := testEnv.pool.QueryRow(ctx, `
		select count(*) from jobs
		where tenant_id = $1 and state in ('running', 'cancelling')
	`, perfInflightTargetTenant).Scan(&targetInflight); err != nil {
		t.Fatalf("count inflight plan fixture: %v", err)
	}
	if targetInflight == 0 {
		t.Fatal("inflight plan fixture is empty")
	}

	// The catalog check above proves the dedicated index contract. This plan
	// check only enforces the performance property: with realistic multi-tenant
	// inflight data, the natural plan must not scan the whole jobs table. The
	// optimizer remains free to choose any legal index or bitmap path.
	assertPlanAvoidsJobsSeqScan(t, explainInflightCount)
	assertCounterTableUsable(t)
	if _, err := testEnv.pool.Exec(
		ctx, "delete from jobs where queue = $1", perfInflightFixtureQueue,
	); err != nil {
		t.Fatalf("delete inflight plan fixture: %v", err)
	}
	if _, err := testEnv.pool.Exec(ctx, "analyze jobs"); err != nil {
		t.Fatalf("analyze jobs after fixture cleanup: %v", err)
	}

	var (
		mu             sync.Mutex
		claimedIDs     = make(map[string]struct{}, n)
		claimLatencies []time.Duration
	)

	var wg sync.WaitGroup
	for w := 0; w < params.workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			workerID := fmt.Sprintf("perf-worker-%d", worker)
			emptyStreak := 0
			for emptyStreak < 3 {
				callStart := time.Now()
				jobs, err := claimJobs(ctx, js, store.ClaimParams{
					Queues:   []string{"perf-claim-q"},
					WorkerID: workerID,
					Types:    []string{"demo.echo"},
					MaxJobs:  50,
					LeaseTTL: 5 * time.Minute,
					// NFR-304 "reserved but not blocking": high enough to
					// never refuse a claim, but > 0 so the pre-filter and the
					// conditional counter reservation run for every candidate.
					TenantMaxInflight: n + 100,
					QuotaPrefilter:    true,
				})
				latency := time.Since(callStart)
				if err != nil {
					t.Errorf("worker %s claim: %v", workerID, err)
					return
				}
				mu.Lock()
				claimLatencies = append(claimLatencies, latency)
				if len(jobs) == 0 {
					emptyStreak++
				} else {
					emptyStreak = 0
					for _, j := range jobs {
						if _, dup := claimedIDs[j.ID]; dup {
							mu.Unlock()
							t.Errorf("job %s claimed twice", j.ID)
							return
						}
						claimedIDs[j.ID] = struct{}{}
					}
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	if len(claimedIDs) != n {
		t.Fatalf("claimed %d jobs, want %d", len(claimedIDs), n)
	}
	c50, c95 := latencyPercentiles(claimLatencies)
	t.Logf("PERF-INDEX claim: calls=%d workers=%d claimed=%d p50=%v p95=%v",
		len(claimLatencies), params.workers, len(claimedIDs), c50, c95)
}

// latencyPercentiles returns p50 and p95 of the samples without mutating the
// caller's slice order assumptions. Reuses percentile from kill_test.go.
func latencyPercentiles(samples []time.Duration) (p50, p95 time.Duration) {
	if len(samples) == 0 {
		return 0, 0
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return percentile(sorted, 50), percentile(sorted, 95)
}

func cleanupPerfIndexFixtures(ctx context.Context) error {
	tx, err := testEnv.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin cleanup: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, query := range []string{
		deletePerfJobAttempts,
		deletePerfJobs,
		"delete from tenant_quota_counters where tenant_id = 'perf-tenant'",
	} {
		if _, err := tx.Exec(ctx, query); err != nil {
			return fmt.Errorf("execute cleanup: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit cleanup: %w", err)
	}
	return nil
}

func checkTenantInflightIndexContract(ctx context.Context, querier indexCatalogQuerier) error {
	var (
		valid      bool
		ready      bool
		keyCount   int16
		attrCount  int16
		key        string
		predicate  string
		definition string
	)
	err := querier.QueryRow(ctx, queryTenantInflightIndexContract).Scan(
		&valid, &ready, &keyCount, &attrCount, &key, &predicate, &definition,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: public.%s", errTenantInflightIndexMissing, tenantInflightIndexName)
	}
	if err != nil {
		return fmt.Errorf("query tenant inflight index contract: %w", err)
	}
	if !valid || !ready {
		return fmt.Errorf(
			"tenant inflight index is not usable: valid=%t ready=%t", valid, ready,
		)
	}
	if keyCount != 1 || attrCount != 1 || key != tenantInflightIndexKey {
		return fmt.Errorf(
			"tenant inflight index key mismatch: keys=%d attributes=%d key=%q definition=%q",
			keyCount, attrCount, key, definition,
		)
	}
	normalizedPredicate := strings.Join(strings.Fields(predicate), " ")
	if normalizedPredicate != tenantInflightIndexPredicate {
		return fmt.Errorf(
			"tenant inflight index predicate mismatch: got %q want %q",
			normalizedPredicate, tenantInflightIndexPredicate,
		)
	}
	return nil
}

func assertTenantInflightIndexContractGate(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if err := checkTenantInflightIndexContract(ctx, testEnv.pool); err != nil {
		t.Fatalf("tenant inflight index contract: %v", err)
	}

	tx, err := testEnv.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin missing-index probe: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "drop index public.idx_jobs_tenant_inflight"); err != nil {
		t.Fatalf("drop tenant inflight index in probe transaction: %v", err)
	}
	if err := checkTenantInflightIndexContract(ctx, tx); !errors.Is(err, errTenantInflightIndexMissing) {
		t.Fatalf("missing-index probe returned %v, want %v", err, errTenantInflightIndexMissing)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback missing-index probe: %v", err)
	}
	if err := checkTenantInflightIndexContract(ctx, testEnv.pool); err != nil {
		t.Fatalf("tenant inflight index contract after rollback: %v", err)
	}
}

// assertPlanUsesIndex runs the given EXPLAIN statement (a constant, fully
// literal query) and fails unless the natural plan references the expected
// index.
func assertPlanUsesIndex(t *testing.T, explainQuery, index string) {
	t.Helper()
	plan := explainPlan(t, explainQuery)
	if !strings.Contains(plan, index) {
		t.Fatalf("plan does not use %s:\n%s", index, plan)
	}
}

func assertPlanAvoidsJobsSeqScan(t *testing.T, explainQuery string) {
	t.Helper()
	plan := explainPlan(t, explainQuery)
	if strings.Contains(plan, "Seq Scan on jobs") || strings.Contains(plan, "Seq Scan on public.jobs") {
		t.Fatalf("plan scans the complete jobs table:\n%s", plan)
	}
}

func explainPlan(t *testing.T, explainQuery string) string {
	t.Helper()
	rows, err := testEnv.pool.Query(context.Background(), explainQuery)
	if err != nil {
		t.Fatalf("explain: %v", err)
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
