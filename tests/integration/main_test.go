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
	"github.com/testcontainers/testcontainers-go/wait"

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

	code := m.Run()

	pool.Close()
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
	cleanup := `drop table if exists scheduler_leadership, outbox_events, job_attempts, workers, jobs cascade`
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
