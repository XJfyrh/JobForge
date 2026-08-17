//go:build scale

// Package scale provides the large-scale reliability suite that closes PRD
// v0.2 GAP-1 (FR-601/602/603, AT-13/AT-14) at the literal NFR-001/002 scale:
//
//   - TestScaleAT13WorkerKillRounds: N rounds (default 100) of Worker kill /
//     lease expiry injection; non-terminal jobs must never be silently lost.
//   - TestScaleAT14IdempotentTenThousand: 10,000 jobs with duplicate delivery;
//     duplicate business side effects must be zero (demo.idempotent_effect).
//
// The suite is isolated from the default test run via the `scale` build tag
// (FR-603): plain `go test ./...` never executes these tests. Run with:
//
//	go test -tags scale -count=1 -timeout 60m ./tests/scale/
//
// Scale parameters (environment variables):
//
//	JOBFORGE_SCALE_KILL_ROUNDS         rounds for AT-13        (default 100)
//	JOBFORGE_SCALE_KILL_JOBS_PER_ROUND jobs killed per round   (default 10)
//	JOBFORGE_SCALE_IDEMPOTENT_JOBS     total jobs for AT-14    (default 10000)
//	JOBFORGE_SCALE_WORKERS             concurrent worker pool  (default 8)
//
// Like the default integration suite, a real PostgreSQL is required. On
// Windows set JOBFORGE_TEST_DSN (see AGENTS.md / docs/development.md); on
// Linux CI a testcontainers PostgreSQL is started automatically.
package scale

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store"
	"github.com/xjfyrh/jobforge/internal/store/postgres"
)

// scaleParams holds the resolved scale configuration.
type scaleParams struct {
	killRounds       int
	killJobsPerRound int
	idempotentJobs   int
	workers          int
}

// testEnv holds the shared test infrastructure.
var (
	testEnv *testEnvironment
	params  scaleParams
)

type testEnvironment struct {
	container *tcpostgres.PostgresContainer // nil when using direct DSN
	pool      *pgxpool.Pool
	dsn       string
}

// envInt reads an integer environment variable with a default fallback.
func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		fmt.Fprintf(os.Stderr, "invalid %s=%q, falling back to %d\n", key, v, def)
		return def
	}
	return n
}

// TestMain sets up the PostgreSQL connection once for all scale tests.
func TestMain(m *testing.M) {
	if os.Getenv("JOBFORGE_TEST_WORKER_HELPER") == "1" {
		os.Exit(m.Run())
	}

	ctx := context.Background()

	params = scaleParams{
		killRounds:       envInt("JOBFORGE_SCALE_KILL_ROUNDS", 100),
		killJobsPerRound: envInt("JOBFORGE_SCALE_KILL_JOBS_PER_ROUND", 10),
		idempotentJobs:   envInt("JOBFORGE_SCALE_IDEMPOTENT_JOBS", 10000),
		workers:          envInt("JOBFORGE_SCALE_WORKERS", 8),
	}

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

Bootstrap a local PostgreSQL first, then point the tests at it:

    docker compose -f deploy/compose.yaml up -d postgres
    set JOBFORGE_TEST_DSN=postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable
    go test -tags scale -count=1 -timeout 60m ./tests/scale/

See docs/development.md for details.`
			fmt.Fprintln(os.Stderr, msg)
			os.Exit(1)
		}

		// Testcontainers mode (Linux CI).
		var err error
		pgContainer, err = tcpostgres.Run(ctx,
			"postgres:16-alpine",
			tcpostgres.WithDatabase("jobforge_scale_test"),
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

// setupStore creates a fresh JobStore for a test.
func setupStore(_ *testing.T) *postgres.JobStore {
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
