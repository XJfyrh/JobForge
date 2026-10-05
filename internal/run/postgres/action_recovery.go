package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/xjfyrh/jobforge/internal/business"
	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

// RecoveryAction reads terminal family identity before receipt-only recovery.
func (s *Store) RecoveryAction(ctx context.Context, tenant, id string, retry bool) (agentrun.ActionBinding, agentrun.Profile, error) {
	var binding agentrun.ActionBinding
	var profile agentrun.Profile
	if !validUUID(id) || !agentrun.ValidIdentifier(tenant) {
		return binding, profile, agentrun.ErrInvalidArgument
	}
	err := s.readOnly(ctx, func(tx pgx.Tx) error {
		r, _, err := readRun(tx.QueryRow(ctx, "select "+runColumns+" from runs where tenant_id=$1 and run_id=$2", tenant, id))
		if err != nil {
			return err
		}
		if !r.State.Terminal() || retry && r.State != agentrun.Failed && r.State != agentrun.Cancelled {
			return agentrun.ErrInvalidTransition
		}
		if retry {
			var until time.Time
			if err := tx.QueryRow(ctx, "select retry_until from business_requests where tenant_id=$1 and business_request_id=$2", tenant, r.BusinessRequestID).Scan(&until); err != nil {
				return err
			}
			now, err := databaseTime(ctx, tx)
			if err != nil {
				return err
			}
			if !until.After(now) {
				return agentrun.ErrInvalidTransition
			}
		}
		binding.Action, err = readAction(ctx, tx, tenant, r.BusinessRequestID, false)
		if err != nil {
			return err
		}
		binding.Effect, err = readEffect(ctx, tx, binding.Action)
		if err != nil || binding.Action == nil {
			return err
		}
		profile, err = s.ledgerProfile(ctx, tx, r, false)
		return err
	})
	return binding, profile, err
}

// ReserveReceiptQuery installs one tenant-wide bounded query lease by fresh DB time.
func (s *Store) ReserveReceiptQuery(ctx context.Context, tenant, id, source, queryID string) (agentrun.ReceiptQueryPermit, error) {
	var permit agentrun.ReceiptQueryPermit
	if !validUUID(id) || !validUUID(queryID) || !agentrun.ValidIdentifier(tenant) || (source != "retry" && source != "reconcile") {
		return permit, agentrun.ErrInvalidArgument
	}
	err := s.transact(ctx, func(tx pgx.Tx) error {
		// Acquire both FK identities before the gate. A late old query may be
		// saving its effect while the next query is being installed; neither path
		// may acquire an authorization FK lock after owning the tenant gate.
		var operation string
		if err := tx.QueryRow(ctx, `select a.operation_id from runs r join action_authorizations a
			on a.tenant_id=r.tenant_id and a.business_request_id=r.business_request_id
			where r.tenant_id=$1 and r.run_id=$2 and r.state in ('failed','cancelled','succeeded')
			for key share of r`, tenant, id).Scan(&operation); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `select operation_id from action_authorizations
			where tenant_id=$1 and operation_id=$2 for key share`, tenant, operation).Scan(&operation); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `insert into tenant_receipt_query_gates(tenant_id,next_allowed_at) values($1,'1970-01-01 00:00:00+00') on conflict(tenant_id) do nothing`, tenant); err != nil {
			return err
		}
		var next time.Time
		var until *time.Time
		if err := tx.QueryRow(ctx, `select next_allowed_at,query_until from tenant_receipt_query_gates where tenant_id=$1 for update`, tenant).Scan(&next, &until); err != nil {
			return err
		}
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if next.After(now) || until != nil && until.After(now) {
			return agentrun.ErrRateLimited
		}
		permit = agentrun.ReceiptQueryPermit{ID: queryID, Deadline: now.Add(10 * time.Second)}
		if _, err := tx.Exec(ctx, `update tenant_receipt_query_gates set query_id=$2,query_until=$3,next_allowed_at=$4 where tenant_id=$1`, tenant, queryID, permit.Deadline, now.Add(time.Second)); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `insert into action_receipt_queries(query_id,tenant_id,operation_id,source_run_id,source,reserved_at,deadline)
			values($1,$2,$3,$4,$5,$6,$7)`, queryID, tenant, operation, id, source, now, permit.Deadline)
		return err
	})
	return permit, err
}

// RecordReceiptQuery saves first effect and releases only its own query lease.
func (s *Store) RecordReceiptQuery(ctx context.Context, tenant, queryID string, action business.SignedAction, receipt *business.ActionReceipt, outcome string) error {
	if !agentrun.ValidIdentifier(tenant) || !validUUID(queryID) || (outcome != "found" && outcome != "absent" && outcome != "unavailable") || (outcome == "found") != (receipt != nil) {
		return agentrun.ErrInvalidArgument
	}
	return s.transact(ctx, func(tx pgx.Tx) error {
		var source, operation string
		var finished *time.Time
		var priorOutcome *string
		if err := tx.QueryRow(ctx, `select source,operation_id,finished_at,outcome from action_receipt_queries where tenant_id=$1 and query_id=$2 for update`, tenant, queryID).Scan(&source, &operation, &finished, &priorOutcome); err != nil {
			return err
		}
		if finished != nil && (priorOutcome == nil || *priorOutcome != outcome) {
			return agentrun.ErrActionConflict
		}
		if action.Authorization.TenantID != tenant || action.Authorization.OperationID != operation {
			return agentrun.ErrActionConflict
		}
		// Lock the authorization before inserting its effect child. FK locks
		// acquired after an effect unique-key wait would reverse CompleteAction's
		// authorization -> effect order across an earlier terminal family Run.
		original, err := readActionForReceipt(ctx, tx, tenant, action.Authorization.BusinessRequestID)
		if err != nil {
			return err
		}
		if original == nil || original.Authorization.OperationID != operation {
			return agentrun.ErrActionConflict
		}
		expectedHash, err := original.Authorization.Hash()
		actualHash, actualErr := action.Authorization.Hash()
		if err != nil || actualErr != nil || expectedHash != actualHash {
			return agentrun.ErrActionConflict
		}
		action = *original
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if receipt != nil {
			if err := saveEffect(ctx, tx, action, *receipt, source, now); err != nil {
				return err
			}
		}
		if finished == nil {
			if _, err := tx.Exec(ctx, `update action_receipt_queries set outcome=$3,finished_at=$4 where tenant_id=$1 and query_id=$2`, tenant, queryID, outcome, now); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `update tenant_receipt_query_gates set query_id=null,query_until=null where tenant_id=$1 and query_id=$2`, tenant, queryID)
		return err
	})
}

// AdmitActionRetry creates the sole terminal successor without replan or dispatch.
func (s *Store) AdmitActionRetry(ctx context.Context, input agentrun.Admission, expected business.SignedAction) (agentrun.SubmitResponse, error) {
	var result agentrun.SubmitResponse
	if input.Retry.Validate() != nil || !validUUID(input.RunID) || !validUUID(input.OperationID) || !validUUID(input.FirstStepID) || input.RequestHash != input.Retry.Hash(input.SourceRunID) {
		return result, agentrun.ErrInvalidArgument
	}
	err := s.transact(ctx, func(tx pgx.Tx) error {
		reused, err := resolveAdmission(ctx, tx, input.TenantID, input.OperationKey, input.SourceRunID, input.RequestHash, "")
		if err != nil {
			return err
		}
		if reused != nil {
			result = *reused
			return nil
		}
		b, err := readBusinessRequest(tx.QueryRow(ctx, "select "+businessRequestColumns+` from business_requests where tenant_id=$1 and business_request_id=(select business_request_id from runs where tenant_id=$1 and run_id=$2) for update`, input.TenantID, input.SourceRunID))
		if err != nil {
			return err
		}
		source, _, err := lockRun(ctx, tx, input.TenantID, input.SourceRunID)
		if err != nil {
			return err
		}
		// Re-resolve the direct successor after the family/source locks. This
		// avoids a unique violation and preserves same-source alias semantics.
		reused, err = resolveAdmission(ctx, tx, input.TenantID, input.OperationKey, input.SourceRunID, input.RequestHash, "")
		if err != nil {
			return err
		}
		if reused != nil {
			result = *reused
			return nil
		}
		action, err := readAction(ctx, tx, input.TenantID, b.ID, true)
		if err != nil {
			return err
		}
		if action == nil {
			return agentrun.ErrActionConflict
		}
		want, _ := expected.Authorization.Hash()
		actual, _ := action.Authorization.Hash()
		if want != actual {
			return agentrun.ErrActionConflict
		}
		effect, err := readEffect(ctx, tx, action)
		if err != nil {
			return err
		}
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if !b.RetryUntil.After(now) || source.State != agentrun.Failed && source.State != agentrun.Cancelled {
			return agentrun.ErrInvalidTransition
		}
		state, code, ref := agentrun.Failed, string(agentrun.ErrActionOutcomeUnknown), "action-unknown:"+action.Authorization.OperationID
		var outcome *string
		if effect.State == "applied" {
			state, code, ref = agentrun.Succeeded, "", *effect.ReceiptRef
			applied := "applied"
			outcome = &applied
		}
		_, err = tx.Exec(ctx, `insert into runs(run_id,tenant_id,business_request_id,business_request_key,ticket_id,retry_of_run_id,admission_hash,
			profile_id,profile_hash,budget_batch_id,snapshot_id,snapshot_hash,version_vector,ticket_binding,index_id,index_profile_hash,
			state,outcome,error_code,error_message,run_timeout_seconds,run_deadline,result_kind,result_ref,next_step_id,next_step_kind,next_input_hash,created_at,updated_at)
			select $3,tenant_id,business_request_id,business_request_key,ticket_id,run_id,$4,profile_id,profile_hash,budget_batch_id,
			snapshot_id,snapshot_hash,version_vector,ticket_binding,index_id,index_profile_hash,$5,$6,nullif($7,''),nullif($7,''),$8,$9,'final',$10,$11,
			'apply_ticket_resolution',next_input_hash,$12,$12 from runs where tenant_id=$1 and run_id=$2`, input.TenantID, input.SourceRunID, input.RunID, input.RequestHash, state, outcome, code,
			input.Retry.RunTimeoutSeconds, now.Add(time.Duration(input.Retry.RunTimeoutSeconds)*time.Second), ref, input.FirstStepID, now)
		if err != nil {
			return err
		}
		r, a, err := lockRun(ctx, tx, input.TenantID, input.RunID)
		if err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, &r, &a, "action_retry_recorded", now); err != nil {
			return err
		}
		if err := saveRun(ctx, tx, &r, &a); err != nil {
			return err
		}
		if err := insertOperation(ctx, tx, agentrun.Operation{ID: input.OperationID, TenantID: input.TenantID, Kind: "retry", SourceRunID: input.SourceRunID, Key: input.OperationKey, RequestHash: input.RequestHash, ResultRunID: input.RunID, CreatedAt: now}); err != nil {
			return err
		}
		result.Run, err = getView(ctx, tx, input.TenantID, input.RunID)
		return err
	})
	// Do not repeat the physical GET on an uncertain insertion result.
	if err != nil && !errors.Is(err, agentrun.ErrInvalidTransition) {
		reused, lookupErr := s.ResolveAdmission(ctx, input.TenantID, input.OperationKey, input.SourceRunID, input.RequestHash, "")
		if lookupErr == nil && reused != nil {
			return *reused, nil
		}
	}
	return result, err
}

// Compile-time assertions keep the network-free store boundary explicit.
var _ agentrun.ActionRecoveryStore = (*Store)(nil)
