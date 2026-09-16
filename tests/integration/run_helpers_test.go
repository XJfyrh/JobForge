package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xjfyrh/jobforge/internal/migrate"
)

// setupRunDB creates an owned database on the suite's existing PostgreSQL and
// applies the production embedded migrations. It never drops or reuses the DSN
// database. Tests must not mistake unavailable PostgreSQL or migration failure
// for a successful or skipped Run acceptance test.
func setupRunDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	if testEnv == nil || testEnv.pool == nil || testEnv.dsn == "" {
		t.Fatal("Run tests require the initialized real PostgreSQL test environment")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	t.Cleanup(cancel)
	config, err := pgxpool.ParseConfig(testEnv.dsn)
	if err != nil {
		t.Fatal("parse Run test PostgreSQL configuration")
	}
	name := "jobforge_run_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if name == config.ConnConfig.Database || !strings.HasPrefix(name, "jobforge_run_test_") {
		t.Fatal("refusing to reuse the PostgreSQL DSN database")
	}
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err = testEnv.pool.Exec(ctx, "create database "+quoted); err != nil {
		t.Fatalf("create owned Run test database: %v", err)
	}
	t.Cleanup(func() {
		// Test cancellation must not leave owned databases or leaked connections.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := testEnv.pool.Exec(cleanupCtx, "drop database "+quoted+" with (force)"); err != nil {
			t.Errorf("drop owned Run test database: %v", err)
		}
	})
	config.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("create isolated Run PostgreSQL pool")
	}
	t.Cleanup(pool.Close)
	if err = migrate.New(pool, testLogger(t)).Up(ctx); err != nil {
		t.Fatalf("apply production migrations to Run test database: %v", err)
	}
	return ctx, pool
}
