package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store"
	workerv1 "github.com/xjfyrh/jobforge/proto/jobforge/worker/v1"
)

// Gateway store SQL queries.

const upsertWorker = `
insert into workers (worker_id, instance_id, supported_types, queues, capacity, version, session_id, last_heartbeat_at, registered_at, status)
values ($1, $2, $3, $4, $5, $6, $7, now(), now(), 'active')
on conflict (worker_id) do update set
    instance_id = excluded.instance_id,
    supported_types = excluded.supported_types,
    queues = excluded.queues,
    capacity = excluded.capacity,
    version = excluded.version,
    session_id = excluded.session_id,
    last_heartbeat_at = now(),
    status = 'active'
`

const getJobState = `
select state from jobs where id = $1
`

const getJobRunAt = `
select run_at from jobs where id = $1
`

// workerCounts samples workers with a fresh liveness timestamp per
// (version, status) for the jobforge_workers_active gauge (PRD 12.1).
// Workers whose last heartbeat is older than the freshness window (or that
// never reported one) are not counted, so the gauge decays when workers
// crash instead of staying inflated forever.
const workerCounts = `
select coalesce(version, ''), status, count(*)
from workers
where last_heartbeat_at is not null
  and last_heartbeat_at > now() - $1::interval
group by version, status
`

// refreshWorkerHeartbeat advances the worker's liveness timestamp, throttled
// at the SQL layer: the row is only written when the stored timestamp is
// missing or at least minInterval old. This keeps write amplification
// bounded regardless of how many Poll/Heartbeat RPCs arrive.
const refreshWorkerHeartbeat = `
update workers
set last_heartbeat_at = now()
where worker_id = $1
  and (last_heartbeat_at is null or last_heartbeat_at < now() - $2::interval)
`

// staleWorkers lists workers whose liveness timestamp is missing or older
// than the given threshold, oldest first (operational query).
const staleWorkers = `
select worker_id, instance_id, coalesce(version, ''), status, last_heartbeat_at, registered_at
from workers
where last_heartbeat_at is null or last_heartbeat_at < now() - $1::interval
order by last_heartbeat_at asc nulls first
`

// RegisterWorker upserts a worker registration record.
func (s *JobStore) RegisterWorker(ctx context.Context, req *workerv1.RegisterRequest, sessionID string) error {
	_, err := s.pool.Exec(ctx, upsertWorker,
		req.WorkerId,
		req.InstanceId,
		req.SupportedTypes,
		req.Queues,
		req.Capacity,
		req.Version,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("register worker %s: %w", req.WorkerId, err)
	}
	return nil
}

// GetJobState returns the current state of a job.
func (s *JobStore) GetJobState(ctx context.Context, jobID string) (domain.JobState, error) {
	var state string
	err := s.pool.QueryRow(ctx, getJobState, jobID).Scan(&state)
	if err != nil {
		return "", fmt.Errorf("get job state %s: %w", jobID, err)
	}
	return domain.JobState(state), nil
}

// GetJobRunAt returns the run_at timestamp of a job (used for next_retry_at).
func (s *JobStore) GetJobRunAt(ctx context.Context, jobID string) (*time.Time, error) {
	var runAt time.Time
	err := s.pool.QueryRow(ctx, getJobRunAt, jobID).Scan(&runAt)
	if err != nil {
		return nil, fmt.Errorf("get job run_at %s: %w", jobID, err)
	}
	return &runAt, nil
}

// WorkerCounts samples workers with a heartbeat fresher than freshWithin
// per (version, status) so the Gateway can emit the jobforge_workers_active
// gauge (PRD 12.1). Crashed workers drop out once their heartbeat ages past
// the window.
func (s *JobStore) WorkerCounts(ctx context.Context, freshWithin time.Duration) ([]store.WorkerCountRow, error) {
	rows, err := s.pool.Query(ctx, workerCounts, freshWithin.String())
	if err != nil {
		return nil, fmt.Errorf("worker counts: %w", err)
	}
	defer rows.Close()

	var result []store.WorkerCountRow
	for rows.Next() {
		var r store.WorkerCountRow
		var status *string
		if err := rows.Scan(&r.Version, &status, &r.Count); err != nil {
			return nil, fmt.Errorf("scan worker count row: %w", err)
		}
		if status != nil {
			r.Status = *status
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// RefreshWorkerHeartbeat advances the worker's liveness timestamp, writing
// at most once per minInterval (throttled by the UPDATE condition).
func (s *JobStore) RefreshWorkerHeartbeat(ctx context.Context, workerID string, minInterval time.Duration) error {
	_, err := s.pool.Exec(ctx, refreshWorkerHeartbeat, workerID, minInterval.String())
	if err != nil {
		return fmt.Errorf("refresh worker heartbeat %s: %w", workerID, err)
	}
	return nil
}

// StaleWorkers lists workers whose liveness timestamp is missing or older
// than olderThan, oldest first.
func (s *JobStore) StaleWorkers(ctx context.Context, olderThan time.Duration) ([]store.WorkerRow, error) {
	rows, err := s.pool.Query(ctx, staleWorkers, olderThan.String())
	if err != nil {
		return nil, fmt.Errorf("stale workers: %w", err)
	}
	defer rows.Close()

	var result []store.WorkerRow
	for rows.Next() {
		var r store.WorkerRow
		if err := rows.Scan(&r.WorkerID, &r.InstanceID, &r.Version, &r.Status,
			&r.LastHeartbeatAt, &r.RegisteredAt); err != nil {
			return nil, fmt.Errorf("scan worker row: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
