package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/xjfyrh/jobforge/internal/domain"
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
