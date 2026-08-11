// Package eventconsumer implements the reference transactional event consumer
// defined by PRD v0.3 M3. Delivery remains at-least-once; business effects in
// PostgreSQL are deduplicated by event_id before an event is acknowledged.
package eventconsumer

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/xjfyrh/jobforge/internal/outbox"
)

const redisStreamsTransport = "redis_streams"

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// ErrPendingPayloadDeleted reports a PEL entry whose stream payload was
// removed by trimming or XDEL before the consumer committed its business work.
var ErrPendingPayloadDeleted = errors.New("pending stream payload was deleted")

// Message is one broker entry. Decode errors stay attached to the entry so a
// malformed message cannot stop delivery of later entries in the stream.
type Message struct {
	EntryID       string
	Envelope      *outbox.Envelope
	DecodeErr     error
	EventIDHint   string
	DeliveryCount int64
	Redelivered   bool
}

// Source separates broker delivery from the PostgreSQL inbox transaction.
type Source interface {
	Transport() string
	EnsureGroup(context.Context) error
	ReadNew(context.Context, time.Duration) (*Message, error)
	ClaimStale(context.Context, time.Duration, string) (*Message, string, error)
	DeliveryCount(context.Context, string) (int64, error)
	Ack(context.Context, string) error
	Quarantine(context.Context, *Message, string, int64, time.Time) error
	Close() error
}

// RedisSource implements Source with one Redis Streams consumer group. It
// owns client and closes it from Close.
type RedisSource struct {
	client       *redis.Client
	stream       string
	group        string
	consumer     string
	poisonStream string
}

// NewRedisSource binds one Redis client to a fixed stream, group, and consumer.
func NewRedisSource(
	client *redis.Client,
	stream string,
	group string,
	consumer string,
	poisonStream string,
) (*RedisSource, error) {
	if client == nil {
		return nil, fmt.Errorf("redis client is required")
	}
	if stream == "" || poisonStream == "" {
		return nil, fmt.Errorf("stream and poison stream are required")
	}
	if stream == poisonStream {
		return nil, fmt.Errorf("poison stream must differ from source stream")
	}
	if !identifierPattern.MatchString(group) || !identifierPattern.MatchString(consumer) {
		return nil, fmt.Errorf("group and consumer must be 1-128 safe characters")
	}
	return &RedisSource{
		client:       client,
		stream:       stream,
		group:        group,
		consumer:     consumer,
		poisonStream: poisonStream,
	}, nil
}

// Transport returns the bounded transport label.
func (s *RedisSource) Transport() string { return redisStreamsTransport }

// EnsureGroup starts new groups at 0-0 so a consumer created after the
// publisher still processes retained backlog. BUSYGROUP preserves an existing
// group's cursor and PEL.
func (s *RedisSource) EnsureGroup(ctx context.Context) error {
	err := s.client.XGroupCreateMkStream(ctx, s.stream, s.group, "0-0").Err()
	if err == nil || isBusyGroup(err) {
		return nil
	}
	return fmt.Errorf("create consumer group: %w", err)
}

// ReadNew takes exactly one new entry. Reading batches would put work in the
// PEL before this process is ready to run its business transaction.
func (s *RedisSource) ReadNew(ctx context.Context, block time.Duration) (*Message, error) {
	streams, err := s.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    s.group,
		Consumer: s.consumer,
		Streams:  []string{s.stream, ">"},
		Count:    1,
		Block:    block,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read new stream entry: %w", err)
	}
	return firstMessage(streams, false), nil
}

// ClaimStale recovers one entry whose owner stopped before ACK. Redis returns a
// scan cursor even when deleted PEL entries were skipped, so callers must keep
// the returned cursor for the next scan.
func (s *RedisSource) ClaimStale(
	ctx context.Context,
	minIdle time.Duration,
	start string,
) (*Message, string, error) {
	messages, next, deletedIDs, err := s.client.XAutoClaimWithDeleted(ctx, &redis.XAutoClaimArgs{
		Stream:   s.stream,
		Group:    s.group,
		Consumer: s.consumer,
		MinIdle:  minIdle,
		Start:    start,
		Count:    1,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return nil, "0-0", nil
	}
	if err != nil {
		return nil, start, fmt.Errorf("claim pending stream entry: %w", err)
	}
	if len(deletedIDs) > 0 {
		return nil, next, fatal(
			"pending_payload_deleted",
			fmt.Errorf("%w: count=%d", ErrPendingPayloadDeleted, len(deletedIDs)),
		)
	}
	if len(messages) == 0 {
		return nil, next, nil
	}
	message := decodeMessage(messages[0], true)
	count, err := s.DeliveryCount(ctx, message.EntryID)
	if err != nil {
		return nil, next, err
	}
	message.DeliveryCount = count
	return message, next, nil
}

// DeliveryCount reads one entry's current PEL delivery count.
func (s *RedisSource) DeliveryCount(ctx context.Context, entryID string) (int64, error) {
	entries, err := s.client.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: s.stream,
		Group:  s.group,
		Start:  entryID,
		End:    entryID,
		Count:  1,
	}).Result()
	if err != nil {
		return 0, fmt.Errorf("read pending delivery count: %w", err)
	}
	if len(entries) == 0 {
		return 0, fmt.Errorf("pending entry is missing")
	}
	return entries[0].RetryCount, nil
}

// Ack removes one committed or quarantined entry from the group PEL.
func (s *RedisSource) Ack(ctx context.Context, entryID string) error {
	if _, err := s.client.XAck(ctx, s.stream, s.group, entryID).Result(); err != nil {
		return fmt.Errorf("ack stream entry: %w", err)
	}
	return nil
}

// Quarantine stores bounded metadata only. It deliberately excludes the event
// payload and connection details. XADD succeeds before the source entry is
// ACKed; a crash between them may create duplicate poison records.
func (s *RedisSource) Quarantine(
	ctx context.Context,
	message *Message,
	reason string,
	deliveryCount int64,
	failedAt time.Time,
) error {
	values := map[string]any{
		"source_stream":   s.stream,
		"source_entry_id": message.EntryID,
		"consumer_group":  s.group,
		"delivery_count":  strconv.FormatInt(deliveryCount, 10),
		"reason":          sanitizeReason(reason),
		"failed_at":       failedAt.UTC().Format(time.RFC3339Nano),
	}
	if message.EventIDHint != "" {
		values["event_id"] = message.EventIDHint
	}
	if _, err := s.client.XAdd(ctx, &redis.XAddArgs{
		Stream: s.poisonStream,
		Values: values,
	}).Result(); err != nil {
		return fmt.Errorf("quarantine stream entry: %w", err)
	}
	return nil
}

// Close releases the owned Redis client.
func (s *RedisSource) Close() error { return s.client.Close() }

func firstMessage(streams []redis.XStream, redelivered bool) *Message {
	for _, stream := range streams {
		if len(stream.Messages) > 0 {
			return decodeMessage(stream.Messages[0], redelivered)
		}
	}
	return nil
}

func decodeMessage(message redis.XMessage, redelivered bool) *Message {
	envelope, err := outbox.DecodeEnvelopeFields(message.Values)
	hint, _ := message.Values["event_id"].(string)
	if parsed, parseErr := strconv.ParseInt(hint, 10, 64); parseErr != nil || parsed < 1 ||
		strconv.FormatInt(parsed, 10) != hint {
		hint = ""
	}
	result := &Message{
		EntryID:       message.ID,
		Envelope:      envelope,
		DecodeErr:     err,
		EventIDHint:   hint,
		DeliveryCount: 1,
		Redelivered:   redelivered,
	}
	return result
}

func isBusyGroup(err error) bool {
	var redisErr redis.Error
	return errors.As(err, &redisErr) && strings.HasPrefix(redisErr.Error(), "BUSYGROUP")
}

func sanitizeReason(reason string) string {
	reason = strings.ToLower(strings.TrimSpace(reason))
	if reason == "" {
		return "permanent_error"
	}
	var builder strings.Builder
	for _, r := range reason {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			builder.WriteRune(r)
		} else {
			builder.WriteByte('_')
		}
		if builder.Len() == 64 {
			break
		}
	}
	if builder.Len() == 0 {
		return "permanent_error"
	}
	return builder.String()
}
