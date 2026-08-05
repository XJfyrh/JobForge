package ctl

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// OutboxStatus summarizes the outbox backlog (PRD v0.2 FR-621): the number
// of unpublished events and the creation time of the oldest one.
type OutboxStatus struct {
	Pending           int64      `json:"pending"`
	OldestUnpublished *time.Time `json:"oldest_unpublished,omitempty"`
}

// outboxStatusQuery is strictly read-only. It scans the partial index on
// unpublished rows added by migration 0004/0007.
const outboxStatusQuery = `
select count(*) filter (where published_at is null),
       min(created_at) filter (where published_at is null)
from outbox_events`

// QueryOutboxStatus connects to PostgreSQL using the given DSN and returns a
// read-only outbox backlog summary. The connection is short-lived and closed
// before returning. This is the only ctl operation that requires database
// credentials; job operations go through the HTTP API.
func QueryOutboxStatus(ctx context.Context, dsn string) (*OutboxStatus, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping database: %w", err)
	}

	var status OutboxStatus
	if err := pool.QueryRow(ctx, outboxStatusQuery).Scan(&status.Pending, &status.OldestUnpublished); err != nil {
		return nil, fmt.Errorf("query outbox status: %w", err)
	}
	return &status, nil
}
