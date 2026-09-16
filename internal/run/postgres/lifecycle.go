package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

type executionSlot struct {
	Kind, ID       string
	Capacity, Used int
}

type attemptResources struct {
	Authority agentrun.Authority
	AttemptNo int64
	Slots     [3]executionSlot
}

func lockSlots(ctx context.Context, tx pgx.Tx, r agentrun.Run, worker string, capacities *[3]int) ([3]executionSlot, error) {
	rows := [3]executionSlot{{Kind: "worker", ID: worker}, {Kind: "tenant", ID: r.TenantID}, {Kind: "profile", ID: r.ProfileID}}
	for i := range rows {
		if capacities != nil {
			if _, err := tx.Exec(ctx, `insert into execution_slots(resource_kind,resource_id,capacity)
				values($1,$2,$3) on conflict(resource_kind,resource_id) do nothing`, rows[i].Kind, rows[i].ID, capacities[i]); err != nil {
				return rows, err
			}
		}
		if err := tx.QueryRow(ctx, `select capacity,used from execution_slots
			where resource_kind=$1 and resource_id=$2 for update`, rows[i].Kind, rows[i].ID).Scan(&rows[i].Capacity, &rows[i].Used); err != nil {
			return rows, err
		}
		if capacities != nil && rows[i].Capacity != capacities[i] {
			return rows, agentrun.ErrConflict
		}
	}
	return rows, nil
}

func saveSlots(ctx context.Context, tx pgx.Tx, slots [3]executionSlot, delta int) error {
	for _, slot := range slots {
		used := slot.Used + delta
		if used < 0 || used > slot.Capacity {
			return agentrun.ErrInternal
		}
		if _, err := tx.Exec(ctx, `update execution_slots set used=$3 where resource_kind=$1 and resource_id=$2`, slot.Kind, slot.ID, used); err != nil {
			return err
		}
	}
	return nil
}

// lockAttemptResources follows Run -> call -> child -> worker/tenant/profile
// slots. Closing changes no budget, so no account lock or refund is needed.
// Callers obtain fresh database time only after this function returns.
func lockAttemptResources(ctx context.Context, tx pgx.Tx, r agentrun.Run, a agentrun.Authority) (attemptResources, error) {
	resources := attemptResources{Authority: a, AttemptNo: r.AttemptNo}
	if a.WorkerID == "" || a.SessionID == "" || r.AttemptNo < 1 {
		return resources, agentrun.ErrStaleLease
	}
	if a.ActiveCallID != nil {
		var attempt int64
		if err := tx.QueryRow(ctx, `select attempt_no from physical_calls
			where tenant_id=$1 and run_id=$2 and physical_call_id=$3 for update`, r.TenantID, r.ID, *a.ActiveCallID).Scan(&attempt); err != nil {
			return resources, err
		}
		if attempt != r.AttemptNo {
			return resources, agentrun.ErrInternal
		}
	}
	var worker, session string
	var token int64
	var finished *time.Time
	if err := tx.QueryRow(ctx, `select worker_id,session_id,fencing_token,finished_at from run_attempts
		where tenant_id=$1 and run_id=$2 and attempt_no=$3 for update`, r.TenantID, r.ID, r.AttemptNo).Scan(&worker, &session, &token, &finished); err != nil {
		return resources, err
	}
	if worker != a.WorkerID || session != a.SessionID || token != a.FencingToken || finished != nil {
		return resources, agentrun.ErrStaleLease
	}
	slots, err := lockSlots(ctx, tx, r, a.WorkerID, nil)
	resources.Slots = slots
	return resources, err
}

// closeAttemptRecords uses the already locked original identity, preserving
// unknown holds even if a domain transition has already cleared current owner.
// It neither saves the Run nor appends its event; both remain in the caller's tx.
func closeAttemptRecords(ctx context.Context, tx pgx.Tx, r *agentrun.Run, a *agentrun.Authority,
	resources attemptResources, outcome string, now time.Time) error {
	if r.AttemptNo != resources.AttemptNo || a.FencingToken != resources.Authority.FencingToken {
		return agentrun.ErrStaleLease
	}
	if resources.Authority.ActiveCallID != nil {
		if _, err := tx.Exec(ctx, `update physical_calls set status=case when status='reserved' then 'unknown' else status end
			where tenant_id=$1 and run_id=$2 and physical_call_id=$3 and attempt_no=$4`, r.TenantID, r.ID,
			*resources.Authority.ActiveCallID, resources.AttemptNo); err != nil {
			return err
		}
	}
	var code *string
	if r.Error != nil {
		code = &r.Error.Code
	}
	tag, err := tx.Exec(ctx, `update run_attempts set finished_at=$4,outcome=$5,error_code=$6
		where tenant_id=$1 and run_id=$2 and attempt_no=$3 and finished_at is null`, r.TenantID, r.ID, resources.AttemptNo, now, outcome, code)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return agentrun.ErrStaleLease
	}
	if err := saveSlots(ctx, tx, resources.Slots, -1); err != nil {
		return err
	}
	a.WorkerID, a.SessionID, a.ActiveCallID = "", "", nil
	r.LeaseUntil, r.AttemptDeadline = nil, nil
	return nil
}

// Claim locks one eligible Run and installs attempt, lease, event and capacity
// atomically. Slots are derived counters, never an alternative work queue.
func (s *Store) Claim(ctx context.Context, principal, sessionID string) (*agentrun.ClaimedRun, error) {
	worker, ok := s.workers[principal]
	if !ok {
		return nil, agentrun.ErrForbidden
	}
	if !validUUID(sessionID) {
		return nil, agentrun.ErrInvalidArgument
	}
	profiles := make([]string, 0, len(worker.ProfileIDs))
	for _, id := range worker.ProfileIDs {
		if profile, ok := s.profiles[id]; ok && profile.Executable {
			profiles = append(profiles, id)
		}
	}
	var result *agentrun.ClaimedRun
	err := s.transact(ctx, func(tx pgx.Tx) error {
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.checkSession(ctx, tx, principal, principal, sessionID, "", "", now, true); err != nil {
			return err
		}
		if len(profiles) == 0 {
			return nil
		}
		r, a, err := readRun(tx.QueryRow(ctx, "select "+runColumns+` from runs
			where state='ready' and tenant_id=any($1) and profile_id=any($2)
			and not exists(select 1 from execution_slots where resource_kind='worker' and resource_id=$3 and used>=capacity)
			and not exists(select 1 from execution_slots where resource_kind='tenant' and resource_id=runs.tenant_id and used>=capacity)
			and not exists(select 1 from execution_slots where resource_kind='profile' and resource_id=runs.profile_id and used>=capacity)
			order by created_at,run_id limit 1 for update skip locked`, worker.Tenants, profiles, principal))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		// The Claim response carries a consistent view of all three budgets.
		if _, err := loadAccounts(ctx, tx, r.TenantID, r.BusinessRequestID, true); err != nil {
			return err
		}
		capacities := [3]int{worker.Capacity, s.tenantCapacity, s.profileCapacity}
		slots, err := lockSlots(ctx, tx, r, principal, &capacities)
		if err != nil {
			return err
		}
		now, err = databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.checkSession(ctx, tx, principal, principal, sessionID, r.TenantID, r.ProfileID, now, true); err != nil {
			return err
		}
		if _, err := s.ledgerProfile(ctx, tx, r, true); err != nil {
			return err
		}
		if !r.RunDeadline.After(now) {
			if err := agentrun.ExpireWaiting(&r, now); err != nil {
				return err
			}
			if err := appendEvent(ctx, tx, &r, &a, "deadline_exceeded", now); err != nil {
				return err
			}
			return saveRun(ctx, tx, &r, &a)
		}
		for _, slot := range slots {
			if slot.Used >= slot.Capacity {
				return nil
			}
		}
		lease, err := agentrun.ClaimExecution(&r, &a, principal, sessionID, now)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `insert into run_attempts
			(tenant_id,run_id,attempt_no,worker_id,session_id,fencing_token,started_at,deadline)
			values($1,$2,$3,$4,$5,$6,$7,$8)`, r.TenantID, r.ID, r.AttemptNo, principal, sessionID, a.FencingToken, now, r.AttemptDeadline)
		if err != nil {
			return err
		}
		if err := saveSlots(ctx, tx, slots, 1); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, &r, &a, "claimed", now); err != nil {
			return err
		}
		if err := saveRun(ctx, tx, &r, &a); err != nil {
			return err
		}
		checkpoint, err := loadCheckpoint(ctx, tx, r, a)
		if err != nil {
			return err
		}
		result = &agentrun.ClaimedRun{Lease: lease, Checkpoint: checkpoint}
		return nil
	})
	return result, err
}

func sameAttempt(r agentrun.Run, a agentrun.Authority, lease agentrun.Lease) bool {
	return r.TenantID == lease.TenantID && r.ID == lease.RunID && r.AttemptNo == lease.AttemptNo &&
		a.WorkerID == lease.WorkerID && a.SessionID == lease.SessionID && a.FencingToken == lease.FencingToken &&
		a.WorkerID != "" && a.SessionID != ""
}

func lockLifecycleSession(ctx context.Context, tx pgx.Tx, principal, sessionID string) (agentrun.Session, error) {
	if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock(hashtextextended($1,18))", principal); err != nil {
		return agentrun.Session{}, err
	}
	return readSession(tx.QueryRow(ctx, "select "+sessionColumns+" from worker_sessions where worker_id=$1 and session_id=$2 for update", principal, sessionID))
}

// HeartbeatExecution serializes session liveness with registration, then uses a
// fresh database clock for both liveness and Run authority. Stopping never renews.
func (s *Store) HeartbeatExecution(ctx context.Context, principal string, lease agentrun.Lease) (agentrun.HeartbeatResult, error) {
	var result agentrun.HeartbeatResult
	if validateLedgerLease(lease) != nil {
		return result, agentrun.ErrInvalidArgument
	}
	if principal != lease.WorkerID {
		return result, agentrun.ErrForbidden
	}
	err := s.transact(ctx, func(tx pgx.Tx) error {
		r, a, err := lockRun(ctx, tx, lease.TenantID, lease.RunID)
		if err != nil {
			return err
		}
		if !sameAttempt(r, a, lease) || (r.State != agentrun.Running && r.State != agentrun.Stopping) {
			return agentrun.ErrStaleLease
		}
		session, err := lockLifecycleSession(ctx, tx, principal, lease.SessionID)
		if err != nil {
			return err
		}
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.checkSession(ctx, tx, principal, lease.WorkerID, lease.SessionID, r.TenantID, r.ProfileID, now, false); err != nil {
			return err
		}
		previous := r.State
		if r.State == agentrun.Running {
			if !r.RunDeadline.After(now) {
				if err := agentrun.RequestStop(&r, agentrun.StopRunDeadline, now); err != nil {
					return err
				}
			} else if r.AttemptDeadline != nil && !r.AttemptDeadline.After(now) {
				if err := agentrun.RequestStop(&r, agentrun.StopAttemptTimeout, now); err != nil {
					return err
				}
			}
		}
		if r.State == agentrun.Stopping {
			if r.StopReason == nil || r.LeaseUntil == nil {
				return agentrun.ErrInternal
			}
			result = agentrun.HeartbeatResult{LeaseUntil: *r.LeaseUntil, StopReason: *r.StopReason}
			if r.CancelRequestedAt != nil {
				result.StopReason = agentrun.StopCancel
			}
			if previous != r.State {
				if err := appendEvent(ctx, tx, &r, &a, "stop_requested", now); err != nil {
					return err
				}
				if err := saveRun(ctx, tx, &r, &a); err != nil {
					return err
				}
			}
		} else {
			if err := s.checkSession(ctx, tx, principal, lease.WorkerID, lease.SessionID, r.TenantID, r.ProfileID, now, true); err != nil {
				return err
			}
			if err := agentrun.Heartbeat(&r, a, lease, now); err != nil {
				return err
			}
			if err := saveRun(ctx, tx, &r, &a); err != nil {
				return err
			}
			result = agentrun.HeartbeatResult{Continue: true, LeaseUntil: *r.LeaseUntil}
		}
		// Expired stopping sessions may receive stop control but are never revived.
		result.SessionExpiresAt = session.ExpiresAt
		if session.ExpiresAt.After(now) {
			_, err = tx.Exec(ctx, "update worker_sessions set seen_at=$3,expires_at=$4 where worker_id=$1 and session_id=$2", principal, lease.SessionID, now, now.Add(sessionTTL))
			result.SessionExpiresAt = now.Add(sessionTTL)
		}
		return err
	})
	return result, err
}

// FailExecution accepts only a current fenced report at its exact step. Its
// stable reason determines recovery; callers cannot supply a retryable flag.
func (s *Store) FailExecution(ctx context.Context, principal string, lease agentrun.Lease, step agentrun.StepIdentity, reason string) (agentrun.Run, error) {
	var result agentrun.Run
	if validateLedgerLease(lease) != nil {
		return result, agentrun.ErrInvalidArgument
	}
	err := s.transact(ctx, func(tx pgx.Tx) error {
		r, a, err := lockRun(ctx, tx, lease.TenantID, lease.RunID)
		if err != nil {
			return err
		}
		resources, err := lockAttemptResources(ctx, tx, r, a)
		if err != nil {
			return err
		}
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.ledgerExecution(ctx, tx, principal, r, a, lease, step, now); err != nil {
			return err
		}
		outcome, err := agentrun.CloseAttempt(&r, &a, reason, now)
		if err != nil {
			return err
		}
		if err := closeAttemptRecords(ctx, tx, &r, &a, resources, outcome, now); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, &r, &a, "attempt_closed", now); err != nil {
			return err
		}
		if err := saveRun(ctx, tx, &r, &a); err != nil {
			return err
		}
		result, err = getView(ctx, tx, r.TenantID, r.ID)
		return err
	})
	return result, err
}

// AcknowledgeStop may use the old expired lease only while it is still the
// matching stopping attempt. Repeated closes use read-only accepted-result APIs.
func (s *Store) AcknowledgeStop(ctx context.Context, principal string, lease agentrun.Lease) (agentrun.StopResult, error) {
	var result agentrun.StopResult
	if validateLedgerLease(lease) != nil {
		return result, agentrun.ErrInvalidArgument
	}
	err := s.transact(ctx, func(tx pgx.Tx) error {
		r, a, err := lockRun(ctx, tx, lease.TenantID, lease.RunID)
		if err != nil {
			return err
		}
		if r.State != agentrun.Stopping || !sameAttempt(r, a, lease) {
			return agentrun.ErrStaleLease
		}
		resources, err := lockAttemptResources(ctx, tx, r, a)
		if err != nil {
			return err
		}
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.checkSession(ctx, tx, principal, lease.WorkerID, lease.SessionID, r.TenantID, r.ProfileID, now, false); err != nil {
			return err
		}
		if r.StopReason == nil {
			return agentrun.ErrInternal
		}
		outcome, err := agentrun.CloseAttempt(&r, &a, *r.StopReason, now)
		if err != nil {
			return err
		}
		if err := closeAttemptRecords(ctx, tx, &r, &a, resources, outcome, now); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, &r, &a, "attempt_closed", now); err != nil {
			return err
		}
		if err := saveRun(ctx, tx, &r, &a); err != nil {
			return err
		}
		result.Run, err = getView(ctx, tx, r.TenantID, r.ID)
		result.AttemptOutcome = outcome
		return err
	})
	return result, err
}

// Cancel resolves an accepted command before examining new state. A new key
// cannot cancel an already terminal Run, while its original accepted key replays.
func (s *Store) Cancel(ctx context.Context, tenant, id, key string) (agentrun.CancelResponse, error) {
	var result agentrun.CancelResponse
	if !agentrun.ValidIdentifier(tenant) || !validUUID(id) || !agentrun.ValidIdentifier(key) {
		return result, agentrun.ErrInvalidArgument
	}
	hash := agentrun.Fingerprint("jobforge.run.cancel.v1", "1", id)
	err := s.transact(ctx, func(tx pgx.Tx) error {
		r, a, err := lockRun(ctx, tx, tenant, id)
		if err != nil {
			return err
		}
		prior, err := findOperation(ctx, tx, tenant, "cancel", id, key)
		if err == nil {
			if prior.RequestHash != hash || prior.ResultRunID != id {
				return agentrun.ErrConflict
			}
			result.OperationID, result.Reused = prior.ID, true
			result.Run, err = getView(ctx, tx, tenant, id)
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		alreadyRequested := r.CancelRequestedAt != nil
		if err := agentrun.RequestCancel(&r, now); err != nil {
			return err
		}
		if !alreadyRequested {
			if err := appendEvent(ctx, tx, &r, &a, "cancel_requested", now); err != nil {
				return err
			}
			if err := saveRun(ctx, tx, &r, &a); err != nil {
				return err
			}
		}
		op := agentrun.Operation{ID: uuid.NewString(), TenantID: tenant, Kind: "cancel", SourceRunID: id, Key: key, RequestHash: hash, ResultRunID: id, CreatedAt: now}
		if err := insertOperation(ctx, tx, op); err != nil {
			return err
		}
		result.OperationID = op.ID
		result.Run, err = getView(ctx, tx, tenant, id)
		return err
	})
	return result, err
}

// Sweep processes at most one hundred candidates, each in its own transaction.
// Stale selections are rechecked under the Run lock using a fresh DB clock.
func (s *Store) Sweep(ctx context.Context, limit int) (int, error) {
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 100 {
		return 0, agentrun.ErrInvalidArgument
	}
	rows, err := s.pool.Query(ctx, `select tenant_id,run_id from runs where
		(state='running' and (lease_until<=clock_timestamp() or attempt_deadline<=clock_timestamp() or run_deadline<=clock_timestamp()))
		or (state='stopping' and lease_until<=clock_timestamp())
		or (state in ('ready','retry_wait','awaiting_approval') and run_deadline<=clock_timestamp())
		or (state='retry_wait' and next_attempt_at<=clock_timestamp())
		or (state='awaiting_approval' and permission_expires_at<=clock_timestamp())
		order by updated_at,run_id limit $1`, limit)
	if err != nil {
		return 0, dbError(err)
	}
	var candidates [][2]string
	for rows.Next() {
		var candidate [2]string
		if err := rows.Scan(&candidate[0], &candidate[1]); err != nil {
			rows.Close()
			return 0, dbError(err)
		}
		candidates = append(candidates, candidate)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, dbError(err)
	}
	changed := 0
	for _, candidate := range candidates {
		updated, err := s.sweepRun(ctx, candidate[0], candidate[1])
		if err != nil {
			return changed, err
		}
		if updated {
			changed++
		}
	}
	return changed, nil
}

func (s *Store) sweepRun(ctx context.Context, tenant, id string) (bool, error) {
	changed := false
	err := s.transact(ctx, func(tx pgx.Tx) error {
		r, a, err := lockRun(ctx, tx, tenant, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if r.State.Terminal() {
			return nil
		}
		before := r.State
		if r.State == agentrun.Running || r.State == agentrun.Stopping {
			resources, err := lockAttemptResources(ctx, tx, r, a)
			if err != nil {
				return err
			}
			now, err := databaseTime(ctx, tx)
			if err != nil {
				return err
			}
			if r.State == agentrun.Running {
				if !r.RunDeadline.After(now) {
					if err := agentrun.RequestStop(&r, agentrun.StopRunDeadline, now); err != nil {
						return err
					}
				} else if r.AttemptDeadline != nil && !r.AttemptDeadline.After(now) {
					if err := agentrun.RequestStop(&r, agentrun.StopAttemptTimeout, now); err != nil {
						return err
					}
				}
			}
			if r.LeaseUntil == nil {
				return agentrun.ErrInternal
			}
			if r.LeaseUntil.After(now) {
				if r.State != before {
					if err := appendEvent(ctx, tx, &r, &a, "stop_requested", now); err != nil {
						return err
					}
					changed = true
					return saveRun(ctx, tx, &r, &a)
				}
				return nil
			}
			reason := agentrun.FailureLeaseExpired
			if r.State == agentrun.Stopping {
				if r.StopReason == nil {
					return agentrun.ErrInternal
				}
				reason = *r.StopReason
			}
			outcome, err := agentrun.CloseAttempt(&r, &a, reason, now)
			if err != nil {
				return err
			}
			if err := closeAttemptRecords(ctx, tx, &r, &a, resources, outcome, now); err != nil {
				return err
			}
			if err := appendEvent(ctx, tx, &r, &a, "attempt_closed", now); err != nil {
				return err
			}
			changed = true
			return saveRun(ctx, tx, &r, &a)
		}
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := agentrun.ExpireWaiting(&r, now); err != nil {
			return err
		}
		if before != r.State {
			if err := appendEvent(ctx, tx, &r, &a, "state_changed", now); err != nil {
				return err
			}
			changed = true
			return saveRun(ctx, tx, &r, &a)
		}
		return nil
	})
	return changed, err
}
