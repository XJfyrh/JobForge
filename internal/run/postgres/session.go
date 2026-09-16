package postgres

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

const sessionTTL = 60 * time.Second

func readSession(row pgx.Row) (agentrun.Session, error) {
	var session agentrun.Session
	err := row.Scan(&session.ID, &session.WorkerID, &session.StartupID, &session.Version,
		&session.CreatedAt, &session.SeenAt, &session.ExpiresAt)
	return session, err
}

const sessionColumns = "session_id,worker_id,startup_id,version,created_at,seen_at,expires_at"

// Register serializes registrations for a configured principal. A second
// startup cannot evict a live process or replace any existing Run lease.
func (s *Store) Register(ctx context.Context, principal, startupID, version string) (agentrun.Session, error) {
	var session agentrun.Session
	if _, ok := s.workers[principal]; !ok {
		return session, agentrun.ErrForbidden
	}
	if !validUUID(startupID) || !agentrun.ValidIdentifier(version) {
		return session, agentrun.ErrInvalidArgument
	}
	err := s.transact(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock(hashtextextended($1,18))", principal); err != nil {
			return err
		}
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		previous, err := readSession(tx.QueryRow(ctx, "select "+sessionColumns+" from worker_sessions where worker_id=$1 and startup_id=$2", principal, startupID))
		if err == nil {
			if previous.Version != version {
				return agentrun.ErrConflict
			}
			// Repeating an expired startup must not resurrect its old authority.
			if !previous.ExpiresAt.After(now) {
				return agentrun.ErrStaleLease
			}
			if err := s.checkExecutorVersion(principal, version); err != nil {
				return err
			}
			session = previous
			session.AuthorityObservedAt = now
			return nil
		}
		if err != pgx.ErrNoRows {
			return err
		}
		var live bool
		if err := tx.QueryRow(ctx, "select exists(select 1 from worker_sessions where worker_id=$1 and expires_at>$2)", principal, now).Scan(&live); err != nil {
			return err
		}
		if live {
			return agentrun.ErrConflict
		}
		if err := s.checkExecutorVersion(principal, version); err != nil {
			return err
		}
		session = agentrun.Session{ID: uuid.NewString(), WorkerID: principal, StartupID: startupID, Version: version,
			CreatedAt: now, SeenAt: now, ExpiresAt: now.Add(sessionTTL), AuthorityObservedAt: now}
		_, err = tx.Exec(ctx, `insert into worker_sessions(session_id,worker_id,startup_id,version,created_at,seen_at,expires_at)
			values($1,$2,$3,$4,$5,$5,$6)`, session.ID, principal, startupID, version, now, session.ExpiresAt)
		return err
	})
	return session, err
}

// checkExecutorVersion binds every advertised capability to the same immutable
// deployment. A worker with no profiles can stay idle but cannot claim work.
// Disabling execution does not erase old profiles needed for late accounting.
func (s *Store) checkExecutorVersion(principal, version string) error {
	// The retired deployed runtime retains only original-call late accounting.
	if version == "linux-v2-ack-runtime-1" {
		return agentrun.ErrProfileUnavailable
	}
	for _, id := range s.workers[principal].ProfileIDs {
		profile, ok := s.profiles[id]
		if !ok || profile.ExecutorVersion != version {
			return agentrun.ErrProfileUnavailable
		}
	}
	return nil
}

// checkSession performs no blocking row lock. Session expiry only moves forward
// on valid heartbeats; registration cannot revoke or repurpose an old session.
func (s *Store) checkSession(ctx context.Context, tx pgx.Tx, principal, workerID, sessionID, tenant, profileID string, now time.Time, requireLive bool) error {
	worker, ok := s.workers[principal]
	if !ok || principal != workerID || (tenant != "" && !slices.Contains(worker.Tenants, tenant)) {
		return agentrun.ErrForbidden
	}
	// Late accounting remains authorized for its old profile even if the
	// deployment no longer advertises it as an executable capability.
	if requireLive && profileID != "" && !slices.Contains(worker.ProfileIDs, profileID) {
		return agentrun.ErrProfileUnavailable
	}
	var expires time.Time
	var version string
	if err := tx.QueryRow(ctx, "select expires_at,version from worker_sessions where worker_id=$1 and session_id=$2", workerID, sessionID).Scan(&expires, &version); err != nil {
		if err == pgx.ErrNoRows {
			return agentrun.ErrStaleLease
		}
		return err
	}
	if requireLive && !expires.After(now) {
		return agentrun.ErrStaleLease
	}
	if requireLive {
		return s.checkExecutorVersion(principal, version)
	}
	return nil
}

// HeartbeatSession renews idle-worker liveness without acquiring or renewing a
// Run lease. Expired sessions require a new startup registration.
func (s *Store) HeartbeatSession(ctx context.Context, principal, workerID, sessionID string) (agentrun.Session, error) {
	var session agentrun.Session
	if principal != workerID || !validUUID(sessionID) {
		return session, agentrun.ErrUnauthorized
	}
	if _, ok := s.workers[principal]; !ok {
		return session, agentrun.ErrForbidden
	}
	err := s.transact(ctx, func(tx pgx.Tx) error {
		// Serialize liveness with Register so a renewal cannot race an expired
		// session being replaced by a different startup for this principal.
		if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock(hashtextextended($1,18))", principal); err != nil {
			return err
		}
		var err error
		session, err = readSession(tx.QueryRow(ctx, "select "+sessionColumns+" from worker_sessions where worker_id=$1 and session_id=$2 for update", workerID, sessionID))
		if err != nil {
			return err
		}
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if !session.ExpiresAt.After(now) {
			return agentrun.ErrStaleLease
		}
		session.SeenAt, session.ExpiresAt = now, now.Add(sessionTTL)
		session.AuthorityObservedAt = now
		_, err = tx.Exec(ctx, "update worker_sessions set seen_at=$3,expires_at=$4 where worker_id=$1 and session_id=$2", workerID, sessionID, now, session.ExpiresAt)
		return err
	})
	return session, err
}
