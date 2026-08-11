package eventconsumer

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xjfyrh/jobforge/internal/observability"
	"github.com/xjfyrh/jobforge/internal/outbox"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

// Handler is a pre-registered business effect executed inside the same
// PostgreSQL transaction as the inbox insert. It is not a dynamic code hook.
type Handler interface {
	Handle(context.Context, pgx.Tx, *outbox.Envelope) error
}

type permanentError struct {
	reason string
	err    error
}

var (
	// ErrConsumerGroupMismatch prevents one inbox schema from silently serving
	// two logical consumer groups.
	ErrConsumerGroupMismatch = errors.New("consumer inbox is bound to another group")
	// ErrInboxMetadataMismatch reports a duplicate event_id whose immutable
	// aggregate identity differs from the first committed delivery.
	ErrInboxMetadataMismatch = errors.New("consumer inbox event metadata mismatch")
)

type fatalError struct {
	reason string
	err    error
}

func (e *fatalError) Error() string { return e.err.Error() }
func (e *fatalError) Unwrap() error { return e.err }

func fatal(reason string, err error) error {
	return &fatalError{reason: sanitizeReason(reason), err: err}
}

func fatalReason(err error) (string, bool) {
	var target *fatalError
	if errors.As(err, &target) {
		return target.reason, true
	}
	return "", false
}

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// Permanent marks a handler rejection that is safe to count toward poison
// isolation. Database, Redis, context, and unmarked errors remain transient.
func Permanent(reason string, err error) error {
	if err == nil {
		err = errors.New("permanent handler error")
	}
	return &permanentError{reason: sanitizeReason(reason), err: err}
}

func permanentReason(err error) (string, bool) {
	var target *permanentError
	if errors.As(err, &target) {
		return target.reason, true
	}
	return "", false
}

// InboxProcessor atomically records event_id and applies its business effect.
// ACK is intentionally outside this type and is only attempted after Commit.
type InboxProcessor struct {
	pool    *pgxpool.Pool
	group   string
	handler Handler
	metrics *observability.Metrics

	bindingMu sync.Mutex
	bound     bool
}

// EnsureBinding atomically binds the reference inbox schema to one logical
// consumer group. It is safe for concurrent startup by multiple instances of
// the same group; a different group fails closed before any event is ACKed.
func (p *InboxProcessor) EnsureBinding(ctx context.Context) error {
	p.bindingMu.Lock()
	defer p.bindingMu.Unlock()
	if p.bound {
		return nil
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin consumer inbox binding: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO consumer_inbox_binding (binding_id, consumer_group)
		VALUES (1, $1)
		ON CONFLICT (binding_id) DO NOTHING
	`, p.group); err != nil {
		return fmt.Errorf("bind consumer inbox: %w", err)
	}
	var existingGroup string
	if err := tx.QueryRow(ctx, `
		SELECT consumer_group
		FROM consumer_inbox_binding
		WHERE binding_id = 1
	`).Scan(&existingGroup); err != nil {
		return fmt.Errorf("read consumer inbox binding: %w", err)
	}
	if existingGroup != p.group {
		return fatal("consumer_group_mismatch", ErrConsumerGroupMismatch)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit consumer inbox binding: %w", err)
	}
	p.bound = true
	return nil
}

// NewInboxProcessor constructs a PostgreSQL transaction processor for group.
func NewInboxProcessor(
	pool *pgxpool.Pool,
	group string,
	handler Handler,
	metrics *observability.Metrics,
) (*InboxProcessor, error) {
	if pool == nil {
		return nil, fmt.Errorf("PostgreSQL pool is required")
	}
	if !identifierPattern.MatchString(group) {
		return nil, fmt.Errorf("consumer group must be 1-128 safe characters")
	}
	if handler == nil {
		return nil, fmt.Errorf("handler is required")
	}
	return &InboxProcessor{pool: pool, group: group, handler: handler, metrics: metrics}, nil
}

// Process returns true when the inbox already contained event_id.
func (p *InboxProcessor) Process(ctx context.Context, envelope *outbox.Envelope) (bool, error) {
	ctx, span := observability.Tracer("jobforge.eventconsumer").Start(ctx, "event.process")
	defer span.End()
	if err := p.EnsureBinding(ctx); err != nil {
		span.RecordError(errors.New("consumer inbox binding failed"))
		span.SetStatus(codes.Error, "bind consumer inbox")
		return false, err
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "begin transaction")
		return false, fmt.Errorf("begin inbox transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	command, err := tx.Exec(ctx, `
		INSERT INTO consumer_inbox (
			event_id, consumer_group, aggregate_id, aggregate_version
		) VALUES ($1, $2, $3, $4)
		ON CONFLICT (event_id) DO NOTHING
	`, envelope.EventID, p.group, envelope.AggregateID, envelope.AggregateVersion)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "insert inbox")
		return false, fmt.Errorf("insert consumer inbox: %w", err)
	}

	duplicate := command.RowsAffected() == 0
	span.SetAttributes(attribute.Bool("jobforge.consumer.inbox_duplicate", duplicate))
	if duplicate {
		var existingGroup string
		var existingAggregateID string
		var existingAggregateVersion int64
		if err := tx.QueryRow(ctx, `
			SELECT consumer_group, aggregate_id, aggregate_version
			FROM consumer_inbox
			WHERE event_id = $1
		`, envelope.EventID).Scan(
			&existingGroup, &existingAggregateID, &existingAggregateVersion,
		); err != nil {
			return false, fmt.Errorf("read duplicate consumer inbox row: %w", err)
		}
		if existingGroup != p.group {
			return false, fatal("consumer_group_mismatch", ErrConsumerGroupMismatch)
		}
		if existingAggregateID != envelope.AggregateID ||
			existingAggregateVersion != envelope.AggregateVersion {
			return false, fatal("inbox_metadata_mismatch", ErrInboxMetadataMismatch)
		}
	} else {
		if err := p.handler.Handle(ctx, tx, envelope); err != nil {
			// Handler errors are opaque because pre-registered business logic may
			// include sensitive values in its error text.
			span.RecordError(errors.New("event handler failed"))
			span.SetStatus(codes.Error, "apply handler")
			return false, fmt.Errorf("apply event handler: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "commit transaction")
		return false, fmt.Errorf("commit inbox transaction: %w", err)
	}
	if duplicate && p.metrics != nil {
		p.metrics.ConsumerInboxDuplicatesTotal.Add(ctx, 1,
			metric.WithAttributes(attribute.String("consumer_group", p.group)))
	}
	return duplicate, nil
}

// DemoEffectHandler is the fixed reference handler used by jobforge consumer.
// event_id is deliberately not unique in consumer_demo_effects; only the inbox
// protocol prevents duplicate effects.
type DemoEffectHandler struct{}

// Handle records the reference business effect in the active inbox transaction.
func (DemoEffectHandler) Handle(ctx context.Context, tx pgx.Tx, envelope *outbox.Envelope) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO consumer_demo_effects (
			event_id, aggregate_id, aggregate_version, event_type
		) VALUES ($1, $2, $3, $4)
	`, envelope.EventID, envelope.AggregateID, envelope.AggregateVersion, envelope.EventType)
	if err != nil {
		return fmt.Errorf("insert demo effect: %w", err)
	}
	return nil
}
