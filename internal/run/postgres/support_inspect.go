package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/xjfyrh/jobforge/internal/run"
)

// SupportBudgetBinding is the existing tenant grant, without mutable authority.
type SupportBudgetBinding struct {
	TenantID        string `json:"tenant_id"`
	BatchAccountID  string `json:"batch_account_id"`
	TenantAccountID string `json:"tenant_account_id"`
}

// SupportInspectionRequest selects the fixed prepared batch and dedicated worker.
type SupportInspectionRequest struct {
	ProfileID string
	WorkerID  string
	Budgets   []run.BudgetSpec
	Bindings  []SupportBudgetBinding
}

// SupportAccountInspection reports persisted values; no zero-usage projection.
type SupportAccountInspection struct {
	AccountID          string            `json:"account_id"`
	Scope              string            `json:"scope"`
	ScopeKey           string            `json:"scope_key"`
	ValidFrom          time.Time         `json:"valid_from"`
	ValidUntil         time.Time         `json:"valid_until"`
	Limits             run.Usage         `json:"limits"`
	Used               run.Usage         `json:"used"`
	KnownTokens        int64             `json:"known_tokens"`
	KnownCostMicroyuan int64             `json:"known_cost_microyuan"`
	HeldTokens         int64             `json:"held_tokens"`
	HeldCostMicroyuan  int64             `json:"held_cost_microyuan"`
	Frozen             bool              `json:"frozen"`
	BatchStopCode      run.BatchStopCode `json:"batch_stop_code"`
}

// SupportInspection is a consistent read-only observation, never a launch permit.
type SupportInspection struct {
	SchemaVersion   int       `json:"schema_version"`
	SampledAt       time.Time `json:"sampled_at"`
	MigrationsReady bool      `json:"migrations_ready"`
	Profile         struct {
		ProfileID     string `json:"profile_id"`
		ProfileHash   string `json:"profile_hash"`
		Registered    bool   `json:"registered"`
		MatchesConfig bool   `json:"matches_config"`
	} `json:"profile"`
	Accounts []SupportAccountInspection `json:"accounts"`
	Bindings []SupportBudgetBinding     `json:"bindings"`
	History  struct {
		BusinessRequests int64 `json:"business_requests"`
		Runs             int64 `json:"runs"`
		Calls            int64 `json:"calls"`
		WorkerSessions   int64 `json:"worker_sessions"`
		WorkerStartups   int64 `json:"worker_startups"`
	} `json:"history"`
	MatchesConfig bool `json:"matches_config"`
}

// InspectSupport never applies migrations, registers profiles or updates budgets.
// A read-only repeatable-read transaction gives the launcher one coherent view.
func (s *Store) InspectSupport(ctx context.Context, request SupportInspectionRequest) (SupportInspection, error) {
	result := SupportInspection{SchemaVersion: 1, Accounts: []SupportAccountInspection{}, Bindings: []SupportBudgetBinding{}}
	profile, ok := s.profiles[request.ProfileID]
	if !ok || len(request.Budgets) != 3 || len(request.Bindings) != 2 || !run.ValidIdentifier(request.WorkerID) {
		return result, run.ErrInvalidArgument
	}
	batchID := request.Bindings[0].BatchAccountID
	result.Profile.ProfileID = request.ProfileID
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, dbError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var schemaPresent bool
	if err := tx.QueryRow(ctx, `select current_timestamp, to_regclass('public.schema_migrations') is not null
		and to_regclass('public.agent_profiles') is not null and to_regclass('public.budget_accounts') is not null
		and to_regclass('public.worker_sessions') is not null and to_regclass('public.physical_calls') is not null
		and to_regclass('business_meta.database_identity') is null`).Scan(&result.SampledAt, &schemaPresent); err != nil {
		return result, dbError(err)
	}
	if schemaPresent {
		if err := tx.QueryRow(ctx, "select count(*)=24 from schema_migrations where version between 1 and 24").Scan(&result.MigrationsReady); err != nil {
			return result, dbError(err)
		}
	}
	if !result.MigrationsReady {
		return result, dbError(tx.Commit(ctx))
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		return result, run.ErrInvalidArgument
	}
	err = tx.QueryRow(ctx, `select profile_hash, definition=$2::jsonb from agent_profiles where profile_id=$1`, profile.ID, encoded).
		Scan(&result.Profile.ProfileHash, &result.Profile.MatchesConfig)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return result, dbError(err)
	}
	result.Profile.Registered = err == nil
	result.Profile.MatchesConfig = result.Profile.MatchesConfig && result.Profile.ProfileHash == profile.Hash
	result.MatchesConfig = result.Profile.Registered && result.Profile.MatchesConfig
	for _, spec := range request.Budgets {
		row, err := readAccount(tx.QueryRow(ctx, "select "+accountColumns()+" from budget_accounts where account_id=$1", spec.ID))
		if errors.Is(err, pgx.ErrNoRows) {
			result.MatchesConfig = false
			continue
		}
		if err != nil {
			return result, dbError(err)
		}
		a := row.Account
		result.Accounts = append(result.Accounts, SupportAccountInspection{a.ID, a.Scope, row.Key, row.ValidFrom, row.ValidUntil,
			a.Limits, a.Used, a.KnownTokens, a.KnownCostMicroyuan, a.HeldTokens, a.HeldCostMicroyuan, a.Frozen, a.BatchStopCode})
		result.MatchesConfig = result.MatchesConfig && a.Scope == spec.Scope && row.Key == spec.Key && a.Limits == spec.Limits &&
			row.ValidFrom.Equal(spec.ValidFrom) && row.ValidUntil.Equal(spec.ValidUntil)
	}
	rows, err := tx.Query(ctx, `select tenant_id,batch_account_id,tenant_account_id from budget_batch_tenants where batch_account_id=$1 order by tenant_id`, batchID)
	if err != nil {
		return result, dbError(err)
	}
	for rows.Next() {
		var binding SupportBudgetBinding
		if err := rows.Scan(&binding.TenantID, &binding.BatchAccountID, &binding.TenantAccountID); err != nil {
			rows.Close()
			return result, dbError(err)
		}
		result.Bindings = append(result.Bindings, binding)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, dbError(err)
	}
	result.MatchesConfig = result.MatchesConfig && len(result.Bindings) == len(request.Bindings)
	for _, binding := range request.Bindings {
		result.MatchesConfig = result.MatchesConfig && slices.Contains(result.Bindings, binding)
	}
	err = tx.QueryRow(ctx, `select
		(select count(*) from business_requests where batch_account_id=$1::uuid),
		(select count(*) from runs r join business_requests b on b.tenant_id=r.tenant_id and b.business_request_id=r.business_request_id where b.batch_account_id=$1::uuid),
		(select count(*) from physical_calls c join runs r on r.tenant_id=c.tenant_id and r.run_id=c.run_id
		 join business_requests b on b.tenant_id=r.tenant_id and b.business_request_id=r.business_request_id where b.batch_account_id=$1::uuid),
		(select count(*) from worker_sessions where worker_id=$2),
		(select count(distinct startup_id) from worker_sessions where worker_id=$2)`, batchID, request.WorkerID).
		Scan(&result.History.BusinessRequests, &result.History.Runs, &result.History.Calls, &result.History.WorkerSessions, &result.History.WorkerStartups)
	if err != nil {
		return result, dbError(err)
	}
	return result, dbError(tx.Commit(ctx))
}
