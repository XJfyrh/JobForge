package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xjfyrh/jobforge/internal/migrate"
	"github.com/xjfyrh/jobforge/migrations"
)

var runMigrationTables = []string{
	"agent_profiles", "budget_accounts", "budget_batch_tenants", "business_requests",
	"worker_sessions", "execution_slots", "runs", "run_operations", "run_attempts",
	"run_steps", "run_events", "run_approvals", "tool_invocations", "physical_calls",
}

func TestRunMigrationUpDownReapplyPreservesLegacyJobs(t *testing.T) {
	ctx, pool := setupRunDB(t)
	assertRunMigrationTables(ctx, t, pool, true)
	downRunMigration(ctx, t, pool)
	assertRunMigrationTables(ctx, t, pool, false)
	jobID := uuid.NewString()
	if _, err := pool.Exec(ctx, `insert into jobs (id,tenant_id,queue,type,payload)
		values ($1,'historical-tenant','historical-queue','demo.echo','{"fixture":true}')`, jobID); err != nil {
		t.Fatalf("insert pre-0023 historical job: %v", err)
	}
	var before string
	if err := pool.QueryRow(ctx, "select to_jsonb(j)::text from jobs j where id=$1", jobID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"up", "re-up"} {
		if err := migrate.New(pool, testLogger(t)).Up(ctx); err != nil {
			t.Fatalf("0023 %s using production Migrator: %v", phase, err)
		}
		assertRunMigrationTables(ctx, t, pool, true)
		var after string
		if err := pool.QueryRow(ctx, "select to_jsonb(j)::text from jobs j where id=$1", jobID).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if before != after {
			t.Fatalf("0023 %s changed the historical job", phase)
		}
		if phase == "up" {
			downRunMigration(ctx, t, pool)
			assertRunMigrationTables(ctx, t, pool, false)
			if err := pool.QueryRow(ctx, "select to_jsonb(j)::text from jobs j where id=$1", jobID).Scan(&after); err != nil || after != before {
				t.Fatalf("0023 down changed the historical job: %v", err)
			}
		}
	}
	// A repeated production Up must leave the same ledger and rows untouched.
	if err := migrate.New(pool, testLogger(t)).Up(ctx); err != nil {
		t.Fatal(err)
	}
	var copies int
	if err := pool.QueryRow(ctx, "select count(*) from schema_migrations where version=23").Scan(&copies); err != nil || copies != 1 {
		t.Fatalf("0023 migration ledger copies=%d error=%v", copies, err)
	}
}

func assertRunMigrationTables(ctx context.Context, t *testing.T, pool *pgxpool.Pool, present bool) {
	t.Helper()
	for _, table := range runMigrationTables {
		var exists bool
		if err := pool.QueryRow(ctx, "select to_regclass($1) is not null", "public."+table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists != present {
			t.Fatalf("table %s presence=%v want=%v", table, exists, present)
		}
		if present {
			var count int
			if err := pool.QueryRow(ctx, "select count(*) from "+pgx.Identifier{table}.Sanitize()).Scan(&count); err != nil || count != 0 {
				t.Fatalf("new table %s has rows=%d error=%v", table, count, err)
			}
		}
	}
}

func downRunMigration(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	auditDown, err := migrations.FS.ReadFile("0024_provider_audit_report.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	content, err := migrations.FS.ReadFile("0023_create_agent_runs.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, string(auditDown)); err != nil {
		t.Fatalf("0024 down migration before 0023: %v", err)
	}
	if _, err = tx.Exec(ctx, string(content)); err != nil {
		t.Fatalf("0023 down migration: %v", err)
	}
	if _, err = tx.Exec(ctx, "delete from schema_migrations where version in (23,24)"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

type runMigrationFixture struct {
	RunID, BusinessRequestID, FamilyID, TenantID, CallID string
}

// seedRunMigrationRows establishes real FK targets using SQL fixtures only.
// It does not claim to exercise production admission, execution or settlement.
func seedRunMigrationRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool) runMigrationFixture {
	t.Helper()
	f := runMigrationFixture{RunID: uuid.NewString(), BusinessRequestID: uuid.NewString(),
		FamilyID: uuid.NewString(), TenantID: "migration-tenant", CallID: uuid.NewString()}
	tenantAccount, batchAccount := uuid.NewString(), uuid.NewString()
	for index, scope := range []string{"family", "tenant", "batch"} {
		id := []string{f.FamilyID, tenantAccount, batchAccount}[index]
		if _, err := pool.Exec(ctx, `insert into budget_accounts
			(account_id,scope,scope_key,valid_from,valid_until,limit_chat,limit_logical_tools,
			limit_query_embedding,limit_profile_metadata_http,limit_business_tool_http,
			limit_physical_http,limit_protocol_corrections,limit_tokens,limit_cost_microyuan)
			values ($1,$2,$2,clock_timestamp(),clock_timestamp()+interval '1 day',
			12,8,8,16,8,44,1,10000,10000)`, id, scope); err != nil {
			t.Fatalf("seed budget account: %v", err)
		}
	}
	sessionID, stepID, toolID, snapshotID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	hash := strings.Repeat("a", 64)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	statements := []struct {
		sql  string
		args []any
	}{
		{`insert into agent_profiles (profile_id,profile_hash,definition) values ('migration-fixture-v1',$1,'{}')`, []any{hash}},
		{`insert into budget_batch_tenants (batch_account_id,tenant_id,tenant_account_id) values ($1,$2,$3)`, []any{batchAccount, f.TenantID, tenantAccount}},
		{`insert into worker_sessions (session_id,worker_id,startup_id,version,created_at,seen_at,expires_at)
			values ($1,'migration-worker',$2,'test',clock_timestamp(),clock_timestamp(),clock_timestamp()+interval '60 seconds')`, []any{sessionID, uuid.NewString()}},
		{`insert into business_requests (business_request_id,tenant_id,business_request_key,request_hash,
			root_run_id,family_account_id,tenant_account_id,batch_account_id,created_at,retry_until)
			values ($1,$2,'migration-business-key',$3,$4,$5,$6,$7,statement_timestamp(),statement_timestamp()+interval '7 days')`,
			[]any{f.BusinessRequestID, f.TenantID, hash, f.RunID, f.FamilyID, tenantAccount, batchAccount}},
		{`insert into runs (run_id,tenant_id,business_request_id,business_request_key,ticket_id,
			admission_hash,profile_id,profile_hash,budget_batch_id,snapshot_id,snapshot_hash,version_vector,
			ticket_binding,index_id,index_profile_hash,state,run_timeout_seconds,run_deadline,next_step_id,
			next_step_kind,next_input_hash,created_at,updated_at)
			values ($1,$2,$3,'migration-business-key','ticket-fixture',$4,'migration-fixture-v1',$4,'batch',
			$5,$4,'{}','{}',$6,$4,'ready',3600,clock_timestamp()+interval '1 hour',$7,'search_policy',$4,
			clock_timestamp(),clock_timestamp())`, []any{f.RunID, f.TenantID, f.BusinessRequestID, hash, snapshotID, uuid.NewString(), stepID}},
		{`insert into run_operations (operation_id,tenant_id,operation_kind,operation_scope,operation_key,
			request_hash,result_run_id,created_at) values ($1,$2,'submit','submit','migration-submit-key',$3,$4,clock_timestamp())`,
			[]any{uuid.NewString(), f.TenantID, hash, f.RunID}},
		{`insert into run_attempts (tenant_id,run_id,attempt_no,worker_id,session_id,fencing_token,started_at,deadline)
			values ($1,$2,1,'migration-worker',$3,1,clock_timestamp(),clock_timestamp()+interval '180 seconds')`, []any{f.TenantID, f.RunID, sessionID}},
		{`insert into run_steps (tenant_id,run_id,attempt_no,sequence,step_id,kind,input_hash,profile_hash,snapshot_hash,
			commit_hash,output_ref,output,output_bytes,cursor_version,created_at)
			values ($1,$2,1,1,$3,'search_policy',$4,$4,$4,$4,'run-step:fixture:1','{}',2,1,clock_timestamp())`, []any{f.TenantID, f.RunID, stepID, hash}},
		{`insert into run_events (tenant_id,run_id,sequence,event_type,state,attempt_no,cursor_version,created_at)
			values ($1,$2,1,'submitted','ready',0,0,clock_timestamp())`, []any{f.TenantID, f.RunID}},
		{`insert into run_approvals (tenant_id,run_id,status,proposal_hash,proposal_ref,snapshot_id,snapshot_hash,
			version_vector,permission_expires_at,created_at)
			values ($1,$2,'pending',$3,'run-proposal:fixture',$4,$3,'{}',clock_timestamp()+interval '1 hour',clock_timestamp())`, []any{f.TenantID, f.RunID, hash, snapshotID}},
		{`insert into tool_invocations (invocation_id,tenant_id,run_id,step_id,attempt_no,fencing_token,tool_name,input_hash,created_at)
			values ($1,$2,$3,$4,1,1,'search_policy',$5,clock_timestamp())`, []any{toolID, f.TenantID, f.RunID, stepID, hash}},
		{`insert into physical_calls (physical_call_id,tenant_id,run_id,step_id,step_kind,attempt_no,worker_id,session_id,
			fencing_token,tool_invocation_id,kind,subcall,ordinal,input_hash,profile_hash,price_hash,reserved_tokens,
			reserved_cost_microyuan,status,reserved_at,dispatch_expires_at,call_deadline)
			values ($1,$2,$3,$4,'search_policy',1,'migration-worker',$5,1,$6,'query_embedding','query_embedding',1,$7,$7,$7,
			100,100,'unknown',clock_timestamp(),clock_timestamp()+interval '5 seconds',clock_timestamp()+interval '10 seconds')`,
			[]any{f.CallID, f.TenantID, f.RunID, stepID, sessionID, toolID, hash}},
	}
	for index, statement := range statements {
		if _, err = tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("seed migration relation %d: %v", index, err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatalf("commit cyclic Run identity: %v", err)
	}
	return f
}

func TestRunMigrationTenantForeignKeysAndNoCascade(t *testing.T) {
	ctx, pool := setupRunDB(t)
	f := seedRunMigrationRows(ctx, t, pool)
	// Clone each child with another tenant but the original Run. UUID-only FKs
	// would accept these records and permit cross-tenant audit/checkpoint binding.
	for _, test := range []struct{ table, standaloneID string }{
		{"runs", "run_id"}, {"run_operations", "operation_id"}, {"run_attempts", ""},
		{"run_steps", ""}, {"run_events", ""}, {"run_approvals", ""},
		{"tool_invocations", "invocation_id"}, {"physical_calls", "physical_call_id"},
	} {
		t.Run(test.table, func(t *testing.T) {
			table := pgx.Identifier{test.table}.Sanitize()
			patch := `jsonb_build_object('tenant_id','other-tenant')`
			var args []any
			if test.standaloneID != "" {
				patch += ` || jsonb_build_object($1::text,$2::text)`
				args = []any{test.standaloneID, uuid.NewString()}
			}
			_, err := pool.Exec(ctx, "insert into "+table+" select (jsonb_populate_record(null::"+table+",to_jsonb(original) || "+patch+")).* from "+table+" original limit 1", args...)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23503" || pgErr.TableName != test.table {
				t.Fatalf("cross-tenant %s must violate its FK, got %v", test.table, err)
			}
		})
	}
	var cascades int
	if err := pool.QueryRow(ctx, `select count(*) from pg_constraint
		where contype='f' and confdeltype='c' and conrelid in
		(select oid from pg_class where relnamespace='public'::regnamespace and relname=any($1))`, runMigrationTables).Scan(&cascades); err != nil || cascades != 0 {
		t.Fatalf("Run/account audit foreign keys cascade=%d error=%v", cascades, err)
	}
	for _, test := range []struct{ table, column, id string }{
		{"runs", "run_id", f.RunID}, {"business_requests", "business_request_id", f.BusinessRequestID},
		{"budget_accounts", "account_id", f.FamilyID},
	} {
		_, err := pool.Exec(ctx, "delete from "+pgx.Identifier{test.table}.Sanitize()+" where "+pgx.Identifier{test.column}.Sanitize()+"=$1", test.id)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
			t.Fatalf("deleting referenced %s must retain audit: %v", test.table, err)
		}
	}
	var unknownCalls int
	if err := pool.QueryRow(ctx, "select count(*) from physical_calls where physical_call_id=$1 and status='unknown'", f.CallID).Scan(&unknownCalls); err != nil || unknownCalls != 1 {
		t.Fatalf("unknown call audit rows=%d error=%v", unknownCalls, err)
	}
}

func TestRunMigrationIntegerBudgetBounds(t *testing.T) {
	ctx, pool := setupRunDB(t)
	f := seedRunMigrationRows(ctx, t, pool)
	columns := []string{"known_tokens", "known_cost_microyuan", "held_tokens", "held_cost_microyuan"}
	for _, suffix := range []string{"chat", "logical_tools", "query_embedding", "profile_metadata_http", "business_tool_http", "physical_http", "protocol_corrections", "tokens", "cost_microyuan"} {
		columns = append(columns, "limit_"+suffix, "used_"+suffix)
	}
	for _, column := range columns {
		for _, invalid := range []int64{-1, 9007199254740992} {
			t.Run(fmt.Sprintf("account_%s_%d", column, invalid), func(t *testing.T) {
				_, err := pool.Exec(ctx, "update budget_accounts set "+pgx.Identifier{column}.Sanitize()+"=$1 where account_id=$2", invalid, f.FamilyID)
				assertRunNumericConstraint(t, err)
			})
		}
	}
	for _, column := range []string{"reserved_tokens", "reserved_cost_microyuan", "known_tokens", "known_cost_microyuan"} {
		for _, invalid := range []int64{-1, 9007199254740992} {
			t.Run(fmt.Sprintf("call_%s_%d", column, invalid), func(t *testing.T) {
				_, err := pool.Exec(ctx, "update physical_calls set "+pgx.Identifier{column}.Sanitize()+"=$1 where physical_call_id=$2", invalid, f.CallID)
				assertRunNumericConstraint(t, err)
			})
		}
	}
	// Both individual safe-integer edges and overflow in the stored exposure sum
	// must be checked by real PostgreSQL, without leaving partial account updates.
	if _, err := pool.Exec(ctx, `update budget_accounts set limit_tokens=9007199254740991,
		used_tokens=9007199254740991,held_tokens=9007199254740991 where account_id=$1`, f.FamilyID); err != nil {
		t.Fatalf("exact maximum safe integer rejected: %v", err)
	}
	_, err := pool.Exec(ctx, `update budget_accounts set known_tokens=9223372036854775807,
		held_tokens=1 where account_id=$1`, f.FamilyID)
	assertRunNumericConstraint(t, err)
	var held, known, used int64
	if err = pool.QueryRow(ctx, "select held_tokens,known_tokens,used_tokens from budget_accounts where account_id=$1", f.FamilyID).Scan(&held, &known, &used); err != nil {
		t.Fatal(err)
	}
	if held != 9007199254740991 || known != 0 || used != held {
		t.Fatalf("failed overflow update changed exposure: held=%d known=%d used=%d", held, known, used)
	}
}

func assertRunNumericConstraint(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || (pgErr.Code != "23514" && pgErr.Code != "22003") {
		t.Fatalf("invalid budget integer must fail check/overflow, got %v", err)
	}
}
