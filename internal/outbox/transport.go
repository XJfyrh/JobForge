// External event transport layer (PRD v0.3 §5.1, ADR-0006).
//
// The internal job-ready hint (jobforge_job_ready LISTEN/NOTIFY) is a
// separate mechanism and stays in the gateway/notify path; this layer only
// serves the external outbox transport. Transports are broker-neutral by
// contract (FR-706): the interface exposes envelopes, never broker-private
// types, so a future Kafka adapter can implement the same contract.

package outbox

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Transport names accepted by JOBFORGE_OUTBOX_TRANSPORT.
const (
	TransportNotify       = "notify"
	TransportRedisStreams = "redis_streams"
)

// DefaultChannel is the default PostgreSQL NOTIFY channel for outbox events.
// It is intentionally separate from the job-ready signal channel (ADR-0003).
const DefaultChannel = "jobforge_outbox"

// Transport is a pluggable publish target for outbox events (PRD v0.2
// FR-611, upgraded by PRD v0.3 FR-701~706). Implementations deliver
// envelopes to an external sink; returning an error keeps the event
// unpublished for retry with backoff (at-least-once).
//
// A transport must never expose broker-private types in this contract, and a
// failed durable transport must never silently downgrade to a non-durable
// delivery while still being marked successful (FR-702).
type Transport interface {
	// Name identifies the transport for metrics, logs and spans.
	Name() string

	// Durable reports whether the transport provides replayable delivery.
	// The notify transport is a compatibility/local-dev default and reports
	// false (FR-705, D2).
	Durable() bool

	// Publish delivers one envelope. Success must mean the broker
	// acknowledged the entry durably enough for the transport's own
	// contract (for Redis Streams: XADD success response).
	Publish(ctx context.Context, env *Envelope) error
}

// NotifyTransport publishes outbox events via pg_notify. Delivery is NOT
// durable: PostgreSQL restarts or subscriber disconnects lose notifications,
// which is acceptable because this transport is the v0.2-compatible default
// only (D2). The NOTIFY payload is just the event_id (decimal string): it
// stays well below the ~8000-byte NOTIFY limit, and consumers re-query
// outbox_events by event_id to obtain the full event. NOTIFY is a hint
// only, consistent with ADR-0003.
type NotifyTransport struct {
	pool    *pgxpool.Pool
	channel string
	logger  *slog.Logger
}

// NewNotifyTransport creates a NOTIFY-based transport with the given channel
// name.
func NewNotifyTransport(pool *pgxpool.Pool, channel string, logger *slog.Logger) *NotifyTransport {
	if channel == "" {
		channel = DefaultChannel
	}
	return &NotifyTransport{pool: pool, channel: channel, logger: logger}
}

// Name implements Transport.
func (t *NotifyTransport) Name() string { return TransportNotify }

// Durable implements Transport: pg_notify is fire-and-forget.
func (t *NotifyTransport) Durable() bool { return false }

// Publish implements Transport. Notification failure is an error: the event
// remains unpublished and will be retried (at-least-once).
func (t *NotifyTransport) Publish(ctx context.Context, env *Envelope) error {
	if _, err := t.pool.Exec(ctx, "select pg_notify($1, $2)", t.channel, env.EventID); err != nil {
		return fmt.Errorf("pg_notify %s: %w", t.channel, err)
	}
	return nil
}
