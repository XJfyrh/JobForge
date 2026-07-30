// Package notify provides PostgreSQL LISTEN/NOTIFY based event signaling
// for inter-process communication (ADR-0003). NOTIFY is used as a hint only;
// consumers must re-query the database to confirm actual state.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Channel is the PostgreSQL NOTIFY channel used for job-ready signals.
const Channel = "jobforge_job_ready"

// Notifier sends pg_notify signals after transaction commits.
type Notifier struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewNotifier creates a Notifier that uses the connection pool to send NOTIFY.
func NewNotifier(pool *pgxpool.Pool, logger *slog.Logger) *Notifier {
	return &Notifier{pool: pool, logger: logger}
}

// NotifyJobReady sends a notification that new jobs may be available.
// The payload is the queue name (empty string for "all queues").
// Failure to notify is non-fatal; only a warning is logged.
func (n *Notifier) NotifyJobReady(ctx context.Context, queue string) error {
	_, err := n.pool.Exec(ctx, "select pg_notify($1, $2)", Channel, queue)
	if err != nil {
		n.logger.Warn("failed to send pg_notify", "channel", Channel, "queue", queue, "error", err)
		return fmt.Errorf("pg_notify: %w", err)
	}
	return nil
}

// Listener maintains a dedicated PostgreSQL connection for LISTEN and broadcasts
// notifications to multiple concurrent waiters via a fan-out channel architecture.
// A single background goroutine owns the pgx.Conn (which is NOT safe for concurrent
// use); callers register ephemeral waiter channels that receive broadcast signals.
//
// This makes Listener safe for use by multiple gRPC Poll goroutines concurrently.
type Listener struct {
	connConfig *pgx.ConnConfig
	logger     *slog.Logger

	mu      sync.Mutex
	waiters map[chan struct{}]struct{}

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// NewListener creates a Listener and starts the background LISTEN loop.
// Call Close to stop the loop and release the connection.
func NewListener(databaseURL string, logger *slog.Logger) (*Listener, error) {
	cfg, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse listener config: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	l := &Listener{
		connConfig: cfg,
		logger:     logger,
		waiters:    make(map[chan struct{}]struct{}),
		ctx:        ctx,
		cancel:     cancel,
		done:       make(chan struct{}),
	}

	go l.run()
	return l, nil
}

// Listen is a no-op kept for interface compatibility. The background loop
// connects automatically on startup and on reconnection.
func (l *Listener) Listen(_ context.Context) error {
	return nil
}

// WaitForNotification blocks until a notification is broadcast or the context
// is cancelled. Returns true if a notification was received. Safe for concurrent
// use by multiple goroutines.
func (l *Listener) WaitForNotification(ctx context.Context) bool {
	ch := make(chan struct{}, 1)

	l.mu.Lock()
	l.waiters[ch] = struct{}{}
	l.mu.Unlock()

	defer func() {
		l.mu.Lock()
		delete(l.waiters, ch)
		l.mu.Unlock()
	}()

	select {
	case <-ch:
		return true
	case <-ctx.Done():
		return false
	}
}

// Close stops the background LISTEN loop and releases the connection.
func (l *Listener) Close() {
	l.cancel()
	<-l.done
}

// run is the background loop that owns the LISTEN connection. It reconnects
// on failure and broadcasts notifications to all registered waiters.
func (l *Listener) run() {
	defer close(l.done)

	for l.ctx.Err() == nil {
		conn, err := pgx.ConnectConfig(l.ctx, l.connConfig)
		if err != nil {
			if l.ctx.Err() != nil {
				return
			}
			l.logger.Warn("listener connect failed, retrying", "error", err)
			l.sleep(1 * time.Second)
			continue
		}

		// Channel is a package-level constant, not user input. LISTEN does not
		// support parameterized queries; quote the identifier for safety.
		_, err = conn.Exec(l.ctx, `listen "`+Channel+`"`)
		if err != nil {
			_ = conn.Close(l.ctx)
			if l.ctx.Err() != nil {
				return
			}
			l.logger.Warn("listener LISTEN failed, retrying", "error", err)
			l.sleep(1 * time.Second)
			continue
		}

		l.logger.Info("listener connected", "channel", Channel)

		// Block on notifications until connection error or shutdown.
		for {
			_, waitErr := conn.WaitForNotification(l.ctx)
			if waitErr != nil {
				if l.ctx.Err() != nil {
					_ = conn.Close(context.Background())
					return
				}
				l.logger.Warn("listener connection lost, reconnecting", "error", waitErr)
				_ = conn.Close(context.Background())
				break
			}
			// Received a notification; broadcast to all waiters.
			l.broadcast()
		}

		// Brief pause before reconnection.
		l.sleep(100 * time.Millisecond)
	}
}

// broadcast signals all registered waiters (non-blocking).
func (l *Listener) broadcast() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ch := range l.waiters {
		select {
		case ch <- struct{}{}:
		default:
			// Waiter already has a pending signal; skip.
		}
	}
}

// sleep waits for d or until the listener is closed.
func (l *Listener) sleep(d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-l.ctx.Done():
	}
}
