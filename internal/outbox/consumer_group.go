// Minimal Redis Streams consumer-group reader (PRD v0.3 AT-20 shape only).
//
// This is intentionally NOT the M3 reference consumer: there is no inbox,
// no pending-entry recovery (XAUTOCLAIM), no retry backoff and no poison
// isolation. It exists so group isolation and in-group distribution can be
// verified against the same stream the transport publishes to (AT-20); the
// full consumption protocol lands with M3 (FR-710~714).

package outbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// GroupReader reads one stream as one consumer inside one consumer group.
type GroupReader struct {
	client   *redis.Client
	stream   string
	group    string
	consumer string
}

// NewGroupReader creates a reader bound to (stream, group, consumer).
// Parse errors never echo the URL: it may contain credentials (NFR-309).
func NewGroupReader(url, stream, group, consumer string) (*GroupReader, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: invalid format (value redacted per NFR-309)")
	}
	return &GroupReader{
		client:   redis.NewClient(opts),
		stream:   stream,
		group:    group,
		consumer: consumer,
	}, nil
}

// EnsureGroup idempotently creates the consumer group starting from new
// entries ("$": tail of the stream; ">" is only valid for XREADGROUP). An
// existing group is left untouched, preserving its cursor and pending
// entries. MKSTREAM covers the empty-stream case.
func (r *GroupReader) EnsureGroup(ctx context.Context) error {
	err := r.client.XGroupCreateMkStream(ctx, r.stream, r.group, "$").Err()
	if err == nil {
		return nil
	}
	// BUSYGROUP: the group already exists — harmless.
	if isBusyGroup(err) {
		return nil
	}
	return fmt.Errorf("xgroup create %s/%s: %w", r.stream, r.group, err)
}

// ReadNext blocks up to block for new entries assigned to this consumer.
// Returns envelopes decoded from the stream fields plus the raw entry IDs
// (needed for Ack); an empty result means the block timed out.
func (r *GroupReader) ReadNext(ctx context.Context, count int, block time.Duration) (envs []*Envelope, entryIDs []string, err error) {
	res, err := r.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    r.group,
		Consumer: r.consumer,
		Streams:  []string{r.stream, ">"},
		Count:    int64(count),
		Block:    block,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("xreadgroup %s/%s: %w", r.stream, r.group, err)
	}
	for _, stream := range res {
		for _, msg := range stream.Messages {
			env, parseErr := envelopeFromStreamFields(msg.Values)
			if parseErr != nil {
				return nil, nil, fmt.Errorf("decode stream entry %s: %w", msg.ID, parseErr)
			}
			envs = append(envs, env)
			entryIDs = append(entryIDs, msg.ID)
		}
	}
	return envs, entryIDs, nil
}

// Ack acknowledges processed entries so they leave the consumer's pending
// list. Called only after the caller has durably applied the event (commit
// before ACK, FR-711 pattern).
func (r *GroupReader) Ack(ctx context.Context, entryIDs ...string) error {
	if len(entryIDs) == 0 {
		return nil
	}
	if _, err := r.client.XAck(ctx, r.stream, r.group, entryIDs...).Result(); err != nil {
		return fmt.Errorf("xack %s/%s: %w", r.stream, r.group, err)
	}
	return nil
}

// Close releases the underlying connection.
func (r *GroupReader) Close() error {
	return r.client.Close()
}

// isBusyGroup detects the "consumer group already exists" server error.
func isBusyGroup(err error) bool {
	var protoErr redis.Error
	if errors.As(err, &protoErr) {
		return len(protoErr.Error()) >= 9 && protoErr.Error()[:9] == "BUSYGROUP"
	}
	return false
}
