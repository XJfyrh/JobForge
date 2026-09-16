package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

var usageNames = []string{"chat", "logical_tools", "query_embedding", "profile_metadata_http",
	"business_tool_http", "physical_http", "protocol_corrections", "tokens", "cost_microyuan"}

func usageValues(u agentrun.Usage) []any {
	return []any{u.Chat, u.LogicalTools, u.QueryEmbedding, u.ProfileMetadataHTTP,
		u.BusinessToolHTTP, u.PhysicalHTTP, u.ProtocolCorrections, u.Tokens, u.CostMicroyuan}
}

func usageTargets(u *agentrun.Usage) []any {
	return []any{&u.Chat, &u.LogicalTools, &u.QueryEmbedding, &u.ProfileMetadataHTTP,
		&u.BusinessToolHTTP, &u.PhysicalHTTP, &u.ProtocolCorrections, &u.Tokens, &u.CostMicroyuan}
}

func usageColumns(prefix string) string {
	names := make([]string, len(usageNames))
	for i, name := range usageNames {
		names[i] = prefix + name
	}
	return strings.Join(names, ",")
}

type budgetRow struct {
	Account    agentrun.Account
	Key        string
	ValidFrom  time.Time
	ValidUntil time.Time
}

func readAccount(row pgx.Row) (budgetRow, error) {
	var b budgetRow
	targets := []any{&b.Account.ID, &b.Account.Scope, &b.Key, &b.ValidFrom, &b.ValidUntil, &b.Account.Frozen, &b.Account.BatchStopCode}
	targets = append(targets, usageTargets(&b.Account.Limits)...)
	targets = append(targets, usageTargets(&b.Account.Used)...)
	targets = append(targets, &b.Account.KnownTokens, &b.Account.KnownCostMicroyuan, &b.Account.HeldTokens, &b.Account.HeldCostMicroyuan)
	err := row.Scan(targets...)
	return b, err
}

func accountColumns() string {
	return "account_id,scope,scope_key,valid_from,valid_until,frozen,coalesce(batch_stop_code,'')," + usageColumns("limit_") + "," + usageColumns("used_") +
		",known_tokens,known_cost_microyuan,held_tokens,held_cost_microyuan"
}

// loadAccounts is called after locking the Run. This order is shared by
// reservations and late settlements; no path waits for a Run while holding an
// account lock. The business identity itself is read without a conflicting lock.
func loadAccounts(ctx context.Context, tx pgx.Tx, tenant, businessID string, lock bool) ([3]budgetRow, error) {
	var ids [3]string
	var rows [3]budgetRow
	err := tx.QueryRow(ctx, `select family_account_id,tenant_account_id,batch_account_id
		from business_requests where tenant_id=$1 and business_request_id=$2`, tenant, businessID).Scan(&ids[0], &ids[1], &ids[2])
	if err != nil {
		return rows, err
	}
	lockSQL := ""
	if lock {
		lockSQL = " for update"
	}
	for i, scope := range []string{"family", "tenant", "batch"} {
		rows[i], err = readAccount(tx.QueryRow(ctx, "select "+accountColumns()+" from budget_accounts where account_id=$1"+lockSQL, ids[i]))
		if err != nil {
			return rows, err
		}
		if rows[i].Account.Scope != scope {
			return rows, agentrun.ErrInternal
		}
	}
	return rows, nil
}

func saveAccount(ctx context.Context, tx pgx.Tx, b budgetRow) error {
	args := []any{b.Account.ID}
	args = append(args, usageValues(b.Account.Used)...)
	sets := make([]string, 0, len(usageNames)+5)
	for i, name := range usageNames {
		sets = append(sets, fmt.Sprintf("used_%s=$%d", name, i+2))
	}
	args = append(args, b.Account.KnownTokens, b.Account.KnownCostMicroyuan, b.Account.HeldTokens, b.Account.HeldCostMicroyuan, b.Account.Frozen, b.Account.BatchStopCode)
	sets = append(sets, "known_tokens=$11", "known_cost_microyuan=$12", "held_tokens=$13", "held_cost_microyuan=$14", "frozen=frozen or $15",
		"batch_stop_code=coalesce(batch_stop_code,nullif($16,''))")
	_, err := tx.Exec(ctx, "update budget_accounts set "+strings.Join(sets, ",")+" where account_id=$1", args...)
	return err
}

func insertBudget(ctx context.Context, tx pgx.Tx, spec agentrun.BudgetSpec) error {
	args := []any{spec.ID, spec.Scope, spec.Key, spec.ValidFrom, spec.ValidUntil}
	args = append(args, usageValues(spec.Limits)...)
	placeholders := make([]string, len(args))
	for i := range args {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	_, err := tx.Exec(ctx, "insert into budget_accounts (account_id,scope,scope_key,valid_from,valid_until,"+
		usageColumns("limit_")+") values ("+strings.Join(placeholders, ",")+")", args...)
	return err
}

// CreateBudget is an explicit administrator setup operation. It never resets
// existing usage or turns an idempotent setup rerun into a budget top-up.
func (s *Store) CreateBudget(ctx context.Context, spec agentrun.BudgetSpec) error {
	// PostgreSQL timestamps have microsecond precision; setup replay uses the
	// same canonical representation instead of comparing discarded nanoseconds.
	spec.ValidFrom = spec.ValidFrom.UTC().Truncate(time.Microsecond)
	spec.ValidUntil = spec.ValidUntil.UTC().Truncate(time.Microsecond)
	if _, err := uuid.Parse(spec.ID); err != nil || !agentrun.ValidIdentifier(spec.Key) ||
		(spec.Scope != "tenant" && spec.Scope != "batch") || !spec.Limits.Valid() || !spec.ValidUntil.After(spec.ValidFrom) {
		return agentrun.ErrInvalidArgument
	}
	return s.transact(ctx, func(tx pgx.Tx) error {
		// Setup serializes by immutable scope/key, including when the row is absent.
		if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock(hashtextextended($1,17))", spec.Scope+":"+spec.Key); err != nil {
			return err
		}
		prior, err := readAccount(tx.QueryRow(ctx, "select "+accountColumns()+" from budget_accounts where scope=$1 and scope_key=$2 for update", spec.Scope, spec.Key))
		if err == nil {
			if prior.Account.ID != spec.ID || prior.Account.Limits != spec.Limits || !prior.ValidFrom.Equal(spec.ValidFrom) || !prior.ValidUntil.Equal(spec.ValidUntil) {
				return agentrun.ErrConflict
			}
			return nil
		}
		if err != pgx.ErrNoRows {
			return err
		}
		return insertBudget(ctx, tx, spec)
	})
}

// BindBudgetTenant grants one existing tenant account access to a batch. It is
// called only by administrator setup, never by Run submit or model output.
func (s *Store) BindBudgetTenant(ctx context.Context, tenant, batchID, tenantAccountID string) error {
	if !agentrun.ValidIdentifier(tenant) {
		return agentrun.ErrInvalidArgument
	}
	return s.transact(ctx, func(tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `select count(*) from budget_accounts where
			(account_id=$1 and scope='batch') or (account_id=$2 and scope='tenant' and scope_key=$3)`, batchID, tenantAccountID, tenant).Scan(&count); err != nil {
			return err
		}
		if count != 2 {
			return agentrun.ErrInvalidArgument
		}
		_, err := tx.Exec(ctx, `insert into budget_batch_tenants(batch_account_id,tenant_id,tenant_account_id)
			values($1,$2,$3) on conflict (batch_account_id,tenant_id) do nothing`, batchID, tenant, tenantAccountID)
		if err != nil {
			return err
		}
		var prior string
		if err := tx.QueryRow(ctx, "select tenant_account_id from budget_batch_tenants where batch_account_id=$1 and tenant_id=$2", batchID, tenant).Scan(&prior); err != nil {
			return err
		}
		if prior != tenantAccountID {
			return agentrun.ErrConflict
		}
		return nil
	})
}
