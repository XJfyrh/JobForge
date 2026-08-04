// Package outbox implements the outbox publisher (PRD v0.2 FR-610~614).
//
// Job state transitions write events to outbox_events inside their own
// transactions; this package publishes them asynchronously with at-least-once
// semantics. Publishing never touches job core state: failures, duplicate
// publications and publisher crashes must not alter job outcomes.
//
// Recovery model: the publisher holds no in-memory progress. After a crash
// it resumes purely from the outbox table (published_at IS NULL), so any
// number of restarts is safe. Consumers must deduplicate by event_id.
package outbox

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/xjfyrh/jobforge/internal/observability"
	"github.com/xjfyrh/jobforge/internal/store"
)

// DefaultChannel is the default PostgreSQL NOTIFY channel for outbox events.
// It is intentionally separate from the job-ready signal channel (ADR-0003).
const DefaultChannel = "jobforge_outbox"

// Channel is a pluggable publish target for outbox events (PRD v0.2 FR-611).
// The default implementation uses PostgreSQL LISTEN/NOTIFY; no external MQ
// dependency is allowed.
type Channel interface {
	// Publish delivers one event. Returning an error marks the attempt as
	// failed; the event stays unpublished and is retried with backoff.
	Publish(ctx context.Context, event *store.OutboxEvent) error
}

// NotifyChannel publishes outbox events via pg_notify. The NOTIFY payload is
// only the event_id (decimal string): it stays well below the ~8000-byte
// NOTIFY limit, and consumers re-query outbox_events by event_id to obtain
// the full event. NOTIFY is a hint only, consistent with ADR-0003.
type NotifyChannel struct {
	pool    *pgxpool.Pool
	channel string
	logger  *slog.Logger
}

// NewNotifyChannel creates a NOTIFY-based channel with the given channel name.
func NewNotifyChannel(pool *pgxpool.Pool, channel string, logger *slog.Logger) *NotifyChannel {
	if channel == "" {
		channel = DefaultChannel
	}
	return &NotifyChannel{pool: pool, channel: channel, logger: logger}
}

// Publish implements Channel. Notification failure is an error: the event
// remains unpublished and will be retried (at-least-once).
func (c *NotifyChannel) Publish(ctx context.Context, event *store.OutboxEvent) error {
	payload := strconv.FormatInt(event.EventID, 10)
	_, err := c.pool.Exec(ctx, "select pg_notify($1, $2)", c.channel, payload)
	if err != nil {
		return fmt.Errorf("pg_notify %s: %w", c.channel, err)
	}
	return nil
}

// Config controls publisher behavior.
type Config struct {
	// PollInterval is the minimum interval between publish rounds when there
	// is backlog (PRD v0.2 §11.4: idle interval configurable, default >= 1s).
	PollInterval time.Duration

	// MaxIdleInterval caps the backoff applied when the backlog is empty or
	// a round fails. The publisher never polls faster than PollInterval and
	// slows down toward this bound when idle.
	MaxIdleInterval time.Duration

	// BatchSize bounds how many events one publish round claims.
	BatchSize int

	// Retention is how long published events are kept before cleanup.
	// Zero or negative disables the retention cleaner.
	Retention time.Duration

	// CleanupInterval is the period of the retention cleaner loop.
	CleanupInterval time.Duration
}

// Publisher polls the outbox table and publishes unpublished events through
// the configured Channel, tracking progress via published_at and
// publish_attempts (PRD v0.2 FR-610/612).
type Publisher struct {
	store   store.OutboxStore
	channel Channel
	cfg     Config
	logger  *slog.Logger
	metrics *observability.Metrics
}

// New creates a Publisher. metrics may be nil in tests.
func New(st store.OutboxStore, ch Channel, cfg Config, logger *slog.Logger, metrics *observability.Metrics) *Publisher {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 1 * time.Second
	}
	if cfg.MaxIdleInterval < cfg.PollInterval {
		cfg.MaxIdleInterval = 30 * time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.CleanupInterval <= 0 {
		cfg.CleanupInterval = time.Hour
	}
	return &Publisher{store: st, channel: ch, cfg: cfg, logger: logger, metrics: metrics}
}

// Run starts the retention cleaner (if enabled) and the publish loop,
// blocking until ctx is cancelled. Returns ctx.Err() semantics like other
// JobForge long-running components: nil on graceful shutdown.
func (p *Publisher) Run(ctx context.Context) error {
	done := make(chan struct{})
	defer close(done)

	if p.cfg.Retention > 0 {
		cleaner := NewCleaner(p.store, p.cfg.Retention, p.cfg.CleanupInterval, p.logger)
		go func() {
			_ = cleaner.Run(ctx)
		}()
	}

	interval := p.cfg.PollInterval
	for {
		hadWork, failed := p.publishRound(ctx)

		switch {
		case failed:
			// Back off after publish failures to avoid retry storms.
			interval = backoff(interval, p.cfg.MaxIdleInterval)
		case hadWork:
			// Backlog present: keep the minimum polling pace.
			interval = p.cfg.PollInterval
		default:
			// Backlog empty: slow down toward the idle bound.
			interval = backoff(interval, p.cfg.MaxIdleInterval)
		}

		p.refreshPendingGauge(ctx)

		if !p.sleep(ctx, interval) {
			return nil
		}
	}
}

// publishRound claims one batch of unpublished events and publishes them.
// Returns hadWork (batch non-empty) and failed (any publish/mark error).
func (p *Publisher) publishRound(ctx context.Context) (hadWork, failed bool) {
	events, err := p.store.FetchUnpublished(ctx, p.cfg.BatchSize)
	if err != nil {
		if ctx.Err() != nil {
			return false, false
		}
		p.logger.Warn("fetch unpublished events failed", "error", err)
		return false, true
	}
	if len(events) == 0 {
		return false, false
	}

	for _, ev := range events {
		if ctx.Err() != nil {
			// Graceful shutdown: leave remaining events unpublished; the
			// next publisher run resumes from the table.
			return true, false
		}
		if !p.publishOne(ctx, ev) {
			failed = true
		}
	}
	return true, failed
}

// publishOne publishes a single event and records the outcome. Returns true
// on success.
func (p *Publisher) publishOne(ctx context.Context, ev *store.OutboxEvent) bool {
	ctx, span := observability.Tracer("jobforge.outbox").Start(ctx, "outbox.publish")
	defer span.End()
	span.SetAttributes(
		attribute.Int64("outbox.event_id", ev.EventID),
		attribute.String("outbox.event_type", ev.EventType),
		attribute.String("outbox.aggregate_id", ev.AggregateID),
	)

	if err := p.channel.Publish(ctx, ev); err != nil {
		span.RecordError(err)
		p.observeFailure(ev.EventType, "channel_error")
		if markErr := p.store.MarkPublishFailed(ctx, ev.EventID); markErr != nil {
			p.logger.Warn("mark publish failed", "event_id", ev.EventID, "error", markErr)
		}
		p.logger.Warn("publish event failed",
			"event_id", ev.EventID, "event_type", ev.EventType, "error", err)
		return false
	}

	marked, err := p.store.MarkPublished(ctx, ev.EventID)
	if err != nil {
		span.RecordError(err)
		p.observeFailure(ev.EventType, "mark_error")
		p.logger.Warn("mark published failed", "event_id", ev.EventID, "error", err)
		return false
	}

	if marked {
		p.observePublished(ev.EventType)
	}
	return true
}

// refreshPendingGauge samples the unpublished backlog into the gauge metric.
func (p *Publisher) refreshPendingGauge(ctx context.Context) {
	if p.metrics == nil {
		return
	}
	pending, err := p.store.CountPending(ctx)
	if err != nil {
		if ctx.Err() == nil {
			p.logger.Warn("count pending events failed", "error", err)
		}
		return
	}
	p.metrics.OutboxPending.Record(ctx, pending)
}

// observePublished increments the successful-publish counter.
func (p *Publisher) observePublished(eventType string) {
	if p.metrics == nil {
		return
	}
	ctx := context.Background()
	p.metrics.OutboxPublishedTotal.Add(ctx, 1,
		metric.WithAttributes(attribute.String("event_type", eventType)))
}

// observeFailure increments the publish-failure counter.
func (p *Publisher) observeFailure(eventType, reason string) {
	if p.metrics == nil {
		return
	}
	ctx := context.Background()
	p.metrics.OutboxPublishFailuresTotal.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("event_type", eventType),
			attribute.String("reason", reason),
		))
}

// sleep waits for d or until ctx is cancelled. Returns false when cancelled.
func (p *Publisher) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// backoff doubles the interval up to the limit.
func backoff(current, limit time.Duration) time.Duration {
	next := current * 2
	if next > limit {
		next = limit
	}
	return next
}
