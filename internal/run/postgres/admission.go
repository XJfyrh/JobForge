package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

func operationScope(kind, source string) string {
	if kind == "submit" {
		return "submit"
	}
	return kind + ":" + source
}

func findOperation(ctx context.Context, tx pgx.Tx, tenant, kind, source, key string) (agentrun.Operation, error) {
	var op agentrun.Operation
	err := tx.QueryRow(ctx, `select operation_id,tenant_id,operation_kind,coalesce(source_run_id::text,''),
		operation_scope,operation_key,request_hash,result_run_id,created_at from run_operations
		where tenant_id=$1 and operation_scope=$2 and operation_key=$3`, tenant, operationScope(kind, source), key).
		Scan(&op.ID, &op.TenantID, &op.Kind, &op.SourceRunID, &op.Scope, &op.Key, &op.RequestHash, &op.ResultRunID, &op.CreatedAt)
	return op, err
}

func insertOperation(ctx context.Context, tx pgx.Tx, op agentrun.Operation) error {
	_, err := tx.Exec(ctx, `insert into run_operations
		(operation_id,tenant_id,operation_kind,source_run_id,operation_scope,operation_key,request_hash,result_run_id,created_at)
		values($1,$2,$3,nullif($4,'')::uuid,$5,$6,$7,$8,$9) on conflict (tenant_id,operation_scope,operation_key) do nothing`,
		op.ID, op.TenantID, op.Kind, op.SourceRunID, operationScope(op.Kind, op.SourceRunID), op.Key, op.RequestHash, op.ResultRunID, op.CreatedAt)
	if err != nil {
		return err
	}
	accepted, err := findOperation(ctx, tx, op.TenantID, op.Kind, op.SourceRunID, op.Key)
	if err != nil {
		return err
	}
	if accepted.RequestHash != op.RequestHash || accepted.ResultRunID != op.ResultRunID {
		return agentrun.ErrConflict
	}
	return nil
}

func readBusinessRequest(row pgx.Row) (agentrun.BusinessRequest, error) {
	var request agentrun.BusinessRequest
	err := row.Scan(&request.ID, &request.TenantID, &request.Key, &request.RequestHash, &request.RootRunID,
		&request.FamilyAccountID, &request.TenantAccountID, &request.BatchAccountID, &request.CreatedAt, &request.RetryUntil)
	return request, err
}

const businessRequestColumns = `business_request_id,tenant_id,business_request_key,request_hash,root_run_id,
	family_account_id,tenant_account_id,batch_account_id,created_at,retry_until`

// ResolveAdmission returns an already accepted object before any executable
// profile, expiry, balance, or external business capture check. It may append an
// alias operation key but never creates a new Run or new budget family.
func (s *Store) ResolveAdmission(ctx context.Context, tenant, key, sourceID, hash, businessKey string) (*agentrun.SubmitResponse, error) {
	var response *agentrun.SubmitResponse
	err := s.transact(ctx, func(tx pgx.Tx) error {
		var err error
		response, err = resolveAdmission(ctx, tx, tenant, key, sourceID, hash, businessKey)
		return err
	})
	return response, err
}

func resolveAdmission(ctx context.Context, tx pgx.Tx, tenant, key, sourceID, hash, businessKey string) (*agentrun.SubmitResponse, error) {
	kind := "submit"
	if sourceID != "" {
		kind = "retry"
	}
	op, err := findOperation(ctx, tx, tenant, kind, sourceID, key)
	if err == nil {
		if op.RequestHash != hash {
			return nil, agentrun.ErrConflict
		}
		r, err := getView(ctx, tx, tenant, op.ResultRunID)
		if err != nil {
			return nil, err
		}
		return &agentrun.SubmitResponse{Run: r, Reused: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var business agentrun.BusinessRequest
	if kind == "submit" {
		business, err = readBusinessRequest(tx.QueryRow(ctx, "select "+businessRequestColumns+
			" from business_requests where tenant_id=$1 and business_request_key=$2 for update", tenant, businessKey))
	} else {
		business, err = readBusinessRequest(tx.QueryRow(ctx, "select "+businessRequestColumns+` from business_requests
			where tenant_id=$1 and business_request_id=(select business_request_id from runs where tenant_id=$1 and run_id=$2) for update`, tenant, sourceID))
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// A missing retry source is a public 404, not a new business intent.
		if kind == "retry" {
			return nil, agentrun.ErrNotFound
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	resultID := business.RootRunID
	acceptedHash := business.RequestHash
	if kind == "retry" {
		if _, _, err := lockRun(ctx, tx, tenant, sourceID); err != nil {
			return nil, err
		}
		err = tx.QueryRow(ctx, "select run_id,admission_hash from runs where tenant_id=$1 and retry_of_run_id=$2", tenant, sourceID).Scan(&resultID, &acceptedHash)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
	}
	if acceptedHash != hash {
		return nil, agentrun.ErrConflict
	}
	now, err := databaseTime(ctx, tx)
	if err != nil {
		return nil, err
	}
	err = insertOperation(ctx, tx, agentrun.Operation{ID: uuid.NewString(), TenantID: tenant, Kind: kind,
		SourceRunID: sourceID, Key: key, RequestHash: hash, ResultRunID: resultID, CreatedAt: now})
	if err != nil {
		return nil, err
	}
	r, err := getView(ctx, tx, tenant, resultID)
	if err != nil {
		return nil, err
	}
	return &agentrun.SubmitResponse{Run: r, Reused: true}, nil
}

// AdmissionContext verifies a not-yet-accepted request before external capture.
// The same dynamic conditions are checked again inside Admit after locking.
func (s *Store) AdmissionContext(ctx context.Context, tenant, sourceID, profileID, batchKey string) (agentrun.Run, error) {
	var source agentrun.Run
	err := s.readOnly(ctx, func(tx pgx.Tx) error {
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if sourceID != "" {
			source, _, err = readRun(tx.QueryRow(ctx, "select "+runColumns+" from runs where tenant_id=$1 and run_id=$2", tenant, sourceID))
			if err != nil {
				return err
			}
			if source.State != agentrun.Failed && source.State != agentrun.Cancelled {
				return agentrun.ErrInvalidTransition
			}
			var retryUntil time.Time
			if err := tx.QueryRow(ctx, "select retry_until from business_requests where tenant_id=$1 and business_request_id=$2", tenant, source.BusinessRequestID).Scan(&retryUntil); err != nil {
				return err
			}
			if !retryUntil.After(now) {
				return agentrun.ErrInvalidTransition
			}
			profileID, batchKey = source.ProfileID, source.BudgetBatchID
		}
		p, err := s.Profile(ctx, profileID)
		if err != nil {
			return err
		}
		if sourceID != "" && source.ProfileHash != p.Hash {
			return agentrun.ErrProfileUnavailable
		}
		_, _, err = admissionAccounts(ctx, tx, tenant, batchKey, now, false)
		return err
	})
	return source, err
}

func admissionAccounts(ctx context.Context, tx pgx.Tx, tenant, batchKey string, now time.Time, lock bool) (budgetRow, budgetRow, error) {
	var tenantID, batchID string
	err := tx.QueryRow(ctx, `select permission.tenant_account_id,batch.account_id from budget_batch_tenants permission
		join budget_accounts batch on batch.account_id=permission.batch_account_id
		where permission.tenant_id=$1 and batch.scope='batch' and batch.scope_key=$2`, tenant, batchKey).Scan(&tenantID, &batchID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return budgetRow{}, budgetRow{}, agentrun.ErrNotFound
		}
		return budgetRow{}, budgetRow{}, err
	}
	lockSQL := ""
	if lock {
		lockSQL = " for update"
	}
	tenantRow, err := readAccount(tx.QueryRow(ctx, "select "+accountColumns()+" from budget_accounts where account_id=$1"+lockSQL, tenantID))
	if err != nil {
		return budgetRow{}, budgetRow{}, err
	}
	batchRow, err := readAccount(tx.QueryRow(ctx, "select "+accountColumns()+" from budget_accounts where account_id=$1"+lockSQL, batchID))
	if err != nil {
		return budgetRow{}, budgetRow{}, err
	}
	if lock {
		now, err = databaseTime(ctx, tx)
		if err != nil {
			return budgetRow{}, budgetRow{}, err
		}
	}
	if tenantRow.Account.Scope != "tenant" || tenantRow.Key != tenant || batchRow.Account.Scope != "batch" {
		return budgetRow{}, budgetRow{}, agentrun.ErrInternal
	}
	for _, account := range []budgetRow{tenantRow, batchRow} {
		if account.Account.Frozen || account.ValidFrom.After(now) || !account.ValidUntil.After(now) {
			return budgetRow{}, budgetRow{}, agentrun.ErrBudgetExhausted
		}
	}
	return tenantRow, batchRow, nil
}
