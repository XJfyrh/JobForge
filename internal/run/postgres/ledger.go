package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

type toolRow struct {
	ID, TenantID, RunID, StepID, Name, InputHash string
	AttemptNo, FencingToken                      int64
}

type callRow struct {
	Reservation                         agentrun.CallReservation
	Lease                               agentrun.Lease
	StepID, StepKind, Kind, ProfileHash string
	Status                              string
	ObservationHash, UsageHash          *string
	TransportOutcome, BusinessOutcome   *string
}

func readTool(ctx context.Context, tx pgx.Tx, id string) (toolRow, error) {
	var t toolRow
	err := tx.QueryRow(ctx, `select invocation_id,tenant_id,run_id,step_id,tool_name,input_hash,
		attempt_no,fencing_token from tool_invocations where invocation_id=$1 for update`, id).Scan(
		&t.ID, &t.TenantID, &t.RunID, &t.StepID, &t.Name, &t.InputHash, &t.AttemptNo, &t.FencingToken)
	return t, err
}

func toolMatches(t toolRow, req agentrun.BeginToolRequest) bool {
	return t.TenantID == req.Lease.TenantID && t.RunID == req.Lease.RunID && t.StepID == req.Step.ID &&
		t.Name == req.Step.Kind && t.InputHash == req.Step.InputHash &&
		t.AttemptNo == req.Lease.AttemptNo && t.FencingToken == req.Lease.FencingToken
}

func readCall(ctx context.Context, tx pgx.Tx, tenant, runID, id string) (callRow, error) {
	var c callRow
	var usage []byte
	err := tx.QueryRow(ctx, `select physical_call_id,coalesce(tool_invocation_id::text,''),subcall,input_hash,price_hash,
		ordinal,reserved_at,dispatch_expires_at,call_deadline,reserved_tokens,reserved_cost_microyuan,
		tenant_id,run_id,worker_id,session_id,attempt_no,fencing_token,step_id,step_kind,kind,profile_hash,
		status,observation_hash,usage_hash,transport_outcome,business_outcome,measurement_anomaly,usage
		from physical_calls where tenant_id=$1 and run_id=$2 and physical_call_id=$3 for update`, tenant, runID, id).Scan(
		&c.Reservation.PhysicalCallID, &c.Reservation.ToolInvocationID, &c.Reservation.Subcall, &c.Reservation.ParameterHash, &c.Reservation.PriceHash,
		&c.Reservation.Ordinal, &c.Reservation.ReservedAt, &c.Reservation.DispatchExpiresAt, &c.Reservation.CallDeadline,
		&c.Reservation.Budget.TotalTokens, &c.Reservation.Budget.CostMicroyuan,
		&c.Lease.TenantID, &c.Lease.RunID, &c.Lease.WorkerID, &c.Lease.SessionID, &c.Lease.AttemptNo, &c.Lease.FencingToken,
		&c.StepID, &c.StepKind, &c.Kind, &c.ProfileHash, &c.Status, &c.ObservationHash, &c.UsageHash,
		&c.TransportOutcome, &c.BusinessOutcome, &c.Reservation.MeasurementAnomaly, &usage)
	if err != nil {
		return c, err
	}
	c.Reservation.UsageKnown = c.Status == "known"
	if len(usage) > 0 {
		var report agentrun.UsageReport
		if err := json.Unmarshal(usage, &report); err != nil || report.Validate() != nil {
			return c, agentrun.ErrInternal
		}
		c.Reservation.ReportedUsage = &report
	}
	return c, nil
}

func validateLedgerLease(lease agentrun.Lease) error {
	if !agentrun.ValidIdentifier(lease.TenantID) || !agentrun.ValidIdentifier(lease.WorkerID) ||
		!agentrun.ValidUUID(lease.RunID) || !agentrun.ValidUUID(lease.SessionID) || lease.AttemptNo < 1 ||
		lease.AttemptNo > agentrun.MaxSafeInteger || lease.FencingToken < 1 || lease.FencingToken > agentrun.MaxSafeInteger {
		return agentrun.ErrInvalidArgument
	}
	return nil
}

func (s *Store) ledgerExecution(ctx context.Context, tx pgx.Tx, principal string, r agentrun.Run, a agentrun.Authority,
	lease agentrun.Lease, step agentrun.StepIdentity, now time.Time) error {
	if err := s.checkSession(ctx, tx, principal, lease.WorkerID, lease.SessionID, r.TenantID, r.ProfileID, now, true); err != nil {
		return err
	}
	if err := agentrun.CheckExecution(r, a, lease, now); err != nil {
		return err
	}
	return agentrun.CheckStep(r, a, step)
}

func (s *Store) ledgerProfile(ctx context.Context, tx pgx.Tx, r agentrun.Run, executable bool) (agentrun.Profile, error) {
	var p agentrun.Profile
	var body []byte
	var hash string
	if err := tx.QueryRow(ctx, "select profile_hash,definition from agent_profiles where profile_id=$1", r.ProfileID).Scan(&hash, &body); err != nil {
		return p, agentrun.ErrProfileUnavailable
	}
	if json.Unmarshal(body, &p) != nil || p.ID != r.ProfileID || hash != r.ProfileHash || p.Hash != hash || !agentrun.ValidHash(p.Pricing.Hash) {
		return p, agentrun.ErrProfileUnavailable
	}
	if executable {
		registered, ok := s.profiles[r.ProfileID]
		if !ok || !registered.Executable || registered.Hash != p.Hash {
			return p, agentrun.ErrProfileUnavailable
		}
	}
	return p, nil
}

func accountsAvailable(accounts [3]budgetRow, now time.Time) error {
	for _, account := range accounts {
		if now.Before(account.ValidFrom) || !account.ValidUntil.After(now) || account.Account.Frozen {
			return agentrun.ErrBudgetExhausted
		}
	}
	return nil
}

func reserveLedgerAccounts(accounts [3]budgetRow, delta agentrun.Usage, now time.Time) ([3]budgetRow, error) {
	if err := accountsAvailable(accounts, now); err != nil {
		return accounts, err
	}
	updated := accounts
	for i := range updated {
		account, err := agentrun.ReserveAccount(updated[i].Account, delta)
		if err != nil {
			return accounts, err
		}
		updated[i].Account = account
	}
	return updated, nil
}

func saveLedgerAccounts(ctx context.Context, tx pgx.Tx, accounts [3]budgetRow) error {
	for _, account := range accounts {
		if err := saveAccount(ctx, tx, account); err != nil {
			return err
		}
	}
	return nil
}

// BeginTool consumes the shared logical count once. Replaying its ID returns
// the old invocation and never authorizes another logical tool execution.
func (s *Store) BeginTool(ctx context.Context, principal string, req agentrun.BeginToolRequest) (agentrun.BeginToolResponse, error) {
	result := agentrun.BeginToolResponse{ToolInvocationID: req.ToolInvocationID}
	if !agentrun.ValidUUID(req.ToolInvocationID) || validateLedgerLease(req.Lease) != nil || len(agentrun.ToolSequence(req.Step.Kind)) == 0 {
		return result, agentrun.ErrInvalidArgument
	}
	err := s.transact(ctx, func(tx pgx.Tx) error {
		r, a, err := lockRun(ctx, tx, req.Lease.TenantID, req.Lease.RunID)
		if err != nil {
			return err
		}
		accounts, err := loadAccounts(ctx, tx, r.TenantID, r.BusinessRequestID, true)
		if err != nil {
			return err
		}
		prior, priorErr := readTool(ctx, tx, req.ToolInvocationID)
		if priorErr != nil && !errors.Is(priorErr, pgx.ErrNoRows) {
			return priorErr
		}
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.ledgerExecution(ctx, tx, principal, r, a, req.Lease, req.Step, now); err != nil {
			return err
		}
		if priorErr == nil {
			if !toolMatches(prior, req) {
				return agentrun.ErrCallConflict
			}
			return nil
		}
		if a.ActiveCallID != nil {
			return agentrun.ErrCallConflict
		}
		if _, err := s.ledgerProfile(ctx, tx, r, true); err != nil {
			return err
		}
		accounts, err = reserveLedgerAccounts(accounts, agentrun.Usage{LogicalTools: 1}, now)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `insert into tool_invocations
			(invocation_id,tenant_id,run_id,step_id,attempt_no,fencing_token,tool_name,input_hash,created_at)
			values($1,$2,$3,$4,$5,$6,$7,$8,$9)`, req.ToolInvocationID, r.TenantID, r.ID, req.Step.ID,
			req.Lease.AttemptNo, req.Lease.FencingToken, req.Step.Kind, req.Step.InputHash, now)
		if err != nil {
			return err
		}
		if err := saveLedgerAccounts(ctx, tx, accounts); err != nil {
			return err
		}
		// A unique-key insert may have waited behind an unrelated transaction.
		now, err = databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.ledgerExecution(ctx, tx, principal, r, a, req.Lease, req.Step, now); err != nil {
			return err
		}
		if err := accountsAvailable(accounts, now); err != nil {
			return err
		}
		result.NewlyStarted = true
		return nil
	})
	if errors.Is(err, agentrun.ErrConflict) {
		return agentrun.BeginToolResponse{}, agentrun.ErrCallConflict
	}
	return result, err
}

// ReserveCall atomically grants one physical send and all three budget holds.
// Its ID is not a provider idempotency guarantee; duplicate responses are audit.
func (s *Store) ReserveCall(ctx context.Context, principal string, req agentrun.ReserveCallRequest) (agentrun.ReserveCallResponse, error) {
	var result agentrun.ReserveCallResponse
	if validateLedgerLease(req.Lease) != nil || !agentrun.ValidUUID(req.PhysicalCallID) ||
		!agentrun.ValidHash(req.ParameterHash) || !agentrun.ValidHash(req.PriceHash) || agentrun.CallKind(req.Subcall) == "" ||
		(req.ToolInvocationID != "" && !agentrun.ValidUUID(req.ToolInvocationID)) {
		return result, agentrun.ErrInvalidArgument
	}
	err := s.transact(ctx, func(tx pgx.Tx) error {
		r, a, err := lockRun(ctx, tx, req.Lease.TenantID, req.Lease.RunID)
		if err != nil {
			return err
		}
		accounts, err := loadAccounts(ctx, tx, r.TenantID, r.BusinessRequestID, true)
		if err != nil {
			return err
		}
		prior, priorErr := readCall(ctx, tx, r.TenantID, r.ID, req.PhysicalCallID)
		if priorErr != nil && !errors.Is(priorErr, pgx.ErrNoRows) {
			return priorErr
		}
		var tool toolRow
		if req.ToolInvocationID != "" {
			tool, err = readTool(ctx, tx, req.ToolInvocationID)
			if errors.Is(err, pgx.ErrNoRows) {
				return agentrun.ErrCallConflict
			}
			if err != nil {
				return err
			}
		}
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.ledgerExecution(ctx, tx, principal, r, a, req.Lease, req.Step, now); err != nil {
			return err
		}
		profile, err := s.ledgerProfile(ctx, tx, r, false)
		if err != nil {
			return err
		}
		if priorErr == nil {
			if prior.Lease != req.Lease || prior.StepID != req.Step.ID || prior.StepKind != req.Step.Kind || prior.Reservation.ToolInvocationID != req.ToolInvocationID ||
				prior.Reservation.Subcall != req.Subcall || prior.Reservation.ParameterHash != req.ParameterHash ||
				prior.Reservation.PriceHash != req.PriceHash || prior.ProfileHash != req.Step.ProfileHash {
				return agentrun.ErrCallConflict
			}
			if err := restoreCallBudget(&prior, profile); err != nil {
				return err
			}
			result.Reservation = prior.Reservation
			return nil
		}
		if a.ActiveCallID != nil {
			return agentrun.ErrCallConflict
		}
		if _, err := s.ledgerProfile(ctx, tx, r, true); err != nil {
			return err
		}
		if profile.Pricing.Hash != req.PriceHash {
			return agentrun.ErrProfileUnavailable
		}
		if err := checkSubcall(ctx, tx, req, tool); err != nil {
			return err
		}
		budget, err := agentrun.ReservationBudget(profile, req.Subcall)
		if err != nil {
			return err
		}
		accounts, err = reserveLedgerAccounts(accounts, agentrun.ReservationUsage(req.Step.Kind, req.Subcall, budget), now)
		if err != nil {
			return err
		}
		var ordinal int64
		if err := tx.QueryRow(ctx, "select coalesce(max(ordinal),0)+1 from physical_calls where tenant_id=$1 and run_id=$2", r.TenantID, r.ID).Scan(&ordinal); err != nil {
			return err
		}
		if ordinal > 44 {
			return agentrun.ErrBudgetExhausted
		}
		limit := r.RunDeadline
		for _, account := range accounts {
			limit = earliest(limit, account.ValidUntil)
		}
		limit = earliest(limit, *r.AttemptDeadline)
		timeout := 10 * time.Second
		if req.Subcall == agentrun.SubcallChat {
			timeout = 60 * time.Second
		}
		callDeadline := earliest(now.Add(timeout), limit)
		dispatchExpiry := earliest(*r.LeaseUntil, callDeadline)
		reservation := agentrun.CallReservation{PhysicalCallID: req.PhysicalCallID, ToolInvocationID: req.ToolInvocationID,
			Subcall: req.Subcall, ParameterHash: req.ParameterHash, PriceHash: req.PriceHash, Ordinal: ordinal,
			ReservedAt: now, DispatchExpiresAt: dispatchExpiry, CallDeadline: callDeadline, Budget: budget}
		_, err = tx.Exec(ctx, `insert into physical_calls
			(physical_call_id,tenant_id,run_id,step_id,attempt_no,worker_id,session_id,fencing_token,tool_invocation_id,
			kind,subcall,ordinal,input_hash,profile_hash,price_hash,reserved_tokens,reserved_cost_microyuan,status,
			reserved_at,dispatch_expires_at,call_deadline,step_kind)
			values($1,$2,$3,$4,$5,$6,$7,$8,nullif($9,'')::uuid,$10,$11,$12,$13,$14,$15,$16,$17,'reserved',$18,$19,$20,$21)`,
			req.PhysicalCallID, r.TenantID, r.ID, req.Step.ID, req.Lease.AttemptNo, req.Lease.WorkerID, req.Lease.SessionID,
			req.Lease.FencingToken, req.ToolInvocationID, agentrun.CallKind(req.Subcall), req.Subcall, ordinal,
			req.ParameterHash, req.Step.ProfileHash, req.PriceHash, budget.TotalTokens, budget.CostMicroyuan, now, dispatchExpiry, callDeadline, req.Step.Kind)
		if err != nil {
			return err
		}
		if err := saveLedgerAccounts(ctx, tx, accounts); err != nil {
			return err
		}
		a.ActiveCallID, r.UpdatedAt = &req.PhysicalCallID, now
		if err := saveRun(ctx, tx, &r, &a); err != nil {
			return err
		}
		now, err = databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.ledgerExecution(ctx, tx, principal, r, a, req.Lease, req.Step, now); err != nil {
			return err
		}
		if err := accountsAvailable(accounts, now); err != nil {
			return err
		}
		if !reservation.DispatchExpiresAt.After(now) {
			return agentrun.ErrStaleLease
		}
		result.Reservation, result.NewlyReserved = reservation, true
		return nil
	})
	if errors.Is(err, agentrun.ErrConflict) {
		return agentrun.ReserveCallResponse{}, agentrun.ErrCallConflict
	}
	return result, err
}

func checkSubcall(ctx context.Context, tx pgx.Tx, req agentrun.ReserveCallRequest, tool toolRow) error {
	if req.Subcall == agentrun.SubcallChat {
		if req.ToolInvocationID != "" || (req.Step.Kind != "model_proposal" && req.Step.Kind != "protocol_correction") {
			return agentrun.ErrCallConflict
		}
		return nil
	}
	if req.ToolInvocationID == "" || !toolMatches(tool, agentrun.BeginToolRequest{Lease: req.Lease, Step: req.Step, ToolInvocationID: req.ToolInvocationID}) {
		return agentrun.ErrCallConflict
	}
	sequence := agentrun.ToolSequence(req.Step.Kind)
	rows, err := tx.Query(ctx, `select subcall,transport_outcome,business_outcome,observation_hash
		from physical_calls where tenant_id=$1 and run_id=$2 and tool_invocation_id=$3 order by ordinal`,
		req.Lease.TenantID, req.Lease.RunID, req.ToolInvocationID)
	if err != nil {
		return err
	}
	defer rows.Close()
	position := 0
	for rows.Next() {
		var subcall agentrun.Subcall
		var transport, business, observation *string
		if err := rows.Scan(&subcall, &transport, &business, &observation); err != nil {
			return err
		}
		if position >= len(sequence) || subcall != sequence[position] || transport == nil || *transport != "response" ||
			business == nil || *business != "accepted" || observation == nil {
			return agentrun.ErrCallConflict
		}
		position++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if position >= len(sequence) || req.Subcall != sequence[position] {
		return agentrun.ErrCallConflict
	}
	return nil
}

func restoreCallBudget(c *callRow, p agentrun.Profile) error {
	if p.Hash != c.ProfileHash || p.Pricing.Hash != c.Reservation.PriceHash {
		return agentrun.ErrProfileUnavailable
	}
	budget, err := agentrun.ReservationBudget(p, c.Reservation.Subcall)
	if err != nil {
		return err
	}
	if budget.TotalTokens != c.Reservation.Budget.TotalTokens || budget.CostMicroyuan != c.Reservation.Budget.CostMicroyuan {
		return agentrun.ErrInternal
	}
	c.Reservation.Budget = budget
	return nil
}

// ObserveCall closes only the current attempt's active call. A complete valid
// usage report settles even when business output was rejected by its validator.
func (s *Store) ObserveCall(ctx context.Context, principal string, req agentrun.ObserveCallRequest) (agentrun.CallReservation, error) {
	var result agentrun.CallReservation
	if validateLedgerLease(req.Lease) != nil || req.Validate() != nil {
		return result, agentrun.ErrInvalidArgument
	}
	err := s.transact(ctx, func(tx pgx.Tx) error {
		r, a, err := lockRun(ctx, tx, req.Lease.TenantID, req.Lease.RunID)
		if err != nil {
			return err
		}
		accounts, err := loadAccounts(ctx, tx, r.TenantID, r.BusinessRequestID, true)
		if err != nil {
			return err
		}
		call, err := readCall(ctx, tx, r.TenantID, r.ID, req.PhysicalCallID)
		if err != nil {
			return err
		}
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.ledgerExecution(ctx, tx, principal, r, a, req.Lease, req.Step, now); err != nil {
			return err
		}
		if call.Lease != req.Lease || call.StepID != req.Step.ID || call.StepKind != req.Step.Kind || call.ProfileHash != req.Step.ProfileHash {
			return agentrun.ErrCallConflict
		}
		profile, err := s.ledgerProfile(ctx, tx, r, false)
		if err != nil {
			return err
		}
		if err := restoreCallBudget(&call, profile); err != nil {
			return err
		}
		hash := req.Hash()
		if call.ObservationHash != nil {
			if *call.ObservationHash != hash {
				return agentrun.ErrCallConflict
			}
			result = call.Reservation
			return nil
		}
		if a.ActiveCallID == nil || *a.ActiveCallID != req.PhysicalCallID {
			return agentrun.ErrCallConflict
		}
		if req.Usage != nil {
			if _, err := settleLedgerUsage(ctx, tx, &call, profile, accounts, *req.Usage, now); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `update physical_calls set status=case when status='reserved' then 'unknown' else status end,
			transport_outcome=$4,business_outcome=$5,observation_hash=$6,observed_at=$7
			where tenant_id=$1 and run_id=$2 and physical_call_id=$3`, r.TenantID, r.ID, req.PhysicalCallID,
			req.TransportOutcome, req.BusinessOutcome, hash, now)
		if err != nil {
			return err
		}
		a.ActiveCallID, r.UpdatedAt = nil, now
		if err := saveRun(ctx, tx, &r, &a); err != nil {
			return err
		}
		result = call.Reservation
		return nil
	})
	return result, err
}

// SettleUsage authenticates the original reserved principal/session, permits an
// expired lease, and changes only call/account metering. It never saves the Run.
func (s *Store) SettleUsage(ctx context.Context, principal string, req agentrun.SettleUsageRequest) (agentrun.SettleUsageResponse, error) {
	var result agentrun.SettleUsageResponse
	if validateLedgerLease(req.Lease) != nil || !agentrun.ValidUUID(req.PhysicalCallID) || req.Usage.Validate() != nil {
		return result, agentrun.ErrInvalidArgument
	}
	err := s.transact(ctx, func(tx pgx.Tx) error {
		r, _, err := lockRun(ctx, tx, req.Lease.TenantID, req.Lease.RunID)
		if err != nil {
			return err
		}
		accounts, err := loadAccounts(ctx, tx, r.TenantID, r.BusinessRequestID, true)
		if err != nil {
			return err
		}
		call, err := readCall(ctx, tx, r.TenantID, r.ID, req.PhysicalCallID)
		if err != nil {
			return err
		}
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if call.Lease != req.Lease {
			return agentrun.ErrCallConflict
		}
		if err := s.checkSession(ctx, tx, principal, call.Lease.WorkerID, call.Lease.SessionID, r.TenantID, r.ProfileID, now, false); err != nil {
			return err
		}
		profile, err := s.ledgerProfile(ctx, tx, r, false)
		if err != nil {
			return err
		}
		if err := restoreCallBudget(&call, profile); err != nil {
			return err
		}
		first, err := settleLedgerUsage(ctx, tx, &call, profile, accounts, req.Usage, now)
		if err != nil {
			return err
		}
		result.Reservation, result.NewlySettled = call.Reservation, first
		return nil
	})
	return result, err
}

func settleLedgerUsage(ctx context.Context, tx pgx.Tx, call *callRow, profile agentrun.Profile,
	accounts [3]budgetRow, usage agentrun.UsageReport, now time.Time) (bool, error) {
	if !call.Reservation.ReservedAt.Add(agentrun.UsageSettlementWindow).After(now) {
		return false, agentrun.ErrCallSettlementExpired
	}
	if call.UsageHash != nil {
		if *call.UsageHash != usage.UsageHash {
			return false, agentrun.ErrCallConflict
		}
		return false, nil
	}
	if call.Kind != "chat" && call.Kind != "query_embedding" {
		return false, agentrun.ErrInvalidArgument
	}
	body, err := agentrun.UsageJSON(usage)
	if err != nil {
		return false, err
	}
	budget := call.Reservation.Budget
	anomaly := usage.InputTokens > budget.InputTokens || usage.OutputTokens > budget.OutputTokens ||
		usage.InputTokens > agentrun.MaxSafeInteger-usage.OutputTokens ||
		(call.Kind == "query_embedding" && usage.CachedInputTokens != 0)
	var cost int64
	if call.Kind == "chat" {
		cost, err = agentrun.UsageCost(profile.Pricing, usage)
		if errors.Is(err, agentrun.ErrBudgetExhausted) {
			anomaly = true
		} else if err != nil {
			return false, err
		}
	}
	anomaly = anomaly || cost > budget.CostMicroyuan
	if anomaly {
		// Preserve the original hold and the full bounded report. Freezing is a
		// persisted safety fact, not an error that would roll this transaction back.
		for i := range accounts {
			accounts[i].Account.Frozen = true
		}
		if err := saveLedgerAccounts(ctx, tx, accounts); err != nil {
			return false, err
		}
		_, err = tx.Exec(ctx, `update physical_calls set status='unknown',measurement_anomaly=true,
			usage_hash=$4,usage=$5 where tenant_id=$1 and run_id=$2 and physical_call_id=$3`,
			call.Lease.TenantID, call.Lease.RunID, call.Reservation.PhysicalCallID, usage.UsageHash, body)
		if err != nil {
			return false, err
		}
		call.Status, call.UsageHash = "unknown", &usage.UsageHash
		call.Reservation.MeasurementAnomaly, call.Reservation.ReportedUsage = true, &usage
		return false, nil
	}
	tokens := usage.InputTokens + usage.OutputTokens
	for i := range accounts {
		settled, err := agentrun.SettleAccount(accounts[i].Account, budget.TotalTokens, budget.CostMicroyuan, tokens, cost)
		if err != nil {
			return false, err
		}
		accounts[i].Account = settled
	}
	if err := saveLedgerAccounts(ctx, tx, accounts); err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `update physical_calls set status='known',usage_hash=$4,usage=$5,
		known_tokens=$6,known_cost_microyuan=$7,settled_at=$8
		where tenant_id=$1 and run_id=$2 and physical_call_id=$3`, call.Lease.TenantID, call.Lease.RunID,
		call.Reservation.PhysicalCallID, usage.UsageHash, body, tokens, cost, now)
	if err != nil {
		return false, err
	}
	call.Status, call.UsageHash = "known", &usage.UsageHash
	call.Reservation.UsageKnown, call.Reservation.ReportedUsage = true, &usage
	return true, nil
}

func earliest(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
