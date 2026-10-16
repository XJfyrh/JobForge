package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

// fillPageBudgets keeps the same repeatable-read view with three queries per
// page rather than six per Run. Call and tool usage remain scoped to each Run.
func fillPageBudgets(ctx context.Context, tx pgx.Tx, tenant string, runs []agentrun.Run) error {
	if len(runs) == 0 {
		return nil
	}
	businessIDs, runIDs := make([]string, 0, len(runs)), make([]string, 0, len(runs))
	byRun := make(map[string]*agentrun.Run, len(runs))
	for i := range runs {
		businessIDs = append(businessIDs, runs[i].BusinessRequestID)
		runIDs = append(runIDs, runs[i].ID)
		byRun[runs[i].ID] = &runs[i]
	}
	rows, err := tx.Query(ctx, "select b.business_request_id,"+accountColumns()+` from business_requests b
		join budget_accounts a on a.account_id=case a.scope
		when 'family' then b.family_account_id when 'tenant' then b.tenant_account_id when 'batch' then b.batch_account_id end
		where b.tenant_id=$1 and b.business_request_id=any($2::uuid[])`, tenant, businessIDs)
	if err != nil {
		return err
	}
	defer rows.Close()
	accounts := make(map[string]map[string]agentrun.Account)
	for rows.Next() {
		var businessID string
		var account budgetRow
		if err := rows.Scan(append([]any{&businessID}, accountTargets(&account)...)...); err != nil {
			return err
		}
		if accounts[businessID] == nil {
			accounts[businessID] = make(map[string]agentrun.Account)
		}
		accounts[businessID][account.Account.Scope] = account.Account
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for i := range runs {
		a := accounts[runs[i].BusinessRequestID]
		if len(a) != 3 || a["family"].ID == "" || a["tenant"].ID == "" || a["batch"].ID == "" {
			return agentrun.ErrInternal
		}
		runs[i].Budget.Family, runs[i].Budget.Tenant, runs[i].Budget.Batch = a["family"], a["tenant"], a["batch"]
	}
	rows, err = tx.Query(ctx, `select run_id,
		count(*) filter (where kind='chat'),count(*) filter (where kind='query_embedding'),
		count(*) filter (where kind='profile_metadata_http'),count(*) filter (where kind='business_tool_http'),
		count(*),count(*) filter (where step_kind='protocol_correction'),
		coalesce(sum(case when status='known' then known_tokens else reserved_tokens end),0)::bigint,
		coalesce(sum(case when status='known' then known_cost_microyuan else reserved_cost_microyuan end),0)::bigint
		from physical_calls where tenant_id=$1 and run_id=any($2::uuid[]) group by run_id`, tenant, runIDs)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var u agentrun.Usage
		if err := rows.Scan(&id, &u.Chat, &u.QueryEmbedding, &u.ProfileMetadataHTTP, &u.BusinessToolHTTP,
			&u.PhysicalHTTP, &u.ProtocolCorrections, &u.Tokens, &u.CostMicroyuan); err != nil {
			return err
		}
		byRun[id].Budget.RunUsage = u
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	rows, err = tx.Query(ctx, `select run_id,count(*) from tool_invocations
		where tenant_id=$1 and run_id=any($2::uuid[]) group by run_id`, tenant, runIDs)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var count int64
		if err := rows.Scan(&id, &count); err != nil {
			return err
		}
		byRun[id].Budget.RunUsage.LogicalTools = count
	}
	return rows.Err()
}
