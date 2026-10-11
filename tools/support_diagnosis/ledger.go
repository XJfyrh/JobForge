package diagnosis

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xjfyrh/jobforge/internal/jsonstrict"
	"github.com/xjfyrh/jobforge/internal/migrate"
	"github.com/xjfyrh/jobforge/internal/run"
	runpg "github.com/xjfyrh/jobforge/internal/run/postgres"
)

//go:embed migrations/0001_diagnostic_calls.up.sql
var schema embed.FS

const ledgerLock int64 = 7545031101

// Ledger holds the sole diagnostic sender's database session lock.
type Ledger struct {
	pool *pgxpool.Pool
	conn *pgxpool.Conn
	m    Manifest
}

// Initialize applies additive migrations only in an explicitly isolated database.
func Initialize(ctx context.Context, pool *pgxpool.Pool) error {
	var allowed bool
	if err := pool.QueryRow(ctx, `select current_database() like 'jobforge_s5_diagnosis_%'`).Scan(&allowed); err != nil || !allowed {
		return ErrInvalid
	}
	if err := migrate.New(pool, slog.New(slog.NewTextHandler(io.Discard, nil))).Up(ctx); err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock($1)", ledgerLock); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `create table if not exists diagnosis_schema_migrations (version integer primary key, sha256 text not null)`); err != nil {
		return err
	}
	raw, _ := schema.ReadFile("migrations/0001_diagnostic_calls.up.sql")
	var hash string
	err = tx.QueryRow(ctx, "select sha256 from diagnosis_schema_migrations where version=1").Scan(&hash)
	if err == nil {
		if hash != Digest(raw) {
			return ErrInvalid
		}
	} else if errors.Is(err, pgx.ErrNoRows) {
		if _, err := tx.Exec(ctx, string(raw)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "insert into diagnosis_schema_migrations values(1,$1)", Digest(raw)); err != nil {
			return err
		}
	} else {
		return err
	}
	return tx.Commit(ctx)
}

// OpenLedger rejects concurrent senders and unresolved previous reservations.
func OpenLedger(ctx context.Context, pool *pgxpool.Pool, m Manifest, raw []byte) (*Ledger, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	l := &Ledger{pool: pool, conn: conn, m: m}
	var acquired bool
	if err := conn.QueryRow(ctx, "select pg_try_advisory_lock($1)", ledgerLock).Scan(&acquired); err != nil || !acquired {
		conn.Release()
		return nil, errors.New("diagnosis sender already active")
	}
	fail := func(err error) (*Ledger, error) { l.Close(); return nil, err }
	var bound bool
	if err := conn.QueryRow(ctx, `select current_database()=$1 and database_id=$2::uuid
		from diagnosis_database_identity where singleton`, m.DatabaseName, m.DatabaseID).Scan(&bound); err != nil || !bound {
		return fail(ErrInvalid)
	}
	var count int
	otherExperiments := 0
	if m.Parent != nil {
		otherExperiments = 1
		if err := l.verifyParent(ctx); err != nil {
			return fail(err)
		}
	}
	if err := conn.QueryRow(ctx, `select count(*) from diagnosis_experiments where experiment_id<>$1`, m.ExperimentID).Scan(&count); err != nil || count != otherExperiments {
		return fail(ErrInvalid)
	}
	if _, err := conn.Exec(ctx, `insert into diagnosis_experiments (experiment_id,manifest_sha256,manifest,prior_cost_microyuan,cost_cap_microyuan)
		values($1,$2,$3,$4,$5) on conflict do nothing`, m.ExperimentID, Digest(raw), raw, m.PriorCost, m.CostCap); err != nil {
		return fail(err)
	}
	var matches bool
	if err := conn.QueryRow(ctx, `select manifest_sha256=$2 and manifest=$3::jsonb and stopped_reason is null
		from diagnosis_experiments where experiment_id=$1`, m.ExperimentID, Digest(raw), raw).Scan(&matches); err != nil || !matches {
		return fail(ErrInvalid)
	}
	if err := conn.QueryRow(ctx, `select count(*) from diagnosis_calls where state<>'known'`).Scan(&count); err != nil || count != 0 {
		return fail(errors.New("unresolved diagnosis call; no automatic replay"))
	}
	return l, nil
}

func (l *Ledger) parentID() string {
	if l.m.Parent == nil {
		return ""
	}
	return l.m.Parent.ExperimentID
}

func (l *Ledger) verifyParent(ctx context.Context) error {
	p := l.m.Parent
	var raw []byte
	var hash, stop string
	if err := l.conn.QueryRow(ctx, `select manifest,manifest_sha256,coalesce(stopped_reason,'') from diagnosis_experiments where experiment_id=$1`, p.ExperimentID).Scan(&raw, &hash, &stop); err != nil {
		return err
	}
	var old Manifest
	if jsonstrict.Decode(raw, &old) != nil || hash != p.ManifestSHA256 || stop != "thinking_not_observed" ||
		old.SchemaVersion != 1 || old.Parent != nil || old.DatabaseID != l.m.DatabaseID || old.DatabaseName != l.m.DatabaseName ||
		old.PriorCost != l.m.PriorCost || old.CostCap != l.m.CostCap || old.BatchCap != l.m.BatchCap || old.Pricing != l.m.Pricing ||
		old.MaxInputTokens != l.m.MaxInputTokens || old.MaxOutputTokens != l.m.MaxOutputTokens ||
		!reflect.DeepEqual(old.Trials, l.m.Trials) || !reflect.DeepEqual(old.Sources, l.m.Sources) || p.CompletedTrials != 15 {
		return ErrInvalid
	}
	rows, err := l.conn.Query(ctx, `select trial_id,ordinal,request_sha256,state from diagnosis_calls where experiment_id=$1 order by ordinal`, p.ExperimentID)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id, requestHash, state string
		var ordinal int
		if err := rows.Scan(&id, &ordinal, &requestHash, &state); err != nil {
			return err
		}
		if count >= p.CompletedTrials || count >= len(old.Trials) {
			return ErrInvalid
		}
		trial := old.Trials[count]
		if ordinal != count+1 || id != trial.ID || requestHash != trial.RequestSHA256 || state != "known" {
			return ErrInvalid
		}
		count++
	}
	if rows.Err() != nil || count != p.CompletedTrials {
		return ErrInvalid
	}
	return nil
}

// Close destroys the lock-owning session rather than returning a held lock to a pool.
func (l *Ledger) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = l.conn.Conn().Close(ctx)
	l.conn.Release()
}

func (l *Ledger) upper() (run.CallBudget, error) {
	cost, err := run.UsageCost(l.m.Pricing, run.UsageReport{InputTokens: l.m.MaxInputTokens, OutputTokens: l.m.MaxOutputTokens})
	return run.CallBudget{InputTokens: l.m.MaxInputTokens, OutputTokens: l.m.MaxOutputTokens,
		TotalTokens: l.m.MaxInputTokens + l.m.MaxOutputTokens, CostMicroyuan: cost}, err
}

// NextBatch reuses remaining capacity or opens a capped batch after all holds close.
func (l *Ledger) NextBatch(ctx context.Context) (string, error) {
	upper, err := l.upper()
	if err != nil {
		return "", err
	}
	var used, held int64
	if err := l.conn.QueryRow(ctx, `select coalesce(sum(known_cost_microyuan+held_cost_microyuan),0),coalesce(sum(held_cost_microyuan),0)
		from budget_accounts where scope='batch'`).Scan(&used, &held); err != nil {
		return "", err
	}
	if held != 0 || l.m.CostCap-l.m.PriorCost-used < upper.CostMicroyuan {
		return "", run.ErrBudgetExhausted
	}
	var id string
	err = l.conn.QueryRow(ctx, `select account_id from budget_accounts where scope='batch' and not frozen
		and valid_until>clock_timestamp() and limit_cost_microyuan-used_cost_microyuan >= $1 order by valid_from desc limit 1`, upper.CostMicroyuan).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	store, err := runpg.New(l.pool, runpg.Options{})
	if err != nil {
		return "", err
	}
	id = uuid.NewString()
	now := time.Now().UTC()
	batchCap := min(l.m.BatchCap, l.m.CostCap-l.m.PriorCost-used)
	err = store.CreateBudget(ctx, run.BudgetSpec{ID: id, Scope: "batch", Key: "diagnosis-" + id,
		ValidFrom: now.Add(-time.Minute), ValidUntil: now.Add(6 * time.Hour),
		Limits: run.Usage{Chat: int64(len(l.m.Trials)), PhysicalHTTP: int64(len(l.m.Trials)),
			Tokens: int64(len(l.m.Trials)) * upper.TotalTokens, CostMicroyuan: batchCap}})
	return id, err
}

const accountColumns = `account_id,scope,frozen,limit_chat,limit_physical_http,limit_tokens,limit_cost_microyuan,
	used_chat,used_physical_http,used_tokens,used_cost_microyuan,known_tokens,known_cost_microyuan,held_tokens,held_cost_microyuan`

func readAccount(row pgx.Row) (run.Account, error) {
	var a run.Account
	err := row.Scan(&a.ID, &a.Scope, &a.Frozen, &a.Limits.Chat, &a.Limits.PhysicalHTTP, &a.Limits.Tokens, &a.Limits.CostMicroyuan,
		&a.Used.Chat, &a.Used.PhysicalHTTP, &a.Used.Tokens, &a.Used.CostMicroyuan,
		&a.KnownTokens, &a.KnownCostMicroyuan, &a.HeldTokens, &a.HeldCostMicroyuan)
	return a, err
}

func saveAccount(ctx context.Context, tx pgx.Tx, a run.Account) error {
	_, err := tx.Exec(ctx, `update budget_accounts set used_chat=$2,used_physical_http=$3,used_tokens=$4,used_cost_microyuan=$5,
		known_tokens=$6,known_cost_microyuan=$7,held_tokens=$8,held_cost_microyuan=$9,frozen=frozen or $10 where account_id=$1`,
		a.ID, a.Used.Chat, a.Used.PhysicalHTTP, a.Used.Tokens, a.Used.CostMicroyuan,
		a.KnownTokens, a.KnownCostMicroyuan, a.HeldTokens, a.HeldCostMicroyuan, a.Frozen)
	return err
}

// Reserve commits the call identity and conservative hold before one physical send.
func (l *Ledger) Reserve(ctx context.Context, batch string, t Trial, ordinal int) error {
	upper, err := l.upper()
	if err != nil {
		return err
	}
	tx, err := l.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var stopped bool
	if err := tx.QueryRow(ctx, `select stopped_reason is not null from diagnosis_experiments where experiment_id=$1 for update`, l.m.ExperimentID).Scan(&stopped); err != nil || stopped {
		return ErrInvalid
	}
	a, err := readAccount(tx.QueryRow(ctx, "select "+accountColumns+" from budget_accounts where account_id=$1 and scope='batch' and valid_until>clock_timestamp() for update", batch))
	if err != nil {
		return err
	}
	var total int64
	if err := tx.QueryRow(ctx, `select coalesce(sum(known_cost_microyuan+held_cost_microyuan),0) from budget_accounts where scope='batch'`).Scan(&total); err != nil {
		return err
	}
	if l.m.PriorCost+total+upper.CostMicroyuan > l.m.CostCap {
		return run.ErrBudgetExhausted
	}
	a, err = run.ReserveAccount(a, run.Usage{Chat: 1, PhysicalHTTP: 1, Tokens: upper.TotalTokens, CostMicroyuan: upper.CostMicroyuan})
	if err != nil {
		return err
	}
	// The database-wide sender lock also serializes identities across the amendment.
	inserted, err := tx.Exec(ctx, `insert into diagnosis_calls(experiment_id,trial_id,ordinal,batch_account_id,request_sha256,state,reserved_input,reserved_output,reserved_cost)
		select $1,$2,$3,$4,$5,'reserved',$6,$7,$8 where not exists(select 1 from diagnosis_calls where trial_id=$2)`, l.m.ExperimentID, t.ID, ordinal, batch, t.RequestSHA256,
		upper.InputTokens, upper.OutputTokens, upper.CostMicroyuan)
	if err != nil {
		return err
	}
	if inserted.RowsAffected() != 1 {
		return ErrInvalid
	}
	if err := saveAccount(ctx, tx, a); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Settled identifies an already completed trial without granting replay permission.
func (l *Ledger) Settled(ctx context.Context, id string) (bool, error) {
	var state string
	err := l.conn.QueryRow(ctx, `select state from diagnosis_calls where experiment_id in ($1,$3) and trial_id=$2`, l.m.ExperimentID, id, l.parentID()).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if state != "known" {
		return false, ErrInvalid
	}
	return true, nil
}

// Settle atomically persists the first report and known usage, or retains full hold.
func (l *Ledger) Settle(ctx context.Context, t Trial, report Report, elapsed int64) error {
	raw, err := json.Marshal(report)
	if err != nil {
		return err
	}
	hash := Digest(raw)
	tx, err := l.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var batch, state, previous string
	var input, output, heldCost int64
	if err := tx.QueryRow(ctx, `select batch_account_id,state,coalesce(report_sha256,''),reserved_input,reserved_output,reserved_cost
		from diagnosis_calls where experiment_id=$1 and trial_id=$2 for update`, l.m.ExperimentID, t.ID).Scan(&batch, &state, &previous, &input, &output, &heldCost); err != nil {
		return err
	}
	a, err := readAccount(tx.QueryRow(ctx, "select "+accountColumns+" from budget_accounts where account_id=$1 for update", batch))
	if err != nil {
		return err
	}
	if previous != "" {
		if previous == hash {
			return tx.Commit(ctx)
		}
		a.Frozen = true
		if err := saveAccount(ctx, tx, a); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `update diagnosis_experiments set stopped_reason=coalesce(stopped_reason,'report_conflict') where experiment_id=$1`, l.m.ExperimentID); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return errors.New("diagnosis report conflict")
	}
	if state != "reserved" {
		return ErrInvalid
	}
	state = "unknown"
	var in, out, cached, cost *int64
	if report.Usage != nil {
		u := report.Usage
		if u.Validate() != nil {
			return ErrInvalid
		}
		c, err := run.UsageCost(l.m.Pricing, *u)
		if err != nil {
			return err
		}
		if u.InputTokens > input || u.OutputTokens > output || c > heldCost {
			state, report.Stop = "anomaly", "measurement_anomaly"
		} else {
			a, err = run.SettleAccount(a, input+output, heldCost, u.InputTokens+u.OutputTokens, c)
			if err != nil {
				return err
			}
			state, in, out, cached, cost = "known", &u.InputTokens, &u.OutputTokens, &u.CachedInputTokens, &c
		}
	}
	if state != "known" || report.Stop != "" {
		a.Frozen = true
		stop := report.Stop
		if stop == "" {
			stop = "usage_unknown"
		}
		if _, err := tx.Exec(ctx, `update diagnosis_experiments set stopped_reason=coalesce(stopped_reason,$2) where experiment_id=$1`, l.m.ExperimentID, stop); err != nil {
			return err
		}
	}
	if err := saveAccount(ctx, tx, a); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `update diagnosis_calls set state=$3,input_tokens=$4,output_tokens=$5,cached_input_tokens=$6,
		reasoning_tokens=$7,known_cost=$8,report_sha256=$9,response_sha256=$10,outcome=$11,settled_at=clock_timestamp(),elapsed_ms=$12
		where experiment_id=$1 and trial_id=$2`, l.m.ExperimentID, t.ID, state, in, out, cached, report.ReasoningTokens,
		cost, hash, report.ResponseSHA256, report.Outcome, elapsed); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Summary reads authoritative PG financial totals, never inferred file counts.
func (l *Ledger) Summary(ctx context.Context) (map[string]any, error) {
	var known, held, calls int64
	var stop *string
	if err := l.conn.QueryRow(ctx, `select coalesce(sum(known_cost_microyuan),0),coalesce(sum(held_cost_microyuan),0),coalesce(sum(used_chat),0)
		from budget_accounts where scope='batch'`).Scan(&known, &held, &calls); err != nil {
		return nil, err
	}
	if err := l.conn.QueryRow(ctx, `select stopped_reason from diagnosis_experiments where experiment_id=$1`, l.m.ExperimentID).Scan(&stop); err != nil {
		return nil, err
	}
	return map[string]any{"experiment_id": l.m.ExperimentID, "diagnosis_calls": calls, "known_cost_microyuan": known,
		"held_cost_microyuan": held, "prior_cost_microyuan": l.m.PriorCost, "s5_exposure_microyuan": l.m.PriorCost + known + held,
		"remaining_cost_microyuan": l.m.CostCap - l.m.PriorCost - known - held, "stopped_reason": stop}, nil
}

// Export snapshots PG bindings for offline scoring, including incomplete trials.
func (l *Ledger) Export(ctx context.Context) (json.RawMessage, error) {
	var raw []byte
	err := l.conn.QueryRow(ctx, `select jsonb_build_object(
		'database_name',current_database(),
		'database_id',(select database_id from diagnosis_database_identity where singleton),
		'experiment_id',e.experiment_id,'manifest_sha256',e.manifest_sha256,
		'parent',e.manifest->'parent',
		'stopped_reason',e.stopped_reason,'exported_at',clock_timestamp(),
		'calls',coalesce((select jsonb_agg(to_jsonb(c) order by ordinal)
		from diagnosis_calls c where c.experiment_id in (e.experiment_id,$2)),'[]'::jsonb))
		from diagnosis_experiments e where experiment_id=$1`, l.m.ExperimentID, l.parentID()).Scan(&raw)
	return json.RawMessage(raw), err
}
