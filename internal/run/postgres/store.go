// Package postgres implements short, fenced transactions for the Agent Run
// domain. State decisions remain in package run; network calls never enter a
// database transaction.
package postgres

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

// Store owns the control pool, not the separate business database.
type Store struct {
	pool            *pgxpool.Pool
	profiles        map[string]agentrun.Profile
	workers         map[string]agentrun.WorkerConfig
	tenantCapacity  int
	profileCapacity int
}

// Options contains trusted deployment configuration, never request payloads.
type Options struct {
	Profiles        []agentrun.Profile
	Workers         []agentrun.WorkerConfig
	TenantCapacity  int
	ProfileCapacity int
}

// New constructs a store with explicit bounded capacities.
func New(pool *pgxpool.Pool, options Options) (*Store, error) {
	if pool == nil {
		return nil, agentrun.ErrInvalidArgument
	}
	if options.TenantCapacity == 0 {
		options.TenantCapacity = 1
	}
	if options.ProfileCapacity == 0 {
		options.ProfileCapacity = 2
	}
	if options.TenantCapacity < 1 || options.TenantCapacity > 100 || options.ProfileCapacity < 1 || options.ProfileCapacity > 100 {
		return nil, agentrun.ErrInvalidArgument
	}
	s := &Store{pool: pool, profiles: make(map[string]agentrun.Profile), workers: make(map[string]agentrun.WorkerConfig),
		tenantCapacity: options.TenantCapacity, profileCapacity: options.ProfileCapacity}
	for _, p := range options.Profiles {
		if err := p.ValidateAuditPolicy(); err != nil {
			return nil, err
		}
		_, hashErr := hex.DecodeString(p.Hash)
		if !agentrun.ValidIdentifier(p.ID) || len(p.Hash) != 64 || hashErr != nil || p.Hash != strings.ToLower(p.Hash) ||
			p.MaxInputTokens < 1 || p.MaxInputTokens > agentrun.MaxSafeInteger || p.MaxOutputTokens < 1 || p.MaxOutputTokens > 1024 ||
			p.FamilyTokenLimit < 0 || p.FamilyTokenLimit > agentrun.MaxSafeInteger ||
			p.FamilyCostMicroyuan < 0 || p.FamilyCostMicroyuan > agentrun.MaxSafeInteger || !json.Valid(p.Definition) {
			return nil, agentrun.ErrInvalidArgument
		}
		if _, exists := s.profiles[p.ID]; exists {
			return nil, agentrun.ErrConflict
		}
		p.Definition = slices.Clone(p.Definition)
		s.profiles[p.ID] = p
	}
	for _, w := range options.Workers {
		if !agentrun.ValidIdentifier(w.ID) || w.Capacity < 1 || w.Capacity > 100 || len(w.Tenants) == 0 {
			return nil, agentrun.ErrInvalidArgument
		}
		if _, exists := s.workers[w.ID]; exists {
			return nil, agentrun.ErrConflict
		}
		for _, tenant := range w.Tenants {
			if !agentrun.ValidIdentifier(tenant) {
				return nil, agentrun.ErrInvalidArgument
			}
		}
		for _, profile := range w.ProfileIDs {
			if _, ok := s.profiles[profile]; !ok {
				return nil, agentrun.ErrProfileUnavailable
			}
		}
		w.Tenants, w.ProfileIDs = slices.Clone(w.Tenants), slices.Clone(w.ProfileIDs)
		s.workers[w.ID] = w
	}
	return s, nil
}

func dbError(err error) error {
	if err == nil {
		return nil
	}
	var code agentrun.ErrorCode
	if errors.As(err, &code) {
		return code
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return agentrun.ErrNotFound
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return agentrun.ErrConflict
	}
	return agentrun.ErrDependencyUnavailable
}

// transact never retries a callback implicitly: callers must resolve uncertain
// commits through the persisted operation/call/step identity.
func (s *Store) transact(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return dbError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return dbError(err)
	}
	return dbError(tx.Commit(ctx))
}

func databaseTime(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var now time.Time
	err := tx.QueryRow(ctx, "select clock_timestamp()").Scan(&now)
	return now, err
}

func placeholder(position int) string { return "$" + strconv.Itoa(position) }

const runColumns = `run_id, tenant_id, business_request_id, business_request_key,
	ticket_id, retry_of_run_id, profile_id, profile_hash, budget_batch_id,
	snapshot_id, snapshot_hash, version_vector, state, outcome, error_code, error_message,
	attempt_no, recovery_count, cursor_version, run_timeout_seconds, run_deadline,
	attempt_deadline, lease_until, next_attempt_at, permission_expires_at, proposal_ref,
	stop_reason, cancel_requested_at, created_at, updated_at,
	coalesce(worker_id, ''), coalesce(session_id::text, ''), fencing_token, active_call_id,
	checkpoint_bytes, event_sequence, next_step_id, next_step_kind, next_input_hash`

func readRun(row pgx.Row) (agentrun.Run, agentrun.Authority, error) {
	var r agentrun.Run
	var a agentrun.Authority
	var errorCode, errorMessage *string
	err := row.Scan(&r.ID, &r.TenantID, &r.BusinessRequestID, &r.BusinessRequestKey,
		&r.TicketID, &r.RetryOfRunID, &r.ProfileID, &r.ProfileHash, &r.BudgetBatchID,
		&r.SnapshotID, &r.SnapshotHash, &r.VersionVector, &r.State, &r.Outcome, &errorCode, &errorMessage,
		&r.AttemptNo, &r.RecoveryCount, &r.CursorVersion, &r.RunTimeoutSeconds, &r.RunDeadline,
		&r.AttemptDeadline, &r.LeaseUntil, &r.NextAttemptAt, &r.PermissionExpiresAt, &r.ProposalRef,
		&r.StopReason, &r.CancelRequestedAt, &r.CreatedAt, &r.UpdatedAt,
		&a.WorkerID, &a.SessionID, &a.FencingToken, &a.ActiveCallID, &a.CheckpointBytes,
		&a.EventSequence, &a.NextStepID, &a.NextStepKind, &a.NextInputHash)
	if err == nil && errorCode != nil && errorMessage != nil {
		r.Error = &agentrun.Failure{Code: *errorCode, Message: *errorMessage}
	}
	return r, a, err
}

func lockRun(ctx context.Context, tx pgx.Tx, tenant, id string) (agentrun.Run, agentrun.Authority, error) {
	return readRun(tx.QueryRow(ctx, "select "+runColumns+" from runs where tenant_id=$1 and run_id=$2 for update", tenant, id))
}

func saveRun(ctx context.Context, tx pgx.Tx, r *agentrun.Run, a *agentrun.Authority) error {
	var code, message *string
	if r.Error != nil {
		code, message = &r.Error.Code, &r.Error.Message
	}
	_, err := tx.Exec(ctx, `update runs set state=$3, outcome=$4, error_code=$5, error_message=$6,
		attempt_no=$7, recovery_count=$8, cursor_version=$9, attempt_deadline=$10, lease_until=$11,
		next_attempt_at=$12, permission_expires_at=$13, proposal_ref=$14, stop_reason=$15,
		cancel_requested_at=$16, updated_at=$17, worker_id=nullif($18,''), session_id=nullif($19,'')::uuid,
		fencing_token=$20, active_call_id=$21, checkpoint_bytes=$22, event_sequence=$23,
		next_step_id=$24, next_step_kind=$25, next_input_hash=$26 where tenant_id=$1 and run_id=$2`,
		r.TenantID, r.ID, r.State, r.Outcome, code, message, r.AttemptNo, r.RecoveryCount, r.CursorVersion,
		r.AttemptDeadline, r.LeaseUntil, r.NextAttemptAt, r.PermissionExpiresAt, r.ProposalRef, r.StopReason,
		r.CancelRequestedAt, r.UpdatedAt, a.WorkerID, a.SessionID, a.FencingToken, a.ActiveCallID,
		a.CheckpointBytes, a.EventSequence, a.NextStepID, a.NextStepKind, a.NextInputHash)
	return err
}

func appendEvent(ctx context.Context, tx pgx.Tx, r *agentrun.Run, a *agentrun.Authority, kind string, now time.Time) error {
	if a.EventSequence >= agentrun.MaxSafeInteger {
		return agentrun.ErrInternal
	}
	a.EventSequence++
	_, err := tx.Exec(ctx, `insert into run_events
		(tenant_id,run_id,sequence,event_type,state,attempt_no,cursor_version,created_at)
		values ($1,$2,$3,$4,$5,$6,$7,$8)`, r.TenantID, r.ID, a.EventSequence, kind, r.State, r.AttemptNo, r.CursorVersion, now)
	return err
}
