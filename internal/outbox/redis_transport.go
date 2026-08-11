// Redis Streams transport (PRD v0.3 FR-702/704, ADR-0006 §2).
//
// The adapter publishes envelope v1 to a fixed, configurable stream key.
// Success means XADD returned a success response; the publisher marks the
// outbox row published only afterwards. A crash in that window duplicates
// the entry, which consumers absorb via the event_id dedup key (AT-18).
// The adapter never downgrades to another transport on failure.

package outbox

import (
	"context"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// RedisStreamsTransport publishes envelopes to a Redis Stream via XADD.
// It implements the broker-neutral Transport contract: no Redis-private
// types appear in the interface, only in this implementation (FR-706).
type RedisStreamsTransport struct {
	client *redis.Client
	stream string
	maxLen int64
}

// NewRedisStreamsTransport connects to the Redis at url. The connection is
// lazy: publish attempts fail (and the outbox backs off) until Redis is
// reachable, per NFR-303. The url must never be logged (NFR-309): parse
// errors are reported without echoing the input, because url.Parse echoes
// the raw value and it may contain credentials.
func NewRedisStreamsTransport(url, streamKey string, maxLen int64) (*RedisStreamsTransport, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: invalid JOBFORGE_REDIS_URL (value redacted per NFR-309)")
	}
	if streamKey == "" {
		streamKey = "jobforge:events"
	}
	return &RedisStreamsTransport{
		client: redis.NewClient(opts),
		stream: streamKey,
		maxLen: maxLen,
	}, nil
}

// Name implements Transport.
func (t *RedisStreamsTransport) Name() string { return TransportRedisStreams }

// Durable implements Transport: with AOF enabled on the Redis side the
// stream survives broker restarts (ADR-0006 §2).
func (t *RedisStreamsTransport) Durable() bool { return true }

// StreamKey exposes the configured stream key (used by tooling and tests).
func (t *RedisStreamsTransport) StreamKey() string { return t.stream }

// Publish implements Transport: XADD the envelope as stream fields. Only a
// successful XADD response counts as published (FR-702).
func (t *RedisStreamsTransport) Publish(ctx context.Context, env *Envelope) error {
	args := &redis.XAddArgs{
		Stream: t.stream,
		Values: map[string]any{
			"schema_version":    strconv.Itoa(env.SchemaVersion),
			"event_id":          env.EventID,
			"aggregate_id":      env.AggregateID,
			"aggregate_version": strconv.FormatInt(env.AggregateVersion, 10),
			"event_type":        env.EventType,
			"occurred_at":       env.OccurredAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
			"payload":           string(env.Payload),
			"traceparent":       env.Traceparent,
		},
	}
	if t.maxLen > 0 {
		// Approximate trimming keeps XADD O(1) amortized; the ~ tolerance is
		// acceptable because consumers never rely on entry IDs (FR-704).
		args.MaxLen = t.maxLen
		args.Approx = true
	}
	if _, err := t.client.XAdd(ctx, args).Result(); err != nil {
		return fmt.Errorf("redis XADD %s: %w", t.stream, err)
	}
	return nil
}

// Ping checks broker reachability (health checks; never gates job readiness).
func (t *RedisStreamsTransport) Ping(ctx context.Context) error {
	return t.client.Ping(ctx).Err()
}

// Close releases the underlying connection.
func (t *RedisStreamsTransport) Close() error {
	return t.client.Close()
}
