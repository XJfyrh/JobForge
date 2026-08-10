//go:build scale

package scale

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store"
)

// TestScaleQuotaMultiTenantFairness archives the NFR-304 step 4 multi-tenant
// mixed scenario: tenant A stays at its quota with a 10,000-job ready
// backlog while tenants B/C/D receive sustained traffic (20 jobs/s each)
// that is claimed and completed immediately. Per-tenant ready→claim
// percentiles, the reservation conflict count and claim-call durations
// (which include the counter row wait) are logged for archival in
// docs/benchmark.md. B/C/D must each meet the AT-22 fairness bounds
// (p95 <= 1s, max <= 2s); A's backlog must never be claimed. Correctness of
// the hard cap itself is AT-21's job and is not re-proven here.
func TestScaleQuotaMultiTenantFairness(t *testing.T) {
	js := setupStore(t)
	ctx := context.Background()

	const (
		limit      = 20
		duration   = 15 * time.Second
		submitRate = 20 // jobs per second per active tenant
		workers    = 8
	)
	suffix := uuid.New().String()[:8]
	tenantA := "fair-a-" + suffix
	tenants := []string{"fair-b-" + suffix, "fair-c-" + suffix, "fair-d-" + suffix}
	queue := "fair-q-" + suffix

	// Fill A's quota with running jobs, then add the 10,000 ready backlog.
	for i := 0; i < limit; i++ {
		enqueueTenantReadyJob(t, js, tenantA, queue)
	}
	filled, err := js.Claim(ctx, store.ClaimParams{
		Queues:            []string{queue},
		WorkerID:          "fair-filler-" + suffix,
		MaxJobs:           limit,
		LeaseTTL:          duration + time.Minute,
		TenantMaxInflight: limit,
		QuotaPrefilter:    true,
	})
	if err != nil || len(filled.Jobs) != limit {
		t.Fatalf("fill tenant A quota: claimed=%d err=%v", len(filled.Jobs), err)
	}
	if _, err := testEnv.pool.Exec(ctx,
		`insert into jobs (id, tenant_id, queue, type, state, run_at)
		 select gen_random_uuid(), $1, $2, 'demo.echo', 'ready', now() - interval '1 hour'
		 from generate_series(1, 10000)`, tenantA, queue); err != nil {
		t.Fatalf("insert A backlog: %v", err)
	}

	var (
		mu         sync.Mutex
		latencies  = make(map[string][]time.Duration) // tenant -> ready→claim samples
		conflicts  int64
		claimCalls []time.Duration
	)
	submitAt := sync.Map{} // job id -> time.Time

	// Producers: 20 jobs/s per active tenant.
	produceDone := make(chan struct{})
	go func() {
		defer close(produceDone)
		var wg sync.WaitGroup
		for _, tenant := range tenants {
			wg.Add(1)
			go func(tenant string) {
				defer wg.Done()
				ticker := time.NewTicker(time.Second / submitRate)
				defer ticker.Stop()
				for i := 0; i < int(duration.Seconds())*submitRate; i++ {
					<-ticker.C
					job := enqueueTenantReadyJob(t, js, tenant, queue)
					submitAt.Store(job.ID, time.Now())
				}
			}(tenant)
		}
		wg.Wait()
	}()

	// Consumers: claim and complete immediately.
	var stop atomic.Bool
	var consumeWG sync.WaitGroup
	for w := 0; w < workers; w++ {
		consumeWG.Add(1)
		go func(worker int) {
			defer consumeWG.Done()
			workerID := fmt.Sprintf("fair-w%d-%s", worker, suffix)
			for !stop.Load() {
				callStart := time.Now()
				res, err := js.Claim(ctx, store.ClaimParams{
					Queues:            []string{queue},
					WorkerID:          workerID,
					MaxJobs:           20,
					LeaseTTL:          time.Minute,
					TenantMaxInflight: limit,
					QuotaPrefilter:    true,
				})
				callLatency := time.Since(callStart)
				if err != nil {
					t.Errorf("worker %d claim: %v", worker, err)
					return
				}
				mu.Lock()
				claimCalls = append(claimCalls, callLatency)
				mu.Unlock()
				atomic.AddInt64(&conflicts, int64(res.QuotaConflicts))

				for _, j := range res.Jobs {
					if j.TenantID == tenantA {
						t.Errorf("tenant A job claimed while A is at quota")
						continue
					}
					now := time.Now()
					if ts, ok := submitAt.Load(j.ID); ok {
						mu.Lock()
						latencies[j.TenantID] = append(latencies[j.TenantID], now.Sub(ts.(time.Time)))
						mu.Unlock()
					}
					if err := js.Complete(ctx, j.ID, workerID, j.FencingToken, "", 1); err != nil {
						t.Errorf("worker %d complete: %v", worker, err)
					}
				}
				if len(res.Jobs) == 0 {
					time.Sleep(10 * time.Millisecond)
				}
			}
		}(w)
	}

	<-produceDone

	// Drain: keep consuming until the active tenants are empty.
	for {
		var remaining int
		if err := testEnv.pool.QueryRow(ctx,
			`select count(*) from jobs where tenant_id = any($1::text[]) and state not in ('succeeded', 'dead', 'cancelled')`,
			tenants).Scan(&remaining); err != nil {
			t.Fatalf("count remaining: %v", err)
		}
		if remaining == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop.Store(true)
	consumeWG.Wait()

	// Archival output.
	sort.Slice(claimCalls, func(i, j int) bool { return claimCalls[i] < claimCalls[j] })
	t.Logf("QUOTA-FAIRNESS: duration=%v workers=%d limit=%d conflicts=%d claim_calls=%d claim_call_p50=%v claim_call_p95=%v",
		duration, workers, limit, atomic.LoadInt64(&conflicts), len(claimCalls),
		percentile(claimCalls, 50), percentile(claimCalls, 95))
	for _, tenant := range tenants {
		mu.Lock()
		samples := latencies[tenant]
		mu.Unlock()
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		if len(samples) == 0 {
			t.Errorf("tenant %s: no latency samples", tenant)
			continue
		}
		p95 := percentile(samples, 95)
		maxLat := samples[len(samples)-1]
		t.Logf("QUOTA-FAIRNESS: tenant=%s samples=%d p50=%v p95=%v max=%v",
			tenant, len(samples), percentile(samples, 50), p95, maxLat)
		if p95 > time.Second {
			t.Errorf("tenant %s ready→claim p95 = %v, want <= 1s (AT-22 bound)", tenant, p95)
		}
		if maxLat > 2*time.Second {
			t.Errorf("tenant %s ready→claim max = %v, want <= 2s (AT-22 bound)", tenant, maxLat)
		}
	}
}

// enqueueTenantReadyJob enqueues one ready job for the given tenant and
// re-anchors run_at to the PostgreSQL clock (Docker/WSL2 drift safety).
func enqueueTenantReadyJob(t *testing.T, js store.JobStore, tenant, queue string) *domain.Job {
	t.Helper()
	id := uuid.New().String()
	pastRunAt := time.Now().Add(-1 * time.Second)
	job, err := domain.NewJob(id, domain.NewJobParams{
		TenantID: tenant,
		Queue:    queue,
		Type:     "demo.echo",
		Payload:  []byte(`{"fairness":true}`),
		RunAt:    &pastRunAt,
	}, time.Now())
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if _, err := js.Enqueue(context.Background(), job); err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	if _, err := testEnv.pool.Exec(context.Background(),
		`update jobs set run_at = now() - interval '10 seconds' where id = $1`, job.ID); err != nil {
		t.Fatalf("re-anchor run_at: %v", err)
	}
	return job
}
