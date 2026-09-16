package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

// Admit persists an externally captured, validated snapshot. No network call is
// made under these locks. An insert race rolls the entire transaction back;
// its caller resolves the accepted identity instead of repeating capture.
func (s *Store) Admit(ctx context.Context, input agentrun.Admission) (agentrun.SubmitResponse, error) {
	var response agentrun.SubmitResponse
	if !agentrun.ValidIdentifier(input.TenantID) || !agentrun.ValidIdentifier(input.OperationKey) ||
		!validUUID(input.RunID) || !validUUID(input.OperationID) || !validUUID(input.FirstStepID) ||
		(input.SourceRunID != "" && !validUUID(input.SourceRunID)) {
		return response, agentrun.ErrInvalidArgument
	}
	err := s.transact(ctx, func(tx pgx.Tx) error {
		reused, err := resolveAdmission(ctx, tx, input.TenantID, input.OperationKey, input.SourceRunID,
			input.RequestHash, input.Submit.BusinessRequestKey)
		if err != nil {
			return err
		}
		if reused != nil {
			response = *reused
			return nil
		}
		business, source, err := s.prepareBusiness(ctx, tx, input)
		if err != nil {
			return err
		}
		accounts, err := loadAccounts(ctx, tx, input.TenantID, business.ID, true)
		if err != nil {
			return err
		}
		// Account contention may outlast a batch or retry window. Never authorize
		// from a timestamp taken before the final potentially blocking lock.
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		profile, err := s.Profile(ctx, input.Profile.ID)
		if err != nil {
			return err
		}
		if profile.Hash != input.Profile.Hash || (input.SourceRunID != "" && source.ProfileHash != profile.Hash) {
			return agentrun.ErrProfileUnavailable
		}
		for _, account := range accounts {
			if account.Account.Frozen || account.ValidFrom.After(now) || !account.ValidUntil.After(now) {
				return agentrun.ErrBudgetExhausted
			}
		}
		if input.SourceRunID != "" && (!business.RetryUntil.After(now) ||
			(source.State != agentrun.Failed && source.State != agentrun.Cancelled)) {
			return agentrun.ErrInvalidTransition
		}
		if input.SourceRunID == "" {
			// These provisional rows are invisible outside this transaction. Their
			// authoritative acceptance time begins after all account contention.
			business.CreatedAt, business.RetryUntil = now, now.Add(7*24*time.Hour)
			if _, err := tx.Exec(ctx, `update business_requests set created_at=$3,retry_until=$4
				where tenant_id=$1 and business_request_id=$2`, input.TenantID, business.ID, now, business.RetryUntil); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "update budget_accounts set valid_from=$2,valid_until=$3 where account_id=$1",
				business.FamilyAccountID, now, business.RetryUntil.Add(24*time.Hour)); err != nil {
				return err
			}
		}
		r, authority, err := admissionRun(input, business, source, now)
		if err != nil {
			return err
		}
		if err := insertRun(ctx, tx, input, r, authority); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, &r, &authority, "admitted", now); err != nil {
			return err
		}
		if err := saveRun(ctx, tx, &r, &authority); err != nil {
			return err
		}
		kind := "submit"
		if input.SourceRunID != "" {
			kind = "retry"
		}
		if err := insertOperation(ctx, tx, agentrun.Operation{ID: input.OperationID, TenantID: input.TenantID,
			Kind: kind, SourceRunID: input.SourceRunID, Key: input.OperationKey, RequestHash: input.RequestHash,
			ResultRunID: r.ID, CreatedAt: now}); err != nil {
			return err
		}
		response.Run, err = getView(ctx, tx, input.TenantID, r.ID)
		return err
	})
	return response, err
}

func (s *Store) prepareBusiness(ctx context.Context, tx pgx.Tx, input agentrun.Admission) (agentrun.BusinessRequest, agentrun.Run, error) {
	var source agentrun.Run
	if input.SourceRunID != "" {
		business, err := readBusinessRequest(tx.QueryRow(ctx, "select "+businessRequestColumns+` from business_requests
			where tenant_id=$1 and business_request_id=(select business_request_id from runs where tenant_id=$1 and run_id=$2) for update`, input.TenantID, input.SourceRunID))
		if err != nil {
			return business, source, err
		}
		source, _, err = lockRun(ctx, tx, input.TenantID, input.SourceRunID)
		if err != nil {
			return business, source, err
		}
		if input.Retry.Validate() != nil || input.RequestHash != input.Retry.Hash(input.SourceRunID) ||
			input.Profile.ID != source.ProfileID || input.Submit.TicketID != source.TicketID ||
			input.Submit.BudgetBatchID != source.BudgetBatchID || input.Submit.BusinessRequestKey != business.Key {
			return business, source, agentrun.ErrInvalidArgument
		}
		return business, source, nil
	}
	if input.Submit.Validate() != nil || input.RequestHash != input.Submit.Hash() ||
		input.Profile.ID != input.Submit.ProfileID || !validUUID(input.BusinessID) || !validUUID(input.FamilyAccountID) {
		return agentrun.BusinessRequest{}, source, agentrun.ErrInvalidArgument
	}
	now, err := databaseTime(ctx, tx)
	if err != nil {
		return agentrun.BusinessRequest{}, source, err
	}
	tenantAccount, batchAccount, err := admissionAccounts(ctx, tx, input.TenantID, input.Submit.BudgetBatchID, now, false)
	if err != nil {
		return agentrun.BusinessRequest{}, source, err
	}
	business := agentrun.BusinessRequest{ID: input.BusinessID, TenantID: input.TenantID,
		Key: input.Submit.BusinessRequestKey, RequestHash: input.RequestHash, RootRunID: input.RunID,
		FamilyAccountID: input.FamilyAccountID, TenantAccountID: tenantAccount.Account.ID, BatchAccountID: batchAccount.Account.ID,
		CreatedAt: now, RetryUntil: now.Add(7 * 24 * time.Hour)}
	if err := insertBudget(ctx, tx, agentrun.BudgetSpec{ID: business.FamilyAccountID, Scope: "family", Key: business.ID,
		ValidFrom: now, ValidUntil: business.RetryUntil.Add(24 * time.Hour),
		Limits: agentrun.FamilyLimits(input.Profile.FamilyTokenLimit, input.Profile.FamilyCostMicroyuan)}); err != nil {
		return business, source, err
	}
	_, err = tx.Exec(ctx, `insert into business_requests (`+businessRequestColumns+`)
		values($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, business.ID, business.TenantID, business.Key, business.RequestHash,
		business.RootRunID, business.FamilyAccountID, business.TenantAccountID, business.BatchAccountID, business.CreatedAt, business.RetryUntil)
	return business, source, err
}

func admissionRun(input agentrun.Admission, business agentrun.BusinessRequest, source agentrun.Run, now time.Time) (agentrun.Run, agentrun.Authority, error) {
	snapshot := input.Snapshot
	if snapshot.TenantID != input.TenantID || snapshot.TicketID != input.Submit.TicketID ||
		!validUUID(snapshot.ID) || !validUUID(snapshot.IndexID) {
		return agentrun.Run{}, agentrun.Authority{}, agentrun.ErrInvalidArgument
	}
	timeout := input.Submit.RunTimeoutSeconds
	var retryOf *string
	if input.SourceRunID != "" {
		timeout, retryOf = input.Retry.RunTimeoutSeconds, &source.ID
	}
	r := agentrun.Run{ID: input.RunID, TenantID: input.TenantID, BusinessRequestID: business.ID,
		BusinessRequestKey: business.Key, TicketID: input.Submit.TicketID, RetryOfRunID: retryOf,
		ProfileID: input.Profile.ID, ProfileHash: input.Profile.Hash, BudgetBatchID: input.Submit.BudgetBatchID,
		SnapshotID: snapshot.ID, SnapshotHash: snapshot.ContentHash, VersionVector: snapshot.VersionVector,
		State: agentrun.Ready, RunTimeoutSeconds: timeout, RunDeadline: now.Add(time.Duration(timeout) * time.Second),
		CreatedAt: now, UpdatedAt: now}
	a := agentrun.Authority{NextStepID: input.FirstStepID, NextStepKind: "read_ticket",
		CheckpointBytes: int64(len(snapshot.Ticket) + len(snapshot.VersionVector)),
		NextInputHash:   agentrun.InitialStepInput(input.Profile.Hash, snapshot.ContentHash)}
	if a.CheckpointBytes > agentrun.MaxCheckpointBytes {
		return agentrun.Run{}, agentrun.Authority{}, agentrun.ErrorCode("CHECKPOINT_TOO_LARGE")
	}
	return r, a, nil
}

func insertRun(ctx context.Context, tx pgx.Tx, input agentrun.Admission, r agentrun.Run, a agentrun.Authority) error {
	_, err := tx.Exec(ctx, `insert into runs (run_id,tenant_id,business_request_id,business_request_key,ticket_id,
		retry_of_run_id,admission_hash,profile_id,profile_hash,budget_batch_id,snapshot_id,snapshot_hash,
		version_vector,ticket_binding,index_id,index_profile_hash,state,run_timeout_seconds,run_deadline,
		next_step_id,next_step_kind,next_input_hash,created_at,updated_at,checkpoint_bytes)
		values($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$23,$24)`,
		r.ID, r.TenantID, r.BusinessRequestID, r.BusinessRequestKey, r.TicketID, r.RetryOfRunID, input.RequestHash,
		r.ProfileID, r.ProfileHash, r.BudgetBatchID, r.SnapshotID, r.SnapshotHash, r.VersionVector,
		input.Snapshot.Ticket, input.Snapshot.IndexID, input.Snapshot.IndexProfileHash,
		r.State, r.RunTimeoutSeconds, r.RunDeadline, a.NextStepID, a.NextStepKind, a.NextInputHash, r.CreatedAt, a.CheckpointBytes)
	return err
}

// ResolveAdmissionFailure performs one read/alias resolution after an uncertain
// insert result. It never retries a physical capture or manufactures success.
func (s *Store) ResolveAdmissionFailure(ctx context.Context, input agentrun.Admission, failure error) (agentrun.SubmitResponse, error) {
	if !errors.Is(failure, agentrun.ErrConflict) && !errors.Is(failure, agentrun.ErrDependencyUnavailable) {
		return agentrun.SubmitResponse{}, failure
	}
	reused, err := s.ResolveAdmission(ctx, input.TenantID, input.OperationKey, input.SourceRunID,
		input.RequestHash, input.Submit.BusinessRequestKey)
	if err != nil {
		return agentrun.SubmitResponse{}, err
	}
	if reused == nil {
		return agentrun.SubmitResponse{}, failure
	}
	return *reused, nil
}
