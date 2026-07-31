// Package micro provides micro-benchmarks for JobForge store operations.
// These benchmarks measure raw database operation throughput using testing.B.
//
// Run with: go test -bench=. -benchmem -benchtime=10s
package micro

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store/postgres"
)

var benchPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx := context.Background()

	dsn := os.Getenv("JOBFORGE_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable"
	}

	var err error
	benchPool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create pool: %v\n", err)
		os.Exit(1)
	}

	// Ensure schema exists (run migrations manually or assume already applied).
	code := m.Run()

	benchPool.Close()
	os.Exit(code)
}

// BenchmarkEnqueue measures single job insertion throughput.
func BenchmarkEnqueue(b *testing.B) {
	ctx := context.Background()
	js := postgres.NewJobStore(benchPool)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		id := uuid.New().String()
		pastRunAt := time.Now().Add(-1 * time.Second)
		job, err := domain.NewJob(id, domain.NewJobParams{
			TenantID: "bench-tenant",
			Queue:    "bench-enqueue",
			Type:     "demo.echo",
			Payload:  []byte(`{"benchmark":true}`),
			RunAt:    &pastRunAt,
		}, time.Now())
		if err != nil {
			b.Fatalf("create job: %v", err)
		}
		b.StartTimer()

		_, err = js.Enqueue(ctx, job)
		if err != nil {
			b.Fatalf("enqueue: %v", err)
		}
	}
}

// BenchmarkEnqueueParallel measures concurrent job insertion throughput.
func BenchmarkEnqueueParallel(b *testing.B) {
	ctx := context.Background()
	js := postgres.NewJobStore(benchPool)

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			id := uuid.New().String()
			pastRunAt := time.Now().Add(-1 * time.Second)
			job, err := domain.NewJob(id, domain.NewJobParams{
				TenantID: "bench-tenant",
				Queue:    "bench-enqueue-parallel",
				Type:     "demo.echo",
				Payload:  []byte(`{"benchmark":true}`),
				RunAt:    &pastRunAt,
			}, time.Now())
			if err != nil {
				b.Fatalf("create job: %v", err)
			}

			_, err = js.Enqueue(ctx, job)
			if err != nil {
				b.Fatalf("enqueue: %v", err)
			}
		}
	})
}

// BenchmarkEnqueueBatch measures batch insertion using a single transaction.
func BenchmarkEnqueueBatch(b *testing.B) {
	ctx := context.Background()

	batchSizes := []int{10, 50, 100}

	for _, batchSize := range batchSizes {
		b.Run(fmt.Sprintf("batch%d", batchSize), func(b *testing.B) {
			js := postgres.NewJobStore(benchPool)

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				for j := 0; j < batchSize; j++ {
					id := uuid.New().String()
					pastRunAt := time.Now().Add(-1 * time.Second)
					job, err := domain.NewJob(id, domain.NewJobParams{
						TenantID: "bench-tenant",
						Queue:    "bench-batch",
						Type:     "demo.echo",
						Payload:  []byte(`{"benchmark":true}`),
						RunAt:    &pastRunAt,
					}, time.Now())
					if err != nil {
						b.Fatalf("create job: %v", err)
					}

					_, err = js.Enqueue(ctx, job)
					if err != nil {
						b.Fatalf("enqueue: %v", err)
					}
				}
			}
		})
	}
}
