package eventconsumer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/xjfyrh/jobforge/internal/observability"
	"github.com/xjfyrh/jobforge/internal/outbox"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var (
	// ErrAlreadyStarted reports a second Start call on one Consumer.
	ErrAlreadyStarted = errors.New("event consumer already started")
	// ErrNotStarted reports Wait called before Start.
	ErrNotStarted = errors.New("event consumer not started")
	// ErrAfterCommitHook is returned only by the deterministic test failpoint.
	ErrAfterCommitHook = errors.New("after-commit failpoint")
)

// Config bounds delivery, pending recovery, transaction, and retry behavior.
type Config struct {
	Group               string
	BlockTimeout        time.Duration
	PendingScanInterval time.Duration
	PendingMinIdle      time.Duration
	ProcessTimeout      time.Duration
	MaxDeliveries       int64
	RetryBase           time.Duration
	RetryMax            time.Duration

	// AfterCommitBeforeAck is a deterministic integration-test failpoint. A
	// non-nil error stops the consumer after PostgreSQL commit and before ACK.
	AfterCommitBeforeAck func(*Message) error
}

// Processor applies an envelope in a durable business transaction. The
// PostgreSQL InboxProcessor is the production implementation.
type Processor interface {
	Process(context.Context, *outbox.Envelope) (bool, error)
}

func (c Config) validate() error {
	if !identifierPattern.MatchString(c.Group) {
		return fmt.Errorf("consumer group must be 1-128 safe characters")
	}
	if c.BlockTimeout <= 0 || c.PendingScanInterval <= 0 || c.PendingMinIdle <= 0 ||
		c.ProcessTimeout <= 0 || c.RetryBase <= 0 || c.RetryMax <= 0 {
		return fmt.Errorf("consumer durations must be positive")
	}
	if c.PendingMinIdle <= c.ProcessTimeout {
		return fmt.Errorf("pending min idle must exceed process timeout")
	}
	if c.MaxDeliveries < 1 {
		return fmt.Errorf("consumer max deliveries must be at least 1")
	}
	if c.RetryBase > c.RetryMax {
		return fmt.Errorf("consumer retry base must not exceed retry max")
	}
	return nil
}

// Consumer owns one bounded read/process loop. Start may be called once;
// Close cancels it and closes the source, and Wait returns the loop result.
type Consumer struct {
	source    Source
	processor Processor
	config    Config
	metrics   *observability.Metrics
	logger    *slog.Logger

	mu      sync.Mutex
	started bool
	closed  bool
	cancel  context.CancelFunc
	done    chan error
}

// New validates and constructs a single-loop transactional Consumer.
func New(
	source Source,
	processor Processor,
	config Config,
	metrics *observability.Metrics,
	logger *slog.Logger,
) (*Consumer, error) {
	if source == nil {
		return nil, fmt.Errorf("event source is required")
	}
	if processor == nil {
		return nil, fmt.Errorf("inbox processor is required")
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Consumer{
		source: source, processor: processor, config: config,
		metrics: metrics, logger: logger,
	}, nil
}

// Start launches the consumer loop once and returns immediately.
func (c *Consumer) Start(parent context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return ErrAlreadyStarted
	}
	if c.closed {
		return fmt.Errorf("event consumer is closed")
	}
	ctx, cancel := context.WithCancel(parent)
	c.cancel = cancel
	c.done = make(chan error, 1)
	c.started = true
	go func() {
		c.done <- c.run(ctx)
		close(c.done)
	}()
	return nil
}

// Wait blocks until the started consumer loop exits.
func (c *Consumer) Wait() error {
	c.mu.Lock()
	if !c.started {
		c.mu.Unlock()
		return ErrNotStarted
	}
	done := c.done
	c.mu.Unlock()
	return <-done
}

// Close cancels the consumer and closes its source. It is idempotent.
func (c *Consumer) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return c.source.Close()
}

func (c *Consumer) run(ctx context.Context) error {
	retryAttempt := 0
	for {
		err := c.source.EnsureGroup(ctx)
		if err == nil {
			break
		}
		if c.stopOnFatal(ctx, err) {
			return err
		}
		if !c.retry(ctx, "ensure_group", err, &retryAttempt) {
			return nil
		}
	}
	retryAttempt = 0

	pendingCursor := "0-0"
	nextPendingScan := time.Now()
	claimedLast := false
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		if !claimedLast && !time.Now().Before(nextPendingScan) {
			message, next, err := c.source.ClaimStale(
				ctx, c.config.PendingMinIdle, pendingCursor,
			)
			if err != nil {
				if c.stopOnFatal(ctx, err) {
					return err
				}
				if !c.retry(ctx, "claim_pending", err, &retryAttempt) {
					return nil
				}
				continue
			}
			pendingCursor = next
			if message != nil {
				if err := c.handle(ctx, message); err != nil {
					if errors.Is(err, ErrAfterCommitHook) {
						return err
					}
					if c.stopOnFatal(ctx, err) {
						return err
					}
					if !c.retry(ctx, "process_pending", err, &retryAttempt) {
						return nil
					}
				} else {
					retryAttempt = 0
				}
				claimedLast = true
				if pendingCursor == "0-0" {
					nextPendingScan = time.Now().Add(c.config.PendingScanInterval)
				}
				continue
			}
			if pendingCursor == "0-0" {
				nextPendingScan = time.Now().Add(c.config.PendingScanInterval)
			}
		}

		message, err := c.source.ReadNew(ctx, c.config.BlockTimeout)
		if err != nil {
			if c.stopOnFatal(ctx, err) {
				return err
			}
			if !c.retry(ctx, "read_new", err, &retryAttempt) {
				return nil
			}
			claimedLast = false
			continue
		}
		claimedLast = false
		if message == nil {
			continue
		}
		if err := c.handle(ctx, message); err != nil {
			if errors.Is(err, ErrAfterCommitHook) {
				return err
			}
			if c.stopOnFatal(ctx, err) {
				return err
			}
			if !c.retry(ctx, "process_new", err, &retryAttempt) {
				return nil
			}
			continue
		}
		retryAttempt = 0
	}
}

func (c *Consumer) handle(ctx context.Context, message *Message) error {
	if message.Envelope != nil && message.Envelope.Traceparent != "" {
		ctx = observability.ContextWithTraceParent(ctx, message.Envelope.Traceparent)
	}
	ctx, span := observability.Tracer("jobforge.eventconsumer").Start(ctx, "event.consume")
	defer span.End()
	span.SetAttributes(
		attribute.String("messaging.system", c.source.Transport()),
		attribute.String("messaging.consumer.group.name", c.config.Group),
		attribute.Bool("messaging.message.redelivered", message.Redelivered),
	)

	if message.Redelivered && c.metrics != nil {
		c.metrics.EventRedeliveriesTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("transport", c.source.Transport()),
			attribute.String("consumer_group", c.config.Group),
		))
	}

	if message.DecodeErr != nil {
		return c.handlePermanent(ctx, span, message, "invalid_envelope")
	}

	processCtx, cancel := context.WithTimeout(ctx, c.config.ProcessTimeout)
	duplicate, err := c.processor.Process(processCtx, message.Envelope)
	cancel()
	if err != nil {
		if reason, isFatal := fatalReason(err); isFatal {
			span.RecordError(errors.New("fatal event consumer failure"))
			span.SetStatus(codes.Error, "fatal consumer failure")
			span.SetAttributes(
				attribute.String("jobforge.event.consume.result", "fatal"),
				attribute.String("jobforge.event.consume.failure_reason", reason),
			)
			return err
		}
		if reason, permanent := permanentReason(err); permanent {
			return c.handlePermanent(ctx, span, message, reason)
		}
		span.RecordError(errors.New("event processing failed"))
		span.SetStatus(codes.Error, "transient processing failure")
		span.SetAttributes(attribute.String("jobforge.event.consume.result", "pending"))
		return err
	}
	span.SetAttributes(attribute.Bool("jobforge.consumer.inbox_duplicate", duplicate))

	if c.config.AfterCommitBeforeAck != nil {
		if err := c.config.AfterCommitBeforeAck(message); err != nil {
			span.SetStatus(codes.Error, "after-commit failpoint")
			span.SetAttributes(attribute.String("jobforge.event.consume.result", "unacked"))
			return fmt.Errorf("%w: %v", ErrAfterCommitHook, err)
		}
	}
	if err := c.source.Ack(ctx, message.EntryID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "ack failure")
		span.SetAttributes(attribute.String("jobforge.event.consume.result", "unacked"))
		return err
	}
	span.SetAttributes(attribute.String("jobforge.event.consume.result", "acked"))
	return nil
}

func (c *Consumer) stopOnFatal(ctx context.Context, err error) bool {
	reason, ok := fatalReason(err)
	if !ok {
		return false
	}
	if reason == "pending_payload_deleted" && c.metrics != nil {
		c.metrics.EventTransportFailuresTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("transport", c.source.Transport()),
			attribute.String("reason", reason),
		))
	}
	return true
}

func (c *Consumer) handlePermanent(
	ctx context.Context,
	span trace.Span,
	message *Message,
	reason string,
) error {
	deliveryCount := message.DeliveryCount
	if deliveryCount < 1 {
		var err error
		deliveryCount, err = c.source.DeliveryCount(ctx, message.EntryID)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "delivery count failure")
			return err
		}
	}
	span.SetAttributes(attribute.Int64("messaging.message.delivery_count", deliveryCount))
	if deliveryCount < c.config.MaxDeliveries {
		span.SetAttributes(attribute.String("jobforge.event.consume.result", "pending_permanent"))
		c.logger.WarnContext(ctx, "event rejected; awaiting redelivery",
			"reason", sanitizeReason(reason),
			"delivery_count", deliveryCount,
		)
		return nil
	}

	if err := c.source.Quarantine(
		ctx, message, reason, deliveryCount, time.Now(),
	); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "poison write failure")
		span.SetAttributes(attribute.String("jobforge.event.consume.result", "pending"))
		return err
	}
	if err := c.source.Ack(ctx, message.EntryID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "poison ack failure")
		span.SetAttributes(attribute.String("jobforge.event.consume.result", "poison_unacked"))
		return err
	}
	span.SetAttributes(attribute.String("jobforge.event.consume.result", "poison_acked"))
	return nil
}

func (c *Consumer) retry(
	ctx context.Context,
	operation string,
	err error,
	attempt *int,
) bool {
	if ctx.Err() != nil {
		return false
	}
	delay := retryDelay(*attempt, c.config.RetryBase, c.config.RetryMax)
	if *attempt < 62 {
		*attempt = *attempt + 1
	}
	c.logger.WarnContext(ctx, "event consumer transient failure",
		"operation", operation,
		"retry_in", delay,
		"error_kind", transientErrorKind(err),
	)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func transientErrorKind(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return "infrastructure"
}

func retryDelay(attempt int, base time.Duration, maximum time.Duration) time.Duration {
	delay := base
	for i := 0; i < attempt && delay < maximum/2; i++ {
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}
