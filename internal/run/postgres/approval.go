package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

func approvalView(ctx context.Context, tx pgx.Tx, r agentrun.Run, now time.Time) (agentrun.ApprovalView, string, error) {
	v := agentrun.ApprovalView{RunID: r.ID}
	if r.ContentPurgedAt != nil {
		return v, "", agentrun.ErrResultExpired
	}
	err := tx.QueryRow(ctx, `select status,proposal_hash,proposal_ref,permission_expires_at,
		decision_operation_id,actor_id,decided_at from run_approvals where tenant_id=$1 and run_id=$2`, r.TenantID, r.ID).
		Scan(&v.Status, &v.ProposalHash, &v.ProposalRef, &v.PermissionExpiresAt, &v.ApprovalID, &v.ActorID, &v.DecidedAt)
	if err != nil {
		return v, "", err
	}
	var output []byte
	var commit string
	err = tx.QueryRow(ctx, `select output,commit_hash from run_steps
		where tenant_id=$1 and run_id=$2 and kind='submit_proposal'`, r.TenantID, r.ID).Scan(&output, &commit)
	if err != nil {
		return v, "", err
	}
	var result agentrun.StepResult
	if json.Unmarshal(output, &result) != nil || result.Proposal == nil {
		return v, "", agentrun.ErrInternal
	}
	v.Proposal = result.Proposal
	raw, _ := json.Marshal(v.Proposal)
	if agentrun.Fingerprint("jobforge.run.proposal.v1", string(raw)) != v.ProposalHash {
		return v, "", agentrun.ErrInternal
	}
	v.Available = r.State == agentrun.AwaitingApproval && r.CancelRequestedAt == nil && r.RunDeadline.After(now) && v.PermissionExpiresAt.After(now)
	var definition []byte
	if err := tx.QueryRow(ctx, "select definition from agent_profiles where profile_id=$1 and profile_hash=$2", r.ProfileID, r.ProfileHash).Scan(&definition); err != nil {
		return v, "", err
	}
	var profile agentrun.Profile
	if json.Unmarshal(definition, &profile) != nil {
		return v, "", agentrun.ErrInternal
	}
	v.Available = v.Available && profile.ApprovalEnabled()
	return v, commit, nil
}

// Approval reads protected original proposal with tenant authorization.
func (s *Store) Approval(ctx context.Context, tenant, id string) (agentrun.ApprovalView, error) {
	var result agentrun.ApprovalView
	if !validUUID(id) || !agentrun.ValidIdentifier(tenant) {
		return result, agentrun.ErrInvalidArgument
	}
	err := s.readOnly(ctx, func(tx pgx.Tx) error {
		r, _, err := readRun(tx.QueryRow(ctx, "select "+runColumns+" from runs where tenant_id=$1 and run_id=$2", tenant, id))
		if err != nil {
			return err
		}
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		result, _, err = approvalView(ctx, tx, r, now)
		return err
	})
	return result, err
}

// DecideApproval atomically preserves first actor/decision and next registered cursor.
func (s *Store) DecideApproval(ctx context.Context, tenant, id, actor, key string, request agentrun.ApprovalRequest) (agentrun.ApprovalResponse, error) {
	var response agentrun.ApprovalResponse
	if !validUUID(id) || !agentrun.ValidIdentifier(tenant) || !agentrun.ValidIdentifier(actor) || !agentrun.ValidIdentifier(key) || request.Validate() != nil {
		return response, agentrun.ErrInvalidArgument
	}
	hash := request.Hash(tenant, id, actor)
	err := s.transact(ctx, func(tx pgx.Tx) error {
		r, a, err := lockRun(ctx, tx, tenant, id)
		if err != nil {
			return err
		}
		if r.ContentPurgedAt != nil {
			return agentrun.ErrResultExpired
		}
		prior, err := findOperation(ctx, tx, tenant, "approval", id, key)
		if err == nil {
			if prior.RequestHash != hash || prior.ResultRunID != id {
				return agentrun.ErrConflict
			}
			response.Reused = true
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		view, commit, err := approvalView(ctx, tx, r, now)
		if err != nil {
			return err
		}
		if !response.Reused {
			if request.ProposalHash != view.ProposalHash {
				return agentrun.ErrApprovalConflict
			}
			want := map[string]string{"approve": "approved", "reject": "rejected"}[request.Decision]
			if view.Status != "pending" {
				if view.Status != want || view.ActorID == nil || *view.ActorID != actor {
					return agentrun.ErrApprovalConflict
				}
				response.Reused = true
			} else {
				profile, err := s.ledgerProfile(ctx, tx, r, false)
				if err != nil {
					return err
				}
				if !profile.ApprovalEnabled() {
					return agentrun.ErrProfileUnavailable
				}
				if _, err := agentrun.ResolutionFromProposal(r.TicketID, view.Proposal); err != nil {
					return err
				}
				// Recheck time after reading all protected content, before deciding.
				now, err = databaseTime(ctx, tx)
				if err != nil {
					return err
				}
				if err := agentrun.DecideApproval(&r, &a, request, commit, hash, uuid.NewString(), now); err != nil {
					return err
				}
				approvalID := uuid.NewString()
				_, err = tx.Exec(ctx, `update run_approvals set status=$3,decision_operation_id=$4,actor_id=$5,decided_at=$6
					where tenant_id=$1 and run_id=$2 and status='pending'`, tenant, id, want, approvalID, actor, now)
				if err != nil {
					return err
				}
				if request.Decision == "reject" {
					_, err = tx.Exec(ctx, `update runs set result_kind='final',result_ref=$3 where tenant_id=$1 and run_id=$2`, tenant, id, "approval:"+approvalID)
					if err != nil {
						return err
					}
				}
				if err := appendEvent(ctx, tx, &r, &a, "approval_"+want, now); err != nil {
					return err
				}
				if err := saveRun(ctx, tx, &r, &a); err != nil {
					return err
				}
				prior.ID = approvalID
			}
			if prior.ID == "" {
				prior.ID = uuid.NewString()
			}
			if err := insertOperation(ctx, tx, agentrun.Operation{ID: prior.ID, TenantID: tenant, Kind: "approval", SourceRunID: id, Key: key, RequestHash: hash, ResultRunID: id, CreatedAt: now}); err != nil {
				return err
			}
		}
		response.Approval, _, err = approvalView(ctx, tx, r, now)
		if err != nil {
			return err
		}
		response.Run, err = getView(ctx, tx, tenant, id)
		return err
	})
	return response, err
}
