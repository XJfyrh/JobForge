package ctl

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// WorkerStatus describes one registered worker's liveness as seen by the
// operational workers-status query. Stale is true when the last heartbeat
// is missing or older than the caller-supplied threshold.
type WorkerStatus struct {
	WorkerID        string     `json:"worker_id"`
	InstanceID      string     `json:"instance_id"`
	Version         string     `json:"version"`
	Status          string     `json:"status"`
	LastHeartbeatAt *time.Time `json:"last_heartbeat_at,omitempty"`
	RegisteredAt    time.Time  `json:"registered_at"`
	Stale           bool       `json:"stale"`
}

// workersStatusQuery is strictly read-only. It lists every registered worker
// together with its liveness timestamp; staleness is derived against the
// caller-supplied threshold.
const workersStatusQuery = `
select worker_id, instance_id, coalesce(version, ''), status, last_heartbeat_at, registered_at
from workers
order by last_heartbeat_at asc nulls first`

// QueryWorkers connects to PostgreSQL using the given DSN and returns the
// full worker registry with a per-row staleness flag (last heartbeat missing
// or older than staleAfter). The connection is short-lived and closed before
// returning. Like outbox-status, this is a read-only database query that
// does not require API credentials.
func QueryWorkers(ctx context.Context, dsn string, staleAfter time.Duration) ([]WorkerStatus, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping database: %w", err)
	}

	rows, err := pool.Query(ctx, workersStatusQuery)
	if err != nil {
		return nil, fmt.Errorf("query workers: %w", err)
	}
	defer rows.Close()

	var result []WorkerStatus
	for rows.Next() {
		var w WorkerStatus
		if err := rows.Scan(&w.WorkerID, &w.InstanceID, &w.Version, &w.Status,
			&w.LastHeartbeatAt, &w.RegisteredAt); err != nil {
			return nil, fmt.Errorf("scan worker row: %w", err)
		}
		w.Stale = w.LastHeartbeatAt == nil || time.Since(*w.LastHeartbeatAt) > staleAfter
		result = append(result, w)
	}
	return result, rows.Err()
}

// RenderWorkers renders the worker registry with liveness information.
func RenderWorkers(w io.Writer, format string, workers []WorkerStatus, staleAfter time.Duration) error {
	if format == OutputJSON {
		return writeIndentedJSON(w, workers)
	}

	lw := &lineWriter{}
	if len(workers) == 0 {
		lw.printf(w, "(no registered workers)\n")
		return lw.err
	}

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	lw.printf(tw, "WORKER\tVERSION\tSTATUS\tLAST HEARTBEAT\tSTALE\n")
	for i := range workers {
		wk := &workers[i]
		last := "never"
		age := "-"
		if wk.LastHeartbeatAt != nil {
			last = formatTime(*wk.LastHeartbeatAt)
			age = time.Since(*wk.LastHeartbeatAt).Truncate(time.Second).String()
		}
		stale := "-"
		if wk.Stale {
			stale = fmt.Sprintf("yes (age %s > %s)", age, staleAfter.Truncate(time.Second))
		}
		lw.printf(tw, "%s\t%s\t%s\t%s\t%s\n", wk.WorkerID, wk.Version, wk.Status, last, stale)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("flush table: %w", err)
	}
	return lw.err
}
