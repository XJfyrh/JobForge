// Package integration provides integration tests for the JobForge store layer
// using a real PostgreSQL instance. These tests verify concurrent claim safety,
// idempotency, state transitions and fencing token enforcement.
//
// Two modes are supported:
//   - Direct DSN: set JOBFORGE_TEST_DSN to connect to an existing PostgreSQL
//     (e.g. from docker compose). Required on Windows where testcontainers-go
//     does not support Docker Desktop's rootless/WSL2 backend.
//   - Testcontainers: if JOBFORGE_TEST_DSN is empty, a PostgreSQL 16 container
//     is started automatically (Linux CI).
//
// Durable-event tests (PRD v0.3 §10) additionally need a Redis:
//   - Set JOBFORGE_TEST_REDIS_URL to use an existing broker (Windows compose
//     durable-events profile, PRD v0.3 §10.2).
//   - In testcontainers mode a Redis with AOF is started automatically and
//     JOBFORGE_TEST_REDIS_URL/JOBFORGE_TEST_REDIS_CONTAINER are exported.
//   - Without any Redis the durable tests skip (never fail).
package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store"
	"github.com/xjfyrh/jobforge/internal/store/postgres"
)

// testEnv holds the shared test infrastructure.
var testEnv *testEnvironment

type testEnvironment struct {
	container *tcpostgres.PostgresContainer // nil when using direct DSN
	pool      *pgxpool.Pool
	dsn       string
}

// TestMain sets up the PostgreSQL connection once for all tests.
func TestMain(m *testing.M) {
	// Worker crash helpers re-exec the current test binary. They must bypass
	// suite-level container/schema setup and run only their selected helper
	// test; all dependencies are provided explicitly by the parent process.
	if os.Getenv("JOBFORGE_TEST_WORKER_HELPER") == "1" {
		os.Exit(m.Run())
	}

	ctx := context.Background()

	var pool *pgxpool.Pool
	var dsn string
	var pgContainer *tcpostgres.PostgresContainer

	if envDSN := os.Getenv("JOBFORGE_TEST_DSN"); envDSN != "" {
		// Direct connection mode (Windows / pre-existing PostgreSQL).
		dsn = envDSN
		var err error
		pool, err = pgxpool.New(ctx, dsn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to create pool: %v\n", err)
			os.Exit(1)
		}
	} else {
		// Windows: testcontainers-go cannot drive Docker Desktop's
		// rootless/WSL2 backend, so fail fast with explicit bootstrap
		// instructions instead of surfacing a cryptic container error.
		if runtime.GOOS == "windows" {
			const msg = `JOBFORGE_TEST_DSN is not set and testcontainers mode is not supported on Windows:

testcontainers-go cannot drive Docker Desktop's rootless/WSL2 backend.
Bootstrap a local PostgreSQL first, then point the tests at it:

    docker compose -f deploy/compose.yaml up -d postgres
    set JOBFORGE_TEST_DSN=postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable
    go test ./tests/integration/...

See docs/development.md for details.`
			fmt.Fprintln(os.Stderr, msg)
			os.Exit(1)
		}

		// Testcontainers mode (Linux CI).
		var err error
		pgContainer, err = tcpostgres.Run(ctx,
			"postgres:16-alpine",
			tcpostgres.WithDatabase("jobforge_test"),
			tcpostgres.WithUsername("test"),
			tcpostgres.WithPassword("test"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(30*time.Second),
			),
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to start postgres container: %v\n", err)
			os.Exit(1)
		}

		dsn, err = pgContainer.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to get connection string: %v\n", err)
			os.Exit(1)
		}

		pool, err = pgxpool.New(ctx, dsn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to create pool: %v\n", err)
			os.Exit(1)
		}
	}

	// Apply migrations.
	if err := applyMigrations(ctx, pool); err != nil {
		fmt.Fprintf(os.Stderr, "failed to apply migrations: %v\n", err)
		os.Exit(1)
	}

	testEnv = &testEnvironment{
		container: pgContainer,
		pool:      pool,
		dsn:       dsn,
	}

	// Durable-event tests: in testcontainers mode (Linux CI) spin up an AOF
	// Redis so the suite runs the same Redis contract tests as Windows.
	// Direct-DSN environments provide their own broker via
	// JOBFORGE_TEST_REDIS_URL (compose durable-events profile).
	var redisContainer *tcredis.RedisContainer
	if os.Getenv("JOBFORGE_TEST_REDIS_URL") == "" && pgContainer != nil {
		rc, err := tcredis.Run(ctx, "redis:7-alpine",
			testcontainers.CustomizeRequest(testcontainers.GenericContainerRequest{
				ContainerRequest: testcontainers.ContainerRequest{
					Cmd: []string{"redis-server", "--appendonly", "yes", "--appendfsync", "everysec"},
				},
			}),
			testcontainers.WithWaitStrategy(wait.ForLog("Ready to accept connections").
				WithOccurrence(1).
				WithStartupTimeout(30*time.Second)),
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: testcontainers redis failed to start, durable tests will skip: %v\n", err)
		} else {
			redisContainer = rc
			endpoint, err := rc.Endpoint(ctx, "redis")
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: redis endpoint unavailable, durable tests will skip: %v\n", err)
			} else {
				_ = os.Setenv("JOBFORGE_TEST_REDIS_URL", endpoint)
				if name, err := rc.Name(ctx); err == nil {
					// docker stop/start based restarts (AT-17/NFR-303) target
					// this container; the name keeps its volumes alive.
					_ = os.Setenv("JOBFORGE_TEST_REDIS_CONTAINER", strings.TrimPrefix(name, "/"))
				}
			}
		}
	}

	code := m.Run()

	pool.Close()
	if redisContainer != nil {
		if err := redisContainer.Terminate(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "failed to terminate redis container: %v\n", err)
		}
	}
	if pgContainer != nil {
		if err := pgContainer.Terminate(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "failed to terminate container: %v\n", err)
		}
	}
	os.Exit(code)
}

// setupStore creates a fresh JobStore for a test. Each test gets a clean store
// but shares the same database (tables are truncated between tests if needed).
func setupStore(t *testing.T) *postgres.JobStore {
	t.Helper()
	return postgres.NewJobStore(testEnv.pool)
}

// claimJobs unwraps the store.ClaimResult for tests that only need the
// claimed jobs. Tests asserting quota internals call Claim directly.
func claimJobs(ctx context.Context, s store.JobStore, params store.ClaimParams) ([]*domain.Job, error) {
	res, err := s.Claim(ctx, params)
	if err != nil {
		return nil, err
	}
	return res.Jobs, nil
}

// applyMigrations reads and executes all .up.sql migration files in order.
// It uses DROP TABLE IF EXISTS to ensure a clean schema on repeated runs
// against the same database (direct DSN mode).
func applyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	_, filename, _, _ := runtime.Caller(0)
	migrationsDir := filepath.Join(filepath.Dir(filename), "..", "..", "migrations")

	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}

	// Clean slate: drop tables in reverse dependency order for direct DSN mode.
	cleanup := `drop table if exists demo_idempotent_effects,
		consumer_demo_effects, consumer_inbox, consumer_inbox_binding,
		tenant_quota_counters, scheduler_leadership, outbox_events,
		job_attempts, workers, jobs cascade`
	if _, err := pool.Exec(ctx, cleanup); err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		// Only apply .up.sql files.
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}

		content, err := os.ReadFile(filepath.Join(migrationsDir, name))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		if _, err := pool.Exec(ctx, string(content)); err != nil {
			return fmt.Errorf("execute migration %s: %w", name, err)
		}
	}

	return nil
}
