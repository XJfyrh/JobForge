// Package outbox implements the outbox publisher (PRD v0.2 FR-610~614,
// upgraded by PRD v0.3 §5.1: pluggable external transport).
//
// Job state transitions write events to outbox_events inside their own
// transactions; this package publishes them asynchronously with at-least-once
// semantics through the configured Transport (notify | redis_streams,
// ADR-0006). Publishing never touches job core state: failures, duplicate
// publications and publisher crashes must not alter job outcomes.
//
// Recovery model: the publisher holds no in-memory progress. After a crash
// it resumes purely from the outbox table (published_at IS NULL), so any
// number of restarts is safe. Consumers must deduplicate by event_id.
// Fetching is an atomic claim (claimed_at stamped in the same statement as
// FOR UPDATE SKIP LOCKED), so concurrent publishers never pick up the same
// event. Claims are released explicitly on publish failure and for events
// left unprocessed at graceful shutdown; only a hard crash leaves the claim
// stamped, and such claims become reclaimable after the claim TTL.
package outbox

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/xjfyrh/jobforge/internal/observability"
	"github.com/xjfyrh/jobforge/internal/store"
)

// nonDurableWarnInterval bounds how often the publisher re-logs the
// non-durable transport warning (FR-705 bounded-frequency warning).
const nonDurableWarnInterval = 10 * time.Minute

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

	// PublishConcurrency bounds how many events of one round are delivered
	// to the transport concurrently (NFR-302 throughput; no global ordering
	// is guaranteed anyway). <= 0 defaults to 8.
	PublishConcurrency int

	// Retention is how long published events are kept before cleanup.
	// Zero or negative disables the retention cleaner.
	Retention time.Duration

	// CleanupInterval is the period of the retention cleaner loop.
	CleanupInterval time.Duration
}

// Publisher polls the outbox table and publishes unpublished events through
// the configured Transport, tracking progress via published_at and
// publish_attempts (PRD v0.2 FR-610/612, PRD v0.3 FR-701~705).
type Publisher struct {
	store     store.OutboxStore
	transport Transport
	cfg       Config
	logger    *slog.Logger
	metrics   *observability.Metrics

	// lastNonDurableWarn throttles the non-durable transport warning.
	lastNonDurableWarn time.Time
}

// New creates a Publisher. metrics may be nil in tests.
func New(st store.OutboxStore, tr Transport, cfg Config, logger *slog.Logger, metrics *observability.Metrics) *Publisher {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 1 * time.Second
	}
	if cfg.MaxIdleInterval < cfg.PollInterval {
		cfg.MaxIdleInterval = 30 * time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.PublishConcurrency <= 0 {
		cfg.PublishConcurrency = 8
	}
	if cfg.CleanupInterval <= 0 {
		cfg.CleanupInterval = time.Hour
	}
	return &Publisher{store: st, transport: tr, cfg: cfg, logger: logger, metrics: metrics}
}

// Run starts the retention cleaner (if enabled) and the publish loop,
// blocking until ctx is cancelled. Returns ctx.Err() semantics like other
// JobForge long-running components: nil on graceful shutdown.
func (p *Publisher) Run(ctx context.Context) error {
	done := make(chan struct{})
	defer close(done)

	// FR-705: transport identity must be visible at startup; non-durable
	// transports are explicitly flagged as compatibility/local-dev only.
	p.logger.Info("outbox publisher starting",
		"transport", p.transport.Name(),
		"durable", p.transport.Durable(),
	)
	p.warnIfNonDurable(true)

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
		p.warnIfNonDurable(false)

		if !p.sleep(ctx, interval) {
			return nil
		}
	}
}

// warnIfNonDurable logs the non-durable transport warning (FR-705). The
// first call (startup) always logs; later calls are throttled to at most
// once per nonDurableWarnInterval so a long-running notify deployment is not
// flooded.
func (p *Publisher) warnIfNonDurable(force bool) {
	if p.transport.Durable() {
		return
	}
	now := time.Now()
	if !force && now.Sub(p.lastNonDurableWarn) < nonDurableWarnInterval {
		return
	}
	p.lastNonDurableWarn = now
	p.logger.Warn("outbox transport is NOT durable",
		"transport", p.transport.Name(),
		"note", "events are not replayable after broker/subscriber loss; set JOBFORGE_OUTBOX_TRANSPORT=redis_streams for durable delivery (ADR-0006)",
	)
}

// publishRound claims one batch of unpublished events, delivers them to the
// transport with bounded concurrency, and marks the successful ones in a
// single batch statement (NFR-302: per-event marks would serialize the round
// on database round trips). Delivery order within a round is not guaranteed,
// matching the no-global-ordering contract (PRD v0.3 §5.1).
// Returns hadWork (batch non-empty) and failed (any deliver/mark error).
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

	var (
		mu       sync.Mutex
		okEvents []*store.OutboxEvent
		failures int
	)
	sem := make(chan struct{}, p.cfg.PublishConcurrency)
	var wg sync.WaitGroup
	for _, ev := range events {
		if ctx.Err() != nil {
			// Graceful shutdown: release the claims on every event not yet
			// delivered so a fresh publisher can pick them up immediately
			// instead of waiting out the claim TTL.
			p.releaseClaim(ctx, ev.EventID)
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(ev *store.OutboxEvent) {
			defer wg.Done()
			defer func() { <-sem }()
			if p.deliverOne(ctx, ev) {
				mu.Lock()
				okEvents = append(okEvents, ev)
				mu.Unlock()
			} else {
				mu.Lock()
				failures++
				mu.Unlock()
			}
		}(ev)
	}
	wg.Wait()

	// Batch-mark everything the transport acknowledged. A crash between the
	// broker ACK and this mark redelivers those events later (consumers
	// deduplicate by event_id, AT-18).
	//
	// Operational constraint: a round must finish well within OutboxClaimTTL.
	// Otherwise another publisher can reclaim the still-claimed rows and
	// redeliver them while this instance is alive; that stays inside the
	// at-least-once contract but shows up as marked < delivered below.
	if len(okEvents) > 0 {
		ids := make([]int64, len(okEvents))
		for i, ev := range okEvents {
			ids[i] = ev.EventID
		}
		if marked, err := p.store.MarkPublishedBatch(ctx, ids); err != nil {
			p.observeTransportFailure("mark_error")
			// Keep every delivered event reclaimable instead of letting the
			// claim sit until the TTL.
			for _, ev := range okEvents {
				p.releaseClaim(ctx, ev.EventID)
			}
			p.logger.Warn("mark published batch failed",
				"events", len(okEvents), "error", err)
			failures++
		} else {
			// Claims are exclusive per event, so normally batch-affected ==
			// delivered. A shortfall means a claim-TTL reclaim race (see the
			// constraint above): surface it, then count the delivered events
			// — redelivery is permitted under at-least-once.
			if marked != int64(len(okEvents)) {
				p.logger.Warn("batch mark transitioned fewer rows than delivered (claim TTL reclaim race?)",
					"delivered", len(okEvents), "marked", marked)
			}
			for _, ev := range okEvents {
				p.observePublishLag(ev)
				p.observePublished(ev.EventType)
			}
		}
	}
	return true, failures > 0
}

// releaseClaim drops the atomic claim so the event becomes immediately
// reclaimable. Best-effort: a failure only delays recovery to the claim TTL.
// Uses a background context: the caller's ctx may already be cancelled
// (graceful shutdown) while the release itself is still worthwhile.
func (p *Publisher) releaseClaim(ctx context.Context, eventID int64) {
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	if err := p.store.ResetClaim(ctx, eventID); err != nil {
		p.logger.Warn("reset outbox claim failed",
			"event_id", eventID, "error", err)
	}
}

// deliverOne delivers a single event to the transport and handles the
// per-event failure path (mark-failed + claim release). The success mark is
// batched by publishRound. Returns true when the transport acknowledged.
func (p *Publisher) deliverOne(ctx context.Context, ev *store.OutboxEvent) bool {
	ctx, span := observability.Tracer("jobforge.outbox").Start(ctx, "outbox.publish")
	defer span.End()
	span.SetAttributes(
		attribute.Int64("outbox.event_id", ev.EventID),
		attribute.String("outbox.event_type", ev.EventType),
		attribute.String("outbox.aggregate_id", ev.AggregateID),
		attribute.String("outbox.transport", p.transport.Name()),
		attribute.Bool("outbox.transport_durable", p.transport.Durable()),
	)

	env := NewEnvelope(ev)
	if err := p.transport.Publish(ctx, env); err != nil {
		span.RecordError(err)
		span.SetAttributes(attribute.String("outbox.ack", "failed"))
		p.observeFailure(ev.EventType, "channel_error")
		p.observeTransportFailure("channel_error")
		if markErr := p.store.MarkPublishFailed(ctx, ev.EventID); markErr != nil {
			p.logger.Warn("mark publish failed", "event_id", ev.EventID, "error", markErr)
		}
		// Release the claim so the retry path (next rounds with backoff)
		// stays available; without this the event would sit claimed until
		// the claim TTL.
		p.releaseClaim(ctx, ev.EventID)
		p.logger.Warn("publish event failed",
			"event_id", ev.EventID, "event_type", ev.EventType,
			"transport", p.transport.Name(), "error", err)
		return false
	}

	span.SetAttributes(attribute.String("outbox.ack", "delivered"))
	return true
}

// observePublishLag records outbox created_at to broker ACK (PRD v0.3 §8).
// created_at uses the PostgreSQL clock while the lag is computed on the
// local clock; negative results from container/WSL2 clock skew are clamped
// to zero (the lag is a publish-pipeline metric, not a cross-clock SLO).
func (p *Publisher) observePublishLag(ev *store.OutboxEvent) {
	if p.metrics == nil {
		return
	}
	lag := time.Since(ev.CreatedAt)
	if lag < 0 {
		lag = 0
	}
	p.metrics.EventPublishLagSeconds.Record(context.Background(), lag.Seconds(),
		metric.WithAttributes(attribute.String("transport", p.transport.Name())))
}

// observeTransportFailure increments the transport failure counter
// (PRD v0.3 §8: transport, reason labels).
func (p *Publisher) observeTransportFailure(reason string) {
	if p.metrics == nil {
		return
	}
	p.metrics.EventTransportFailuresTotal.Add(context.Background(), 1,
		metric.WithAttributes(
			attribute.String("transport", p.transport.Name()),
			attribute.String("reason", reason),
		))
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
