// Command e2e-bench runs end-to-end benchmarks for JobForge.
//
// It measures the complete job lifecycle: submit → claim → complete,
// reporting p50/p95/p99 latencies and throughput.
//
// Usage:
//
//	go run . -jobs=10000 -workers=4
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store"
	"github.com/xjfyrh/jobforge/internal/store/postgres"
)

var (
	flagJobs    = flag.Int("jobs", 1000, "number of jobs to process")
	flagWorkers = flag.Int("workers", 4, "number of concurrent workers")
	flagDSN     = flag.String("dsn", "", "PostgreSQL DSN (default: env JOBFORGE_TEST_DSN or localhost)")
)

func main() {
	flag.Parse()

	dsn := *flagDSN
	if dsn == "" {
		dsn = os.Getenv("JOBFORGE_TEST_DSN")
	}
	if dsn == "" {
		dsn = "postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable"
	}

	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("failed to create pool: %v", err)
	}
	defer pool.Close()

	js := postgres.NewJobStore(pool)

	fmt.Println("=== JobForge E2E Benchmark ===")
	fmt.Printf("Jobs: %d, Workers: %d\n", *flagJobs, *flagWorkers)
	fmt.Printf("Go version: %s\n", runtime.Version())
	fmt.Printf("GOMAXPROCS: %d\n", runtime.GOMAXPROCS(0))
	fmt.Println()

	// Record baseline goroutines.
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	baselineGoroutines := runtime.NumGoroutine()
	fmt.Printf("Baseline goroutines: %d\n", baselineGoroutines)

	// Phase 1: Submit all jobs.
	fmt.Println("\n--- Phase 1: Submit ---")
	queue := fmt.Sprintf("e2e-bench-%s", uuid.New().String()[:8])
	submitStart := time.Now()

	jobIDs := make([]string, *flagJobs)
	submitTimes := make([]time.Time, *flagJobs)

	for i := 0; i < *flagJobs; i++ {
		id := uuid.New().String()
		pastRunAt := time.Now().Add(-1 * time.Second)
		job, err := domain.NewJob(id, domain.NewJobParams{
			TenantID: "bench-tenant",
			Queue:    queue,
			Type:     "demo.noop",
			Payload:  []byte(`{}`),
			RunAt:    &pastRunAt,
		}, time.Now())
		if err != nil {
			log.Fatalf("create job: %v", err)
		}

		submitTimes[i] = time.Now()
		_, err = js.Enqueue(ctx, job)
		if err != nil {
			log.Fatalf("enqueue: %v", err)
		}
		jobIDs[i] = id
	}

	submitDuration := time.Since(submitStart)
	submitThroughput := float64(*flagJobs) / submitDuration.Seconds()
	fmt.Printf("Submit completed: %v (%.2f jobs/sec)\n", submitDuration, submitThroughput)

	// Phase 2: Process all jobs with concurrent workers.
	fmt.Println("\n--- Phase 2: Process ---")
	processStart := time.Now()

	var completed atomic.Int64
	var failed atomic.Int64
	latencies := make([]time.Duration, *flagJobs)
	var latencyIdx atomic.Int64

	var wg sync.WaitGroup
	jobChan := make(chan int, *flagJobs)

	// Start workers.
	for w := 0; w < *flagWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			wid := fmt.Sprintf("bench-worker-%d", workerID)

			for idx := range jobChan {
				start := time.Now()

				// Claim one job.
				claimRes, err := js.Claim(ctx, store.ClaimParams{
					Queues:   []string{queue},
					WorkerID: wid,
					MaxJobs:  1,
					LeaseTTL: 30 * time.Second,
				})
				if err != nil {
					failed.Add(1)
					continue
				}
				claimed := claimRes.Jobs
				if len(claimed) == 0 {
					// No jobs available, retry.
					jobChan <- idx
					time.Sleep(10 * time.Millisecond)
					continue
				}

				// Simulate minimal work (no-op handler).
				// In a real benchmark, this would execute the actual handler.

				// Complete the job.
				err = js.Complete(ctx, claimed[0].ID, wid, claimed[0].FencingToken, "", 0)
				if err != nil {
					failed.Add(1)
					continue
				}

				latency := time.Since(start)
				latIdx := latencyIdx.Add(1) - 1
				if latIdx < int64(len(latencies)) {
					latencies[latIdx] = latency
				}
				completed.Add(1)
			}
		}(w)
	}

	// Feed jobs to workers.
	for i := 0; i < *flagJobs; i++ {
		jobChan <- i
	}
	close(jobChan)

	wg.Wait()
	processDuration := time.Since(processStart)

	// Calculate statistics.
	actualLatencies := latencies[:latencyIdx.Load()]
	sort.Slice(actualLatencies, func(i, j int) bool {
		return actualLatencies[i] < actualLatencies[j]
	})

	fmt.Printf("Process completed: %v\n", processDuration)
	fmt.Printf("Completed: %d, Failed: %d\n", completed.Load(), failed.Load())
	fmt.Printf("Throughput: %.2f jobs/sec\n", float64(completed.Load())/processDuration.Seconds())

	if len(actualLatencies) > 0 {
		fmt.Println("\n--- Latency Percentiles ---")
		fmt.Printf("p50:  %v\n", percentile(actualLatencies, 50))
		fmt.Printf("p95:  %v\n", percentile(actualLatencies, 95))
		fmt.Printf("p99:  %v\n", percentile(actualLatencies, 99))
		fmt.Printf("min:  %v\n", actualLatencies[0])
		fmt.Printf("max:  %v\n", actualLatencies[len(actualLatencies)-1])
	}

	// Phase 3: Goroutine stability check.
	fmt.Println("\n--- Phase 3: Goroutine Stability ---")
	fmt.Println("Waiting 60 seconds for goroutine settling...")
	time.Sleep(60 * time.Second)

	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	finalGoroutines := runtime.NumGoroutine()

	diff := finalGoroutines - baselineGoroutines
	if diff < 0 {
		diff = -diff
	}
	tolerance := baselineGoroutines / 20 // 5% tolerance
	if tolerance < 5 {
		tolerance = 5
	}

	fmt.Printf("Baseline goroutines: %d\n", baselineGoroutines)
	fmt.Printf("Final goroutines: %d\n", finalGoroutines)
	fmt.Printf("Difference: %d (tolerance: ±%d)\n", finalGoroutines-baselineGoroutines, tolerance)

	if diff <= tolerance {
		fmt.Println("PASS: Goroutine stability within tolerance")
	} else {
		fmt.Println("WARN: Goroutine count outside tolerance (possible leak)")
	}

	fmt.Println("\n=== Benchmark Complete ===")
}

func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := (len(sorted) - 1) * p / 100
	return sorted[idx]
}
