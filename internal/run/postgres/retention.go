package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

// CleanupTerminalContent is an administrator-only bounded content operation.
// Identity, commit hashes, original byte counts, audit, signatures, receipts and
// account exposure survive. Each transaction locks one Run, never an account.
func (s *Store) CleanupTerminalContent(ctx context.Context, limit int, apply bool) (agentrun.CleanupResult, error) {
	result := agentrun.CleanupResult{Applied: apply, RunIDs: []string{}}
	if limit < 1 || limit > 100 {
		return result, agentrun.ErrInvalidArgument
	}
	var candidates []struct{ tenant, id string }
	rows, err := s.pool.Query(ctx, `select r.tenant_id,r.run_id from runs r
		join business_requests b on b.tenant_id=r.tenant_id and b.business_request_id=r.business_request_id
		join budget_accounts a on a.account_id=b.batch_account_id
		where r.state in ('succeeded','failed','cancelled') and r.content_purged_at is null
		and r.terminal_at<=clock_timestamp()-interval '7 days' and a.valid_until<=clock_timestamp()
		order by r.terminal_at,r.run_id limit $1`, limit)
	if err != nil {
		return result, dbError(err)
	}
	for rows.Next() {
		var candidate struct{ tenant, id string }
		if err := rows.Scan(&candidate.tenant, &candidate.id); err != nil {
			rows.Close()
			return result, dbError(err)
		}
		candidates = append(candidates, candidate)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, dbError(err)
	}
	for _, candidate := range candidates {
		var purgedID string
		var contentBytes int64
		err = s.transact(ctx, func(tx pgx.Tx) error {
			r, a, err := lockRun(ctx, tx, candidate.tenant, candidate.id)
			if err != nil {
				return err
			}
			var batchUntil time.Time
			if err := tx.QueryRow(ctx, `select a.valid_until from business_requests b
				join budget_accounts a on a.account_id=b.batch_account_id
				where b.tenant_id=$1 and b.business_request_id=$2`, r.TenantID, r.BusinessRequestID).Scan(&batchUntil); err != nil {
				return err
			}
			now, err := databaseTime(ctx, tx)
			if err != nil {
				return err
			}
			result.CheckedAt = now
			if !agentrun.CanPurgeContent(r, batchUntil, now) {
				return nil
			}
			if apply {
				if _, err := tx.Exec(ctx, `update runs set content_purged_at=$3,ticket_binding=null
					where tenant_id=$1 and run_id=$2`, r.TenantID, r.ID, now); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `update run_steps set output=null where tenant_id=$1 and run_id=$2`, r.TenantID, r.ID); err != nil {
					return err
				}
			}
			purgedID, contentBytes = r.ID, a.CheckpointBytes
			return nil
		})
		if err != nil {
			return result, err
		}
		if purgedID != "" {
			result.RunIDs = append(result.RunIDs, purgedID)
			result.ContentBytes += contentBytes
		}
	}
	if result.CheckedAt.IsZero() {
		err = s.pool.QueryRow(ctx, "select clock_timestamp()").Scan(&result.CheckedAt)
	}
	return result, dbError(err)
}
