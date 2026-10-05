package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/xjfyrh/jobforge/internal/business"
	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

const actionCallColumns = `physical_call_id,tenant_id,run_id,worker_id,session_id,attempt_no,fencing_token,
	step,operation_id,authorization_hash,kind,status,reserved_at,dispatch_expires_at,call_deadline,
	transport_outcome,observation_hash,observed_at`

func readActionCall(row pgx.Row) (agentrun.ActionCall, error) {
	var c agentrun.ActionCall
	var step []byte
	err := row.Scan(&c.ID, &c.Lease.TenantID, &c.Lease.RunID, &c.Lease.WorkerID, &c.Lease.SessionID, &c.Lease.AttemptNo, &c.Lease.FencingToken,
		&step, &c.OperationID, &c.AuthorizationHash, &c.Kind, &c.Status, &c.ReservedAt, &c.DispatchExpiresAt, &c.Deadline, &c.TransportOutcome, &c.ObservationHash, &c.ObservedAt)
	if err != nil {
		return c, err
	}
	c.Step, err = agentrun.DecodeRecoveryStep(step)
	c.ProviderMetering = "not_applicable"
	return c, err
}

// ActionCalls reads the bounded independent original-Run permit ledger.
func (s *Store) ActionCalls(ctx context.Context, tenant, id string) (agentrun.ActionCallsResponse, error) {
	result := agentrun.ActionCallsResponse{Items: []agentrun.ActionCall{}}
	if !validUUID(id) || !agentrun.ValidIdentifier(tenant) {
		return result, agentrun.ErrInvalidArgument
	}
	err := s.readOnly(ctx, func(tx pgx.Tx) error {
		var found string
		if err := tx.QueryRow(ctx, "select run_id from runs where tenant_id=$1 and run_id=$2", tenant, id).Scan(&found); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, "select "+actionCallColumns+" from action_calls where tenant_id=$1 and run_id=$2 order by reserved_at,physical_call_id limit 9", tenant, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := readActionCall(rows)
			if err != nil {
				return err
			}
			result.Items = append(result.Items, c)
			if len(result.Items) > agentrun.MaxActionCalls {
				return agentrun.ErrInternal
			}
		}
		return rows.Err()
	})
	return result, err
}

// ReserveActionCall spends one durable physical permit; replay never dispatches.
func (s *Store) ReserveActionCall(ctx context.Context, principal string, req agentrun.ReserveActionCallRequest) (agentrun.ReserveActionCallResponse, error) {
	var result agentrun.ReserveActionCallResponse
	if validateLedgerLease(req.Lease) != nil || !validUUID(req.ID) || !validUUID(req.OperationID) || !agentrun.ValidHash(req.AuthorizationHash) ||
		(req.Kind != "receipt_query" && req.Kind != "action_write") {
		return result, agentrun.ErrInvalidArgument
	}
	err := s.transact(ctx, func(tx pgx.Tx) error {
		r, a, err := lockRun(ctx, tx, req.Lease.TenantID, req.Lease.RunID)
		if err != nil {
			return err
		}
		var accounts [3]budgetRow
		if req.Kind == "action_write" {
			accounts, err = loadAccounts(ctx, tx, r.TenantID, r.BusinessRequestID, true)
			if err != nil {
				return err
			}
		}
		action, err := readAction(ctx, tx, r.TenantID, r.BusinessRequestID, true)
		if err != nil {
			return err
		}
		if action == nil {
			return agentrun.ErrInvalidTransition
		}
		hash, _ := action.Authorization.Hash()
		if action.Authorization.RunID != r.ID || action.Authorization.OperationID != req.OperationID || hash != req.AuthorizationHash {
			return agentrun.ErrActionConflict
		}
		prior, priorErr := readActionCall(tx.QueryRow(ctx, "select "+actionCallColumns+" from action_calls where tenant_id=$1 and run_id=$2 and physical_call_id=$3 for update", r.TenantID, r.ID, req.ID))
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.actionExecution(ctx, tx, principal, r, a, req.Lease, req.Step, now); err != nil {
			return err
		}
		if priorErr == nil {
			if prior.Lease != req.Lease || prior.Step != req.Step || prior.Kind != req.Kind || prior.OperationID != req.OperationID || prior.AuthorizationHash != req.AuthorizationHash {
				return agentrun.ErrCallConflict
			}
			result.Call = prior
			return nil
		}
		if !errors.Is(priorErr, pgx.ErrNoRows) {
			return priorErr
		}
		var queries, writes int
		if err := tx.QueryRow(ctx, `select query_count,write_count from action_authorizations where tenant_id=$1 and operation_id=$2`, r.TenantID, req.OperationID).Scan(&queries, &writes); err != nil {
			return err
		}
		if req.Kind == "action_write" {
			if writes >= 4 {
				return agentrun.ErrBudgetExhausted
			}
			p, err := s.ledgerProfile(ctx, tx, r, true)
			if err != nil {
				return err
			}
			if err := accountsAvailable(accounts, now); err != nil {
				return err
			}
			if err := checkBatchAuditGuard(ctx, tx, accounts[2].Account.ID, p, r, a, now); err != nil {
				return err
			}
			if action.Authorization.AuthorizationExpiresAt <= now.UnixMicro() {
				return agentrun.ErrActionAuthorizationExpired
			}
		} else if queries >= 4 {
			return agentrun.ErrBudgetExhausted
		}
		var already bool
		if err := tx.QueryRow(ctx, `select exists(select 1 from action_calls where tenant_id=$1 and run_id=$2 and attempt_no=$3 and kind=$4)`, r.TenantID, r.ID, r.AttemptNo, req.Kind).Scan(&already); err != nil {
			return err
		}
		if already {
			return agentrun.ErrCallConflict
		}
		now, err = databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.actionExecution(ctx, tx, principal, r, a, req.Lease, req.Step, now); err != nil {
			return err
		}
		deadline := now.Add(10 * time.Second)
		for _, bound := range []time.Time{*r.LeaseUntil, *r.AttemptDeadline, r.RunDeadline} {
			if bound.Before(deadline) {
				deadline = bound
			}
		}
		if req.Kind == "action_write" {
			if err := accountsAvailable(accounts, now); err != nil {
				return err
			}
			expires := time.UnixMicro(action.Authorization.AuthorizationExpiresAt)
			if !expires.After(now) {
				return agentrun.ErrActionAuthorizationExpired
			}
			if expires.Before(deadline) {
				deadline = expires
			}
		}
		dispatch := now.Add(2 * time.Second)
		if deadline.Before(dispatch) {
			dispatch = deadline
		}
		c := agentrun.ActionCall{ID: req.ID, Lease: req.Lease, Step: req.Step, OperationID: req.OperationID, AuthorizationHash: req.AuthorizationHash,
			Kind: req.Kind, Status: "reserved", ReservedAt: now, DispatchExpiresAt: dispatch, Deadline: deadline, ProviderMetering: "not_applicable"}
		step, _ := json.Marshal(req.Step)
		_, err = tx.Exec(ctx, `insert into action_calls(physical_call_id,tenant_id,run_id,worker_id,session_id,attempt_no,fencing_token,step,
			operation_id,authorization_hash,kind,status,reserved_at,dispatch_expires_at,call_deadline)
			values($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'reserved',$12,$13,$14)`, c.ID, r.TenantID, r.ID, req.Lease.WorkerID, req.Lease.SessionID, r.AttemptNo,
			req.Lease.FencingToken, step, c.OperationID, c.AuthorizationHash, c.Kind, now, dispatch, deadline)
		if err != nil {
			return err
		}
		column := "query_count"
		if req.Kind == "action_write" {
			column = "write_count"
		}
		if _, err := tx.Exec(ctx, "update action_authorizations set "+column+"="+column+"+1 where tenant_id=$1 and operation_id=$2", r.TenantID, c.OperationID); err != nil {
			return err
		}
		result.Call, result.NewlyReserved = c, true
		return nil
	})
	return result, err
}

// ObserveActionCall records only the original physical call's facts. It cannot
// revive execution, change a cursor or clear a successor's active state.
func (s *Store) ObserveActionCall(ctx context.Context, principal string, req agentrun.ObserveActionCallRequest) (agentrun.ActionCall, error) {
	var result agentrun.ActionCall
	if validateLedgerLease(req.Lease) != nil || !validUUID(req.ID) || !agentrun.ValidHash(req.ObservationHash) ||
		(req.TransportOutcome != "response" && req.TransportOutcome != "timeout" && req.TransportOutcome != "network_error") {
		return result, agentrun.ErrInvalidArgument
	}
	err := s.transact(ctx, func(tx pgx.Tx) error {
		r, _, err := lockRun(ctx, tx, req.Lease.TenantID, req.Lease.RunID)
		if err != nil {
			return err
		}
		c, err := readActionCall(tx.QueryRow(ctx, "select "+actionCallColumns+" from action_calls where tenant_id=$1 and run_id=$2 and physical_call_id=$3 for update", r.TenantID, r.ID, req.ID))
		if err != nil {
			return err
		}
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.checkSession(ctx, tx, principal, req.Lease.WorkerID, req.Lease.SessionID, r.TenantID, "", now, false); err != nil {
			return err
		}
		if c.Lease != req.Lease {
			return agentrun.ErrStaleLease
		}
		if c.ObservationHash != nil {
			if *c.ObservationHash != req.ObservationHash || *c.TransportOutcome != req.TransportOutcome {
				return agentrun.ErrCallConflict
			}
			result = c
			return nil
		}
		_, err = tx.Exec(ctx, `update action_calls set status='observed',transport_outcome=$4,observation_hash=$5,observed_at=$6
			where tenant_id=$1 and run_id=$2 and physical_call_id=$3`, r.TenantID, r.ID, c.ID, req.TransportOutcome, req.ObservationHash, now)
		c.Status, c.TransportOutcome, c.ObservationHash, c.ObservedAt = "observed", &req.TransportOutcome, &req.ObservationHash, &now
		result = c
		return err
	})
	return result, err
}

func actionOutput(receipt business.ActionReceipt) ([]byte, error) {
	content, _ := json.Marshal(receipt)
	raw, err := json.Marshal(agentrun.StepResult{SchemaVersion: 1, EvidenceRefs: []string{}, Content: content})
	if err != nil {
		return nil, err
	}
	return agentrun.CanonicalCheckpointJSON(raw)
}

// CompleteAction commits current fenced completion and first receipt atomically.
func (s *Store) CompleteAction(ctx context.Context, principal string, req agentrun.CompleteActionRequest) (agentrun.CommitStepResponse, error) {
	var result agentrun.CommitStepResponse
	if validateLedgerLease(req.Lease) != nil {
		return result, agentrun.ErrInvalidArgument
	}
	err := s.transact(ctx, func(tx pgx.Tx) error {
		r, a, err := lockRun(ctx, tx, req.Lease.TenantID, req.Lease.RunID)
		if err != nil {
			return err
		}
		action, err := readAction(ctx, tx, r.TenantID, r.BusinessRequestID, true)
		if err != nil {
			return err
		}
		if action == nil || req.Receipt.Validate(*action) != nil {
			return agentrun.ErrActionConflict
		}
		effect, err := readEffect(ctx, tx, action)
		if err != nil {
			return err
		}
		source := "action_response"
		if effect.State != "applied" {
			if !validUUID(req.PhysicalCallID) {
				return agentrun.ErrCallConflict
			}
			call, err := readActionCall(tx.QueryRow(ctx, "select "+actionCallColumns+" from action_calls where tenant_id=$1 and run_id=$2 and physical_call_id=$3 for update", r.TenantID, r.ID, req.PhysicalCallID))
			if err != nil {
				return err
			}
			if call.Lease != req.Lease || call.Step != req.Step || call.Status != "observed" || call.TransportOutcome == nil || *call.TransportOutcome != "response" || call.ObservationHash == nil || *call.ObservationHash != agentrun.Fingerprint("jobforge.run.action-receipt-observation.v1", req.Receipt.ReceiptHash) {
				return agentrun.ErrCallConflict
			}
			if call.Kind == "receipt_query" {
				source = "worker_query"
			}
		}
		resources, err := lockAttemptResources(ctx, tx, r, a)
		if err != nil {
			return err
		}
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.actionExecution(ctx, tx, principal, r, a, req.Lease, req.Step, now); err != nil {
			return err
		}
		if err := saveEffect(ctx, tx, *action, req.Receipt, source, now); err != nil {
			return err
		}
		output, err := actionOutput(req.Receipt)
		if err != nil {
			return err
		}
		if r.CursorVersion >= 32 || a.CheckpointBytes+int64(len(output)) > agentrun.MaxCheckpointBytes {
			return agentrun.ErrCheckpointTooLarge
		}
		// All blocking child locks have been acquired before the final authority check.
		now, err = databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.actionExecution(ctx, tx, principal, r, a, req.Lease, req.Step, now); err != nil {
			return err
		}
		r.CursorVersion++
		a.CheckpointBytes += int64(len(output))
		outcome := "applied"
		r.State, r.Outcome, r.UpdatedAt = agentrun.Succeeded, &outcome, now
		hash := agentrun.CommitHash(req.Step, output)
		ref := agentrun.StepReference(r.ID, req.Step.Sequence)
		_, err = tx.Exec(ctx, `insert into run_steps(tenant_id,run_id,attempt_no,sequence,step_id,kind,input_hash,profile_hash,snapshot_hash,
			commit_hash,output_ref,output,output_bytes,cursor_version,created_at) values($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			r.TenantID, r.ID, r.AttemptNo, req.Step.Sequence, req.Step.ID, req.Step.Kind, req.Step.InputHash, req.Step.ProfileHash, req.Step.SnapshotHash, hash, ref, output, len(output), r.CursorVersion, now)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `update runs set result_kind='final',result_ref=$3 where tenant_id=$1 and run_id=$2`, r.TenantID, r.ID, "action-receipt:"+req.Receipt.OperationID); err != nil {
			return err
		}
		if err := closeAttemptRecords(ctx, tx, &r, &a, resources, "succeeded", now); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, &r, &a, "action_completed", now); err != nil {
			return err
		}
		if err := saveRun(ctx, tx, &r, &a); err != nil {
			return err
		}
		result = commitResponse(r, a, agentrun.AcceptedStep{Identity: req.Step, CommitHash: hash, ResultJSON: output, ResultRef: ref}, true)
		return nil
	})
	return result, err
}
