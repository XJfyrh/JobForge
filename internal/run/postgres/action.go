package postgres

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/xjfyrh/jobforge/internal/business"
	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

func readAction(ctx context.Context, tx pgx.Tx, tenant, businessID string, lock bool) (*business.SignedAction, error) {
	query := `select action from action_authorizations where tenant_id=$1 and business_request_id=$2`
	if lock {
		query += " for update"
	}
	return decodeActionRow(tx.QueryRow(ctx, query, tenant, businessID))
}

// Receipt paths lock immutable identity before effect/gate children. KEY SHARE
// matches their FK lock and does not serialize independent receipt readers.
func readActionForReceipt(ctx context.Context, tx pgx.Tx, tenant, businessID string) (*business.SignedAction, error) {
	return decodeActionRow(tx.QueryRow(ctx, `select action from action_authorizations
		where tenant_id=$1 and business_request_id=$2 for key share`, tenant, businessID))
}

func decodeActionRow(row pgx.Row) (*business.SignedAction, error) {
	var raw []byte
	err := row.Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	action, err := business.DecodeSignedAction(raw)
	if err != nil {
		return nil, agentrun.ErrInternal
	}
	return &action, nil
}

func readEffect(ctx context.Context, tx pgx.Tx, action *business.SignedAction) (agentrun.EffectView, error) {
	v := agentrun.EffectView{State: "none"}
	if action == nil {
		return v, nil
	}
	a := action.Authorization
	hash, _ := a.Hash()
	v.State, v.OperationID, v.AuthorizationHash = "unknown", &a.OperationID, &hash
	var raw []byte
	err := tx.QueryRow(ctx, `select receipt,effect_version,source,observed_at from action_receipt_views
		where tenant_id=$1 and operation_id=$2`, a.TenantID, a.OperationID).Scan(&raw, &v.EffectVersion, &v.Source, &v.ObservedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	var receipt business.ActionReceipt
	if json.Unmarshal(raw, &receipt) != nil || receipt.Validate(*action) != nil {
		return v, agentrun.ErrInternal
	}
	ref := "action-receipt:" + a.OperationID
	v.State, v.Receipt, v.ReceiptRef = "applied", &receipt, &ref
	return v, nil
}

func saveEffect(ctx context.Context, tx pgx.Tx, action business.SignedAction, receipt business.ActionReceipt, source string, now time.Time) error {
	if receipt.Validate(action) != nil {
		return agentrun.ErrActionConflict
	}
	raw, _ := json.Marshal(receipt)
	_, err := tx.Exec(ctx, `insert into action_receipt_views(tenant_id,operation_id,receipt,receipt_hash,source,observed_at)
		values($1,$2,$3,$4,$5,$6) on conflict(tenant_id,operation_id) do nothing`, receipt.TenantID, receipt.OperationID, raw, receipt.ReceiptHash, source, now)
	if err != nil {
		return err
	}
	var same bool
	err = tx.QueryRow(ctx, `select receipt_hash=$3 and receipt=$4::jsonb from action_receipt_views
		where tenant_id=$1 and operation_id=$2`, receipt.TenantID, receipt.OperationID, receipt.ReceiptHash, raw).Scan(&same)
	if err != nil {
		return err
	}
	if !same {
		return agentrun.ErrActionConflict
	}
	return nil
}

// Effect reads tenant-authorized monotonic business facts.
func (s *Store) Effect(ctx context.Context, tenant, id string) (agentrun.EffectView, error) {
	var result agentrun.EffectView
	if !validUUID(id) || !agentrun.ValidIdentifier(tenant) {
		return result, agentrun.ErrInvalidArgument
	}
	err := s.readOnly(ctx, func(tx pgx.Tx) error {
		var businessID string
		if err := tx.QueryRow(ctx, "select business_request_id from runs where tenant_id=$1 and run_id=$2", tenant, id).Scan(&businessID); err != nil {
			return err
		}
		action, err := readAction(ctx, tx, tenant, businessID, false)
		if err != nil {
			return err
		}
		result, err = readEffect(ctx, tx, action)
		return err
	})
	return result, err
}

func (s *Store) actionExecution(ctx context.Context, tx pgx.Tx, principal string, r agentrun.Run, a agentrun.Authority, lease agentrun.Lease, step agentrun.StepIdentity, now time.Time) error {
	if step.Kind != business.ResolutionOperation {
		return agentrun.ErrInvalidTransition
	}
	if err := s.ledgerExecution(ctx, tx, principal, r, a, lease, step, now); err != nil {
		return err
	}
	p, err := s.ledgerProfile(ctx, tx, r, false)
	if err != nil {
		return err
	}
	if !p.ApprovalEnabled() {
		return agentrun.ErrProfileUnavailable
	}
	return nil
}

// GetAction requires current fenced authority before exposing protected content.
func (s *Store) GetAction(ctx context.Context, principal string, lease agentrun.Lease, step agentrun.StepIdentity) (agentrun.ActionBinding, error) {
	var result agentrun.ActionBinding
	if validateLedgerLease(lease) != nil {
		return result, agentrun.ErrInvalidArgument
	}
	err := s.transact(ctx, func(tx pgx.Tx) error {
		r, a, err := lockRun(ctx, tx, lease.TenantID, lease.RunID)
		if err != nil {
			return err
		}
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.actionExecution(ctx, tx, principal, r, a, lease, step, now); err != nil {
			return err
		}
		result.Action, err = readAction(ctx, tx, r.TenantID, r.BusinessRequestID, false)
		if err != nil {
			return err
		}
		result.Effect, err = readEffect(ctx, tx, result.Action)
		return err
	})
	return result, err
}

// AuthorizeAction atomically freezes one approved operation under original guards.
func (s *Store) AuthorizeAction(ctx context.Context, principal string, lease agentrun.Lease, step agentrun.StepIdentity) (agentrun.ActionBinding, error) {
	var result agentrun.ActionBinding
	if validateLedgerLease(lease) != nil {
		return result, agentrun.ErrInvalidArgument
	}
	err := s.transact(ctx, func(tx pgx.Tx) error {
		r, a, err := lockRun(ctx, tx, lease.TenantID, lease.RunID)
		if err != nil {
			return err
		}
		accounts, err := loadAccounts(ctx, tx, r.TenantID, r.BusinessRequestID, true)
		if err != nil {
			return err
		}
		action, err := readAction(ctx, tx, r.TenantID, r.BusinessRequestID, true)
		if err != nil {
			return err
		}
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.actionExecution(ctx, tx, principal, r, a, lease, step, now); err != nil {
			return err
		}
		if action == nil {
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
			view, _, err := approvalView(ctx, tx, r, now)
			if err != nil {
				return err
			}
			if view.Status != "approved" || view.ApprovalID == nil || view.ActorID == nil || view.DecidedAt == nil {
				return agentrun.ErrApprovalConflict
			}
			params, err := agentrun.ResolutionFromProposal(r.TicketID, view.Proposal)
			if err != nil {
				return err
			}
			definition, err := agentrun.DecodeSupportDefinition(p.Definition)
			if err != nil {
				return err
			}
			key := s.signers[definition.Action.KeyID]
			if len(key) != ed25519.PrivateKeySize || business.PublicKeyHash(key.Public().(ed25519.PublicKey)) != definition.Action.PublicKeySHA256 {
				return agentrun.ErrProfileUnavailable
			}
			var created time.Time
			if err := tx.QueryRow(ctx, "select created_at from business_requests where tenant_id=$1 and business_request_id=$2", r.TenantID, r.BusinessRequestID).Scan(&created); err != nil {
				return err
			}
			var vector business.VersionVector
			if json.Unmarshal(r.VersionVector, &vector) != nil {
				return agentrun.ErrInternal
			}
			now, err = databaseTime(ctx, tx)
			if err != nil {
				return err
			}
			if err := s.actionExecution(ctx, tx, principal, r, a, lease, step, now); err != nil {
				return err
			}
			if r.PermissionExpiresAt == nil || !r.PermissionExpiresAt.After(now) {
				return agentrun.ErrActionAuthorizationExpired
			}
			if err := accountsAvailable(accounts, now); err != nil {
				return err
			}
			hash, _ := params.Hash()
			authorization := business.ActionAuthorization{SchemaVersion: 1, KeyID: definition.Action.KeyID, Operation: business.ResolutionOperation,
				TenantID: r.TenantID, BusinessRequestID: r.BusinessRequestID, BusinessRequestCreatedAt: created.UnixMicro(), OperationID: uuid.NewString(), RunID: r.ID,
				ApprovalID: *view.ApprovalID, ActorID: *view.ActorID, DecidedAt: view.DecidedAt.UnixMicro(), ProposalHash: view.ProposalHash, ParametersHash: hash,
				SnapshotID: r.SnapshotID, SnapshotHash: r.SnapshotHash, VersionVector: vector, AuthorizedAt: now.UnixMicro(), PermissionExpiresAt: r.PermissionExpiresAt.UnixMicro(),
				AuthorizationExpiresAt: r.PermissionExpiresAt.UnixMicro(), RunDeadline: r.RunDeadline.UnixMicro()}
			signed, err := business.SignAction(authorization, params, key)
			if err != nil {
				return agentrun.ErrInternal
			}
			raw, _ := json.Marshal(signed)
			hash, _ = authorization.Hash()
			_, err = tx.Exec(ctx, `insert into action_authorizations(tenant_id,business_request_id,authorizing_run_id,operation_id,approval_id,
				authorization_hash,action,authorized_at,expires_at) values($1,$2,$3,$4,$5,$6,$7,$8,$9)`, r.TenantID, r.BusinessRequestID, r.ID,
				authorization.OperationID, authorization.ApprovalID, hash, raw, now, r.PermissionExpiresAt)
			if err != nil {
				return err
			}
			action = &signed
		}
		if action.Authorization.RunID != r.ID {
			return agentrun.ErrActionConflict
		}
		result.Action = action
		result.Effect, err = readEffect(ctx, tx, action)
		return err
	})
	return result, err
}
