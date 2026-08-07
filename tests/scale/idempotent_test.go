//go:build scale

package scale

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store"
	"github.com/xjfyrh/jobforge/internal/worker"
	"github.com/xjfyrh/jobforge/internal/worker/demo"
)

// at14BatchSize bounds memory/DB pressure per batch; the suite stays
// resumable batch-by-batch (FR-602: 套件可分批执行).
const at14BatchSize = 1000

const at14Queue = "scale-at14"

// effectRecorder wraps the real demo.IdempotentEffectHandler and counts how
// many times the business side effect actually fired per job. Any job with a
// count greater than 1 is a duplicate business side effect (NFR-002).
type effectRecorder struct {
	inner *demo.IdempotentEffectHandler

	mu      sync.Mutex
	effects map[string]int

	// dedupHits counts re-deliveries that the handler deduplicated.
	dedupHits atomic.Int64
}

func newEffectRecorder() *effectRecorder {
	return &effectRecorder{
		inner:   demo.NewIdempotentEffectHandler(),
		effects: make(map[string]int),
	}
}

// execute runs the registered handler and records the outcome.
func (r *effectRecorder) execute(job *domain.Job, fencingToken int64) (string, error) {
	claimed := &worker.ClaimedJob{
		ID:           job.ID,
		Queue:        job.Queue,
		Type:         job.Type,
		Payload:      job.Payload,
		Attempt:      job.Attempt,
		MaxAttempts:  job.MaxAttempts,
		FencingToken: fencingToken,
	}
	result, err := r.inner.Execute(context.Background(), claimed)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(result, "effect:") {
		r.mu.Lock()
		r.effects[job.ID]++
		r.mu.Unlock()
	} else if result == "deduplicated" {
		r.dedupHits.Add(1)
	}
	return result, nil
}

// duplicateEffects returns the number of jobs whose business side effect
// fired more than once.
func (r *effectRecorder) duplicateEffects() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	dup := 0
	for _, n := range r.effects {
		if n > 1 {
			dup++
		}
	}
	return dup
}

// drainAndExecute lets a pool of simulated workers claim every ready job in
// at14Queue, execute it via the recorder and (if ack is true) Complete it.
// With ack=false the workers "crash" after executing: no Complete is sent.
func drainAndExecute(t *testing.T, js store.JobStore, rec *effectRecorder, workersN int, ack bool) int {
	t.Helper()
	ctx := context.Background()
	var processed atomic.Int64

	var wg sync.WaitGroup
	for w := 0; w < workersN; w++ {
		wg.Add(1)
		workerID := uuid.New().String()
		go func() {
			defer wg.Done()
			for {
				claimed, err := js.Claim(ctx, store.ClaimParams{
					Queues:   []string{at14Queue},
					WorkerID: workerID,
					MaxJobs:  20,
					LeaseTTL: 60 * time.Second,
				})
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if len(claimed) == 0 {
					return
				}
				for _, job := range claimed {
					if _, err := rec.execute(job, job.FencingToken); err != nil {
						t.Errorf("execute %s: %v", job.ID, err)
						return
					}
					processed.Add(1)
					if ack {
						if err := js.Complete(ctx, job.ID, workerID, job.FencingToken, "scale-idempotent", 5); err != nil {
							t.Errorf("complete %s: %v", job.ID, err)
							return
						}
					}
					// ack=false: worker crashes before Complete RPC.
				}
			}
		}()
	}
	wg.Wait()
	return int(processed.Load())
}

// TestScaleAT14IdempotentTenThousand verifies AT-14 / FR-602 (NFR-002 at
// literal scale): 10,000 jobs of type demo.idempotent_effect each experience
// a duplicate delivery (crash before ACK, lease recovery, re-delivery); the
// duplicate business side effect count must be exactly zero.
//
// Each batch of at14BatchSize jobs:
//  1. Enqueue batch
//  2. Concurrent worker pool claims and executes (side effect fires), then
//     every worker "crashes" before Complete
//  3. Leases are force-expired (PostgreSQL clock) and scheduler recovery
//     returns all jobs to ready
//  4. Worker pool re-claims and re-executes: the handler must deduplicate
//     every re-delivery, then Complete
//  5. Batch must end with zero non-succeeded jobs
func TestScaleAT14IdempotentTenThousand(t *testing.T) {
	js := setupStore(t)
	ss := setupScaleSchedulerStore(t)
	ctx := context.Background()

	total := params.idempotentJobs
	workersN := params.workers
	t.Logf("AT-14: totalJobs=%d batchSize=%d workers=%d", total, at14BatchSize, workersN)

	rec := newEffectRecorder()
	start := time.Now()

	for base := 0; base < total; base += at14BatchSize {
		batch := at14BatchSize
		if base+batch > total {
			batch = total - base
		}

		// 1. Enqueue the batch.
		pastRunAt := time.Now().Add(-1 * time.Second)
		for i := 0; i < batch; i++ {
			job, err := domain.NewJob(uuid.New().String(), domain.NewJobParams{
				TenantID: "scale-tenant",
				Queue:    at14Queue,
				Type:     "demo.idempotent_effect",
				Payload:  []byte(`{"scale":true}`),
				RunAt:    &pastRunAt,
			}, time.Now())
			if err != nil {
				t.Fatalf("new job: %v", err)
			}
			if _, err := js.Enqueue(ctx, job); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
		}

		// 2. First delivery: execute, crash before ACK.
		first := drainAndExecute(t, js, rec, workersN, false)
		if first != batch {
			t.Fatalf("batch at %d: first delivery processed %d, want %d", base, first, batch)
		}

		// 3. Force lease expiry and recover everything back to ready.
		forceExpireRunningLeases(t, at14Queue)
		recovered := recoverUntilDrained(t, ss)
		if recovered < batch {
			t.Fatalf("batch at %d: recovered %d, want >= %d", base, recovered, batch)
		}

		// 4. Re-delivery: handler deduplicates, then workers ACK.
		second := drainAndExecute(t, js, rec, workersN, true)
		if second != batch {
			t.Fatalf("batch at %d: second delivery processed %d, want %d", base, second, batch)
		}

		// 5. Batch terminal-state check.
		if lost := countNonSucceeded(t, at14Queue); lost > 0 {
			t.Fatalf("batch at %d: %d jobs not succeeded after re-delivery", base, lost)
		}

		if (base/at14BatchSize)%5 == 4 {
			t.Logf("AT-14 progress: %d/%d jobs done (%s elapsed)", base+batch, total, time.Since(start).Round(time.Second))
		}
	}

	dedupHits := rec.dedupHits.Load()
	dupEffects := rec.duplicateEffects()
	t.Logf("AT-14 results: jobs=%d dedupHits=%d duplicateSideEffects=%d elapsed=%s",
		total, dedupHits, dupEffects, time.Since(start).Round(time.Second))

	if dedupHits != int64(total) {
		t.Errorf("AT-14: deduplicated re-deliveries = %d, want %d (every job must be re-delivered once)", dedupHits, total)
	}
	if dupEffects != 0 {
		t.Fatalf("AT-14 FAILED: duplicate business side effects = %d, want 0", dupEffects)
	}
	if lost := countNonSucceeded(t, at14Queue); lost > 0 {
		t.Fatalf("AT-14 FAILED: %d jobs not in succeeded state", lost)
	}
	t.Logf("AT-14 PASSED: %d jobs with duplicate delivery, zero duplicate business side effects", total)
}
