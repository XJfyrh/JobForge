package observability

import (
	"context"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// RecordRecovery observes a committed recovery. A nil receiver still emits the
// trace. The caller must never pass an uncommitted or duplicate candidate.
func (m *Metrics) RecordRecovery(ctx context.Context, job store.RecoveredAttempt) {
	if job.Traceparent != nil {
		ctx = ContextWithTraceParent(ctx, *job.Traceparent)
	}
	ctx, span := Tracer("jobforge.scheduler").Start(ctx, "scheduler.recover_lease")
	defer span.End()
	resolution := "cancelled"
	if job.State == domain.StateReady {
		resolution = "requeued"
	}
	span.SetAttributes(attribute.String("job_id", job.ID), attribute.String("tenant_id", job.TenantID), attribute.String("queue", job.Queue), attribute.String("type", job.Type),
		attribute.Int("attempt", job.Attempt), attribute.Int64("fencing_token", job.FencingToken), attribute.String("resolution", resolution))
	if m == nil {
		return
	}
	m.LeaseExpiredTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("queue", job.Queue), attribute.String("type", job.Type), attribute.String("resolution", resolution)))
	m.JobAttemptsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("queue", job.Queue), attribute.String("type", job.Type), attribute.String("outcome", "lease_expired")))
}

// RetryErrorCategory limits labels to known infrastructure/business categories.
// Custom Handler errors remain available in tenant-scoped attempt records.
func RetryErrorCategory(code string) string {
	switch code {
	case "TIMEOUT", "EXECUTION_ERROR", "MODEL_UNAVAILABLE", "ARTIFACT_UNAVAILABLE", "UNAVAILABLE", "INTERNAL":
		return code
	default:
		return "OTHER"
	}
}
