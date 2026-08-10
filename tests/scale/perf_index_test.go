//go:build scale

package scale

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xjfyrh/jobforge/internal/store"
)

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
select count(*) from jobs where tenant_id = 'perf-tenant' and state in ('running', 'cancelling')`

// setLocalSeqScanOff is applied in the same transaction as the inflight
// count explain. With few inflight rows the natural plan may drift to a
// different access path; with sequential scans disabled the dedicated
// partial index wins whenever it exists, so a missing index still fails the
// check.
const setLocalSeqScanOff = `set local enable_seqscan = off`

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
	js := setupStore(t)
	ss := setupScaleSchedulerStore(t)

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
	if _, err := testEnv.pool.Exec(ctx, "analyze jobs"); err != nil {
		t.Fatalf("analyze jobs: %v", err)
	}

	// Structural spot checks for the M1 structures: the inflight partial
	// index serves the inflight-caliber queries (seqscan disabled so the
	// assertion is deterministic), and the counter table primary key exists.
	assertPlanUsesIndexNoSeqScan(t, explainInflightCount, "idx_jobs_tenant_inflight")
	assertCounterTableUsable(t)

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

// assertPlanUsesIndex runs the given EXPLAIN statement (a constant, fully
// literal query) and fails unless the natural plan references the expected
// index.
func assertPlanUsesIndex(t *testing.T, explainQuery, index string) {
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
	if plan := b.String(); !strings.Contains(plan, index) {
		t.Fatalf("plan does not use %s:\n%s", index, plan)
	}
}

// assertPlanUsesIndexNoSeqScan runs the given EXPLAIN statement inside a
// transaction with enable_seqscan = off (SET LOCAL), then fails unless the
// plan references the expected index. Both statements are constants; no SQL
// is assembled at runtime.
func assertPlanUsesIndexNoSeqScan(t *testing.T, explainQuery, index string) {
	t.Helper()
	ctx := context.Background()
	tx, err := testEnv.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, setLocalSeqScanOff); err != nil {
		t.Fatalf("set enable_seqscan: %v", err)
	}
	rows, err := tx.Query(ctx, explainQuery)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			t.Fatalf("scan explain row: %v", err)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("explain rows: %v", err)
	}
	rows.Close()
	if plan := b.String(); !strings.Contains(plan, index) {
		t.Fatalf("plan does not use %s:\n%s", index, plan)
	}
}
