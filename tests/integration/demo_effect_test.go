package integration

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xjfyrh/jobforge/internal/migrate"
	"github.com/xjfyrh/jobforge/internal/worker/demo"
	"github.com/xjfyrh/jobforge/migrations"
)

func TestDemoIdempotentEffectStoreConcurrentApply(t *testing.T) {
	ctx := context.Background()
	store := demo.NewPostgresEffectStore(testEnv.pool)
	jobID := uuid.NewString()
	t.Cleanup(func() {
		_, _ = testEnv.pool.Exec(context.Background(),
			"delete from demo_idempotent_effects where job_id = $1", jobID)
	})

	const callers = 32
	results := make(chan demo.EffectResult, callers)
	errs := make(chan error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := store.Apply(ctx, jobID)
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Errorf("concurrent apply: %v", err)
	}
	if t.Failed() {
		t.FailNow()
	}
	applied := 0
	deduplicated := 0
	wantResultRef := "effect:" + jobID
	for result := range results {
		if result.ResultRef != wantResultRef {
			t.Errorf("result_ref=%q, want %q", result.ResultRef, wantResultRef)
		}
		if result.Applied {
			applied++
		} else {
			deduplicated++
		}
	}
	if applied != 1 || deduplicated != callers-1 {
		t.Fatalf("applied=%d deduplicated=%d", applied, deduplicated)
	}

	var rows int
	var storedRef string
	if err := testEnv.pool.QueryRow(ctx, `
		select count(*), min(result_ref)
		from demo_idempotent_effects
		where job_id = $1`, jobID).Scan(&rows, &storedRef); err != nil {
		t.Fatalf("query persistent effect: %v", err)
	}
	if rows != 1 || storedRef != wantResultRef {
		t.Fatalf("rows=%d stored_ref=%q", rows, storedRef)
	}
}

func TestDemoIdempotentEffectStoreDatabaseError(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testEnv.dsn)
	if err != nil {
		t.Fatalf("create disposable pool: %v", err)
	}
	pool.Close()

	_, err = demo.NewPostgresEffectStore(pool).Apply(ctx, uuid.NewString())
	if err == nil || !strings.Contains(err.Error(), "insert demo idempotent effect") {
		t.Fatalf("closed database error=%v", err)
	}
}

// TestMigration0018FromClean0017 verifies the production Migrator's 0017 to
// 0018 forward path plus the destructive down and second forward application.
// A dedicated schema isolates the test from the shared integration tables.
func TestMigration0018FromClean0017(t *testing.T) {
	ctx := context.Background()
	schema := "jobforge_migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	schemaIdentifier := pgx.Identifier{schema}.Sanitize()
	if _, err := testEnv.pool.Exec(ctx, "create schema "+schemaIdentifier); err != nil {
		t.Fatalf("create temporary migration schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testEnv.pool.Exec(context.Background(), "drop schema if exists "+schemaIdentifier+" cascade")
	})

	config, err := pgxpool.ParseConfig(testEnv.dsn)
	if err != nil {
		t.Fatalf("parse migration DSN: %v", err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("create migration pool: %v", err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, `
		create table schema_migrations (
			version integer primary key,
			name text not null,
			applied_at timestamptz not null default now()
		)`); err != nil {
		t.Fatalf("create migration ledger: %v", err)
	}
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		version, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			t.Fatalf("parse migration version %s: %v", name, err)
		}
		if version > 17 {
			continue
		}
		content, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin migration %s: %v", name, err)
		}
		if _, err := tx.Exec(ctx, string(content)); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("execute migration %s: %v", name, err)
		}
		if _, err := tx.Exec(ctx,
			"insert into schema_migrations (version, name) values ($1, $2)",
			version, name,
		); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("record migration %s: %v", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit migration %s: %v", name, err)
		}
	}

	migrator := migrate.New(pool, testLogger(t))
	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("migrate clean 0017 schema to 0018: %v", err)
	}
	verifyDemoEffectMigration(t, pool, true)

	downContent, err := migrations.FS.ReadFile("0018_create_demo_idempotent_effects.down.sql")
	if err != nil {
		t.Fatalf("read 0018 down migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(downContent)); err != nil {
		t.Fatalf("execute 0018 down migration: %v", err)
	}
	verifyDemoEffectMigration(t, pool, false)

	if _, err := pool.Exec(ctx, "delete from schema_migrations where version = 18"); err != nil {
		t.Fatalf("rewind 0018 ledger: %v", err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("reapply 0018 after rollback: %v", err)
	}
	verifyDemoEffectMigration(t, pool, true)
}

func verifyDemoEffectMigration(t *testing.T, pool *pgxpool.Pool, wantPresent bool) {
	t.Helper()
	ctx := context.Background()
	var present bool
	if err := pool.QueryRow(ctx,
		"select to_regclass('demo_idempotent_effects') is not null",
	).Scan(&present); err != nil {
		t.Fatalf("check demo effect table: %v", err)
	}
	if present != wantPresent {
		t.Fatalf("demo effect table present=%v, want %v", present, wantPresent)
	}
	if !present {
		return
	}

	var foreignKeys int
	if err := pool.QueryRow(ctx, `
		select count(*)
		from pg_constraint
		where conrelid = 'demo_idempotent_effects'::regclass
		  and contype = 'f'`).Scan(&foreignKeys); err != nil {
		t.Fatalf("query demo effect foreign keys: %v", err)
	}
	if foreignKeys != 0 {
		t.Fatalf("demo effect table has %d foreign keys, want 0", foreignKeys)
	}

	var version int
	if err := pool.QueryRow(ctx,
		"select version from schema_migrations where version = 18",
	).Scan(&version); err != nil {
		t.Fatalf("verify migration ledger: %v", err)
	}
	if version != 18 {
		t.Fatalf("migration version=%d", version)
	}
}
