package integration

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/xjfyrh/jobforge/internal/migrate"
	agentrun "github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/migrations"
)

func TestRunRecoveryMigrationLockTimeoutLeavesNoPartialSchema(t *testing.T) {
	h := setupRecoveryHarness(t)
	claimed := recoveryAtDecision(t, h, "migration-lock")
	down, err := migrations.FS.ReadFile("0026_attempt_recovery_proof.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := h.Pool.Begin(h.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(h.Ctx, string(down)); err != nil {
		_ = tx.Rollback(h.Ctx)
		t.Fatal(err)
	}
	if _, err := tx.Exec(h.Ctx, "delete from schema_migrations where version=26"); err != nil {
		_ = tx.Rollback(h.Ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(h.Ctx); err != nil {
		t.Fatal(err)
	}
	blocker, err := h.Pool.Begin(h.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(h.Ctx) }()
	if _, err := blocker.Exec(h.Ctx, "select attempt_no from run_attempts where run_id=$1 for update", claimed.Lease.RunID); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(h.Ctx, 5*time.Second)
	defer cancel()
	started := time.Now()
	err = migrate.New(h.Pool, testLogger(t)).Up(bounded)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" || time.Since(started) < 1900*time.Millisecond || time.Since(started) > 4*time.Second {
		t.Fatalf("migration did not respect its actual 2s lock timeout: %v", err)
	}
	var columns, applied int
	if err := h.Pool.QueryRow(h.Ctx, "select (select count(*) from information_schema.columns where table_name='run_attempts' and column_name in ('recovery_step','recovery_ordinal')),(select count(*) from schema_migrations where version=26)").Scan(&columns, &applied); err != nil || columns != 0 || applied != 0 {
		t.Fatal("failed migration left partial schema or version")
	}
	if err := blocker.Rollback(h.Ctx); err != nil {
		t.Fatal(err)
	}
	if err := migrate.New(h.Pool, testLogger(t)).Up(h.Ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Store.GetCheckpoint(h.Ctx, h.Principal, claimed.Lease); err != nil {
		t.Fatal("reapply lost legacy authority")
	}
}

func TestRunRecoveryMigrationPreservesLegacyAndRefusesEvidenceLoss(t *testing.T) {
	h := setupRecoveryHarness(t)
	old := recoveryAtDecision(t, h, "migration-legacy")
	before := ledgerView(t, h, old.Lease)
	down, err := migrations.FS.ReadFile("0026_attempt_recovery_proof.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		tx, err := h.Pool.Begin(h.Ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(h.Ctx, string(down)); err != nil {
			_ = tx.Rollback(h.Ctx)
			t.Fatal(err)
		}
		if _, err := tx.Exec(h.Ctx, "delete from schema_migrations where version=26"); err != nil {
			_ = tx.Rollback(h.Ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(h.Ctx); err != nil {
			t.Fatal(err)
		}
		if err := migrate.New(h.Pool, testLogger(t)).Up(h.Ctx); err != nil {
			t.Fatal(err)
		}
		after := ledgerView(t, h, old.Lease)
		if !reflect.DeepEqual(before, after) {
			t.Fatal("down/reapply changed legacy execution or accounting")
		}
		var count int
		if err := h.Pool.QueryRow(h.Ctx, "select count(*) from run_attempts where recovery_step is not null or recovery_ordinal is not null").Scan(&count); err != nil || count != 0 {
			t.Fatal("migration backfilled proof")
		}
	}
	recoveryChat(t, h, old, true)
	if _, err := h.Store.FailExecution(h.Ctx, h.Principal, old.Lease, currentRunStep(old), "TIMEOUT"); err != nil {
		t.Fatal(err)
	}
	recoveryReady(t, h, old.Lease.RunID, 3*time.Second)
	if _, err := h.Pool.Exec(h.Ctx, string(down)); err == nil {
		t.Fatal("down discarded recorded recovery evidence")
	}
	if claimed := h.claim(t); claimed.Lease.AttemptNo != 2 {
		t.Fatal("failed rollback destroyed proof")
	}
}

func TestRunRecoveryMigrationRejectsPartialAndNonRetryProofs(t *testing.T) {
	h := setupRecoveryHarness(t)
	claimed := recoveryAtDecision(t, h, "migration-check")
	raw, _ := json.Marshal(currentRunStep(claimed))
	queries := []string{
		"update run_attempts set recovery_step=$2 where run_id=$1",
		"update run_attempts set recovery_ordinal=1 where run_id=$1",
		"update run_attempts set recovery_step=$2,recovery_ordinal=1 where run_id=$1",
		"update run_attempts set finished_at=clock_timestamp(),outcome='failed_terminal',error_code='TIMEOUT',recovery_step=$2,recovery_ordinal=1 where run_id=$1",
		"update run_attempts set finished_at=clock_timestamp(),outcome='failed_retry',error_code=null,recovery_step=$2,recovery_ordinal=1 where run_id=$1",
		"update run_attempts set finished_at=clock_timestamp(),outcome='failed_retry',error_code='EXECUTOR_PROTOCOL_ERROR',recovery_step=$2,recovery_ordinal=1 where run_id=$1",
		"update run_attempts set finished_at=clock_timestamp(),outcome='failed_retry',error_code='TIMEOUT',recovery_step=$2,recovery_ordinal=4 where run_id=$1",
	}
	for _, query := range queries {
		args := []any{claimed.Lease.RunID}
		if strings.Contains(query, "$2") {
			args = append(args, raw)
		}
		_, err := h.Pool.Exec(h.Ctx, query, args...)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("partial proof not rejected by CHECK: %v", err)
		}
	}
	for _, mutation := range []string{"{}", "null", `{"extra":1}`, strings.Replace(string(raw), `"cursor_version":1`, `"cursor_version":1.0`, 1), strings.Replace(string(raw), `"sequence":2`, `"sequence":3`, 1)} {
		_, err := h.Pool.Exec(h.Ctx, "update run_attempts set finished_at=clock_timestamp(),outcome='failed_retry',error_code='TIMEOUT',recovery_step=$2,recovery_ordinal=1 where run_id=$1", claimed.Lease.RunID, []byte(mutation))
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("wrong JSON proof not rejected: %v", err)
		}
	}
	if _, err := h.Store.FailExecution(h.Ctx, h.Principal, claimed.Lease, currentRunStep(claimed), "TIMEOUT"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Store.FailExecution(h.Ctx, h.Principal, claimed.Lease, currentRunStep(claimed), "TIMEOUT"); !errors.Is(err, agentrun.ErrStaleLease) {
		t.Fatalf("duplicate close: %v", err)
	}
}
