package demo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/xjfyrh/jobforge/internal/observability"
	"github.com/xjfyrh/jobforge/internal/worker"
)

const maxPostEffectDelayMS = 60_000

// EffectResult reports the stable business result and whether this call
// atomically created it. Applied=false means a previous delivery committed the
// same job ID first.
type EffectResult struct {
	ResultRef string
	Applied   bool
}

// EffectStore persists the business-side idempotency boundary for
// demo.idempotent_effect. Implementations must be safe for concurrent use.
type EffectStore interface {
	Apply(ctx context.Context, jobID string) (EffectResult, error)
}

// PostgresEffectStore stores demo effects independently from the jobs state
// machine. PostgreSQL is used here as a concrete demo business system; the
// table is not a JobForge task-state fact source.
type PostgresEffectStore struct {
	pool *pgxpool.Pool
}

// NewPostgresEffectStore creates a persistent demo effect store.
func NewPostgresEffectStore(pool *pgxpool.Pool) *PostgresEffectStore {
	return &PostgresEffectStore{pool: pool}
}

// Apply atomically creates one effect per job ID. A conflict is followed by a
// separate read so PostgreSQL READ COMMITTED obtains a fresh snapshot after a
// concurrent inserter commits.
func (s *PostgresEffectStore) Apply(ctx context.Context, jobID string) (EffectResult, error) {
	if s == nil || s.pool == nil {
		return EffectResult{}, fmt.Errorf("demo effect store is not configured")
	}

	resultRef := "effect:" + jobID
	var insertedRef string
	err := s.pool.QueryRow(ctx, `
		insert into demo_idempotent_effects (job_id, result_ref)
		values ($1, $2)
		on conflict (job_id) do nothing
		returning result_ref`, jobID, resultRef).Scan(&insertedRef)
	if err == nil {
		return EffectResult{ResultRef: insertedRef, Applied: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return EffectResult{}, fmt.Errorf("insert demo idempotent effect: %w", err)
	}

	var existingRef string
	if err := s.pool.QueryRow(ctx,
		"select result_ref from demo_idempotent_effects where job_id = $1",
		jobID,
	).Scan(&existingRef); err != nil {
		return EffectResult{}, fmt.Errorf("read demo idempotent effect: %w", err)
	}
	return EffectResult{ResultRef: existingRef, Applied: false}, nil
}

type idempotentEffectPayload struct {
	PostEffectDelayMS int `json:"post_effect_delay_ms"`
}

// IdempotentEffectHandler applies a persistent side effect once per job ID.
// Duplicate at-least-once deliveries return the original result reference.
type IdempotentEffectHandler struct {
	store   EffectStore
	logger  *slog.Logger
	metrics *observability.Metrics
}

// NewIdempotentEffectHandler creates a persistent idempotent demo Handler.
func NewIdempotentEffectHandler(
	store EffectStore,
	logger *slog.Logger,
	metrics *observability.Metrics,
) *IdempotentEffectHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &IdempotentEffectHandler{store: store, logger: logger, metrics: metrics}
}

// Execute implements worker.Handler.
func (h *IdempotentEffectHandler) Execute(ctx context.Context, job *worker.ClaimedJob) (string, error) {
	started := time.Now()
	if job == nil || job.ID == "" {
		h.record(ctx, "", "failed", started)
		return "", fmt.Errorf("job id is required")
	}

	var payload idempotentEffectPayload
	if len(job.Payload) > 0 {
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			h.record(ctx, job.ID, "failed", started)
			return "", fmt.Errorf("invalid idempotent effect payload: %w", err)
		}
	}
	if payload.PostEffectDelayMS < 0 || payload.PostEffectDelayMS > maxPostEffectDelayMS {
		h.record(ctx, job.ID, "failed", started)
		return "", fmt.Errorf("post_effect_delay_ms must be between 0 and 60000")
	}

	if h.store == nil {
		h.record(ctx, job.ID, "failed", started)
		return "", worker.NewRetryableError(fmt.Errorf("demo effect store is not configured"))
	}
	result, err := h.store.Apply(ctx, job.ID)
	if err != nil {
		h.record(ctx, job.ID, "failed", started)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		return "", worker.NewRetryableError(err)
	}

	outcome := "deduplicated"
	if result.Applied {
		outcome = "applied"
	}
	h.record(ctx, job.ID, outcome, started)

	if result.Applied && payload.PostEffectDelayMS > 0 {
		timer := time.NewTimer(time.Duration(payload.PostEffectDelayMS) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	return result.ResultRef, nil
}

func (h *IdempotentEffectHandler) record(
	ctx context.Context,
	jobID string,
	outcome string,
	started time.Time,
) {
	if h.metrics != nil {
		h.metrics.DemoIdempotentEffectsTotal.Add(ctx, 1,
			metric.WithAttributes(attribute.String("outcome", outcome)))
	}
	h.logger.Info("demo idempotent effect",
		"job_id", jobID,
		"effect_outcome", outcome,
		"duration", time.Since(started),
	)
}
