package observability

import (
	"context"
	"fmt"

	promclient "github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// Metrics holds all JobForge Prometheus instruments defined in PRD 12.1.
// High-cardinality fields (job_id, trace_id) are never used as labels.
type Metrics struct {
	// JobsSubmittedTotal counts submitted jobs (PRD 12.1).
	// Labels: tenant, queue, type.
	JobsSubmittedTotal metric.Int64Counter

	// JobAttemptsTotal counts execution attempts (PRD 12.1).
	// Labels: queue, type, outcome.
	JobAttemptsTotal metric.Int64Counter

	// QueueDepth tracks pending jobs per queue (PRD 12.1).
	// Labels: tenant, queue, state.
	QueueDepth metric.Int64Gauge

	// JobLatencySeconds records execution duration (PRD 12.1).
	// Labels: queue, type, outcome.
	JobLatencySeconds metric.Float64Histogram

	// ClaimDurationSeconds records claim transaction duration (PRD 12.1).
	// Labels: queue.
	ClaimDurationSeconds metric.Float64Histogram

	// RetriesTotal counts retry events (PRD 12.1).
	// Labels: queue, error_code.
	RetriesTotal metric.Int64Counter

	// DLQTotal counts jobs entering dead-letter queue (PRD 12.1).
	// Labels: queue, type.
	DLQTotal metric.Int64Counter

	// LeaseExpiredTotal counts lease expiry recoveries (PRD 12.1).
	// Labels: queue.
	LeaseExpiredTotal metric.Int64Counter

	// WorkersActive tracks registered workers (PRD 12.1).
	// Labels: version, status.
	WorkersActive metric.Int64Gauge

	// TenantThrottledTotal counts tenant throttling events (PRD 12.1).
	// Labels: tenant, reason.
	TenantThrottledTotal metric.Int64Counter

	// OutboxPending tracks unpublished outbox events (PRD v0.2 §8).
	// Labels: none.
	OutboxPending metric.Int64Gauge

	// OutboxPublishedTotal counts successfully published outbox events
	// (PRD v0.2 §8). Labels: event_type.
	OutboxPublishedTotal metric.Int64Counter

	// OutboxPublishFailuresTotal counts outbox publish failures
	// (PRD v0.2 §8). Labels: event_type, reason.
	OutboxPublishFailuresTotal metric.Int64Counter

	// QuotaReservationConflictsTotal counts claim candidates skipped because
	// their tenant's atomic slot reservation hit the hard cap (PRD v0.3 §8).
	// Labels: none.
	QuotaReservationConflictsTotal metric.Int64Counter

	// QuotaCounterDrift records the total absolute drift found by the last
	// quota reconcile between tenant_quota_counters and the jobs aggregation
	// (PRD v0.3 §8). Labels: none.
	QuotaCounterDrift metric.Int64Gauge

	// EventPublishLagSeconds measures outbox created_at to broker ACK
	// (PRD v0.3 §8). Labels: transport.
	EventPublishLagSeconds metric.Float64Histogram

	// EventTransportFailuresTotal counts broker/serialization/ACK failures
	// on the external event transport (PRD v0.3 §8). Labels: transport, reason.
	EventTransportFailuresTotal metric.Int64Counter

	// EventRedeliveriesTotal counts entries recovered from a broker pending
	// list. Labels: transport, consumer_group.
	EventRedeliveriesTotal metric.Int64Counter

	// ConsumerInboxDuplicatesTotal counts events absorbed by the PostgreSQL
	// inbox after a previous transaction committed. Labels: consumer_group.
	ConsumerInboxDuplicatesTotal metric.Int64Counter
}

// SetupMetrics initializes the global MeterProvider with a Prometheus
// exporter. The exporter registers metrics with the provided prometheus
// registerer (or the default if nil). Returns the Metrics instruments
// and a shutdown function.
func SetupMetrics(_ context.Context, reg promclient.Registerer) (*Metrics, func(context.Context) error, error) {
	if reg == nil {
		reg = promclient.DefaultRegisterer
	}

	exporter, err := otelprom.New(otelprom.WithRegisterer(reg))
	if err != nil {
		return nil, nil, fmt.Errorf("create prometheus exporter: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(exporter),
	)
	otel.SetMeterProvider(mp)

	meter := mp.Meter("jobforge")

	m := &Metrics{}

	m.JobsSubmittedTotal, err = meter.Int64Counter("jobforge_jobs_submitted_total",
		metric.WithDescription("Total number of jobs submitted"))
	if err != nil {
		return nil, nil, fmt.Errorf("create jobs_submitted_total: %w", err)
	}

	m.JobAttemptsTotal, err = meter.Int64Counter("jobforge_job_attempts_total",
		metric.WithDescription("Total number of job execution attempts"))
	if err != nil {
		return nil, nil, fmt.Errorf("create job_attempts_total: %w", err)
	}

	m.QueueDepth, err = meter.Int64Gauge("jobforge_queue_depth",
		metric.WithDescription("Current number of pending jobs in queue"))
	if err != nil {
		return nil, nil, fmt.Errorf("create queue_depth: %w", err)
	}

	m.JobLatencySeconds, err = meter.Float64Histogram("jobforge_job_latency_seconds",
		metric.WithDescription("Job execution duration in seconds"),
		metric.WithExplicitBucketBoundaries(0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300))
	if err != nil {
		return nil, nil, fmt.Errorf("create job_latency_seconds: %w", err)
	}

	m.ClaimDurationSeconds, err = meter.Float64Histogram("jobforge_claim_duration_seconds",
		metric.WithDescription("Claim transaction duration in seconds"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1))
	if err != nil {
		return nil, nil, fmt.Errorf("create claim_duration_seconds: %w", err)
	}

	m.RetriesTotal, err = meter.Int64Counter("jobforge_retries_total",
		metric.WithDescription("Total number of job retries"))
	if err != nil {
		return nil, nil, fmt.Errorf("create retries_total: %w", err)
	}

	m.DLQTotal, err = meter.Int64Counter("jobforge_dlq_total",
		metric.WithDescription("Total number of jobs entering dead-letter queue"))
	if err != nil {
		return nil, nil, fmt.Errorf("create dlq_total: %w", err)
	}

	m.LeaseExpiredTotal, err = meter.Int64Counter("jobforge_lease_expired_total",
		metric.WithDescription("Total number of lease expiry recoveries"))
	if err != nil {
		return nil, nil, fmt.Errorf("create lease_expired_total: %w", err)
	}

	m.WorkersActive, err = meter.Int64Gauge("jobforge_workers_active",
		metric.WithDescription("Number of currently active workers"))
	if err != nil {
		return nil, nil, fmt.Errorf("create workers_active: %w", err)
	}

	m.TenantThrottledTotal, err = meter.Int64Counter("jobforge_tenant_throttled_total",
		metric.WithDescription("Total number of tenant throttling events"))
	if err != nil {
		return nil, nil, fmt.Errorf("create tenant_throttled_total: %w", err)
	}

	m.OutboxPending, err = meter.Int64Gauge("jobforge_outbox_pending",
		metric.WithDescription("Number of unpublished outbox events (sampled)"))
	if err != nil {
		return nil, nil, fmt.Errorf("create outbox_pending: %w", err)
	}

	m.OutboxPublishedTotal, err = meter.Int64Counter("jobforge_outbox_published_total",
		metric.WithDescription("Total number of outbox events successfully published"))
	if err != nil {
		return nil, nil, fmt.Errorf("create outbox_published_total: %w", err)
	}

	m.OutboxPublishFailuresTotal, err = meter.Int64Counter("jobforge_outbox_publish_failures_total",
		metric.WithDescription("Total number of outbox publish failures"))
	if err != nil {
		return nil, nil, fmt.Errorf("create outbox_publish_failures_total: %w", err)
	}

	m.QuotaReservationConflictsTotal, err = meter.Int64Counter("jobforge_quota_reservation_conflicts_total",
		metric.WithDescription("Total number of claim candidates skipped because the tenant quota was full"))
	if err != nil {
		return nil, nil, fmt.Errorf("create quota_reservation_conflicts_total: %w", err)
	}

	m.QuotaCounterDrift, err = meter.Int64Gauge("jobforge_quota_counter_drift",
		metric.WithDescription("Absolute quota counter drift found by the last reconcile"))
	if err != nil {
		return nil, nil, fmt.Errorf("create quota_counter_drift: %w", err)
	}

	m.EventPublishLagSeconds, err = meter.Float64Histogram("jobforge_event_publish_lag_seconds",
		metric.WithDescription("Outbox event age at broker ACK (created_at to publish success)"),
		metric.WithExplicitBucketBoundaries(0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60))
	if err != nil {
		return nil, nil, fmt.Errorf("create event_publish_lag_seconds: %w", err)
	}

	m.EventTransportFailuresTotal, err = meter.Int64Counter("jobforge_event_transport_failures_total",
		metric.WithDescription("Total number of external event transport failures"))
	if err != nil {
		return nil, nil, fmt.Errorf("create event_transport_failures_total: %w", err)
	}

	m.EventRedeliveriesTotal, err = meter.Int64Counter("jobforge_event_redeliveries_total",
		metric.WithDescription("Total number of event entries recovered from pending delivery"))
	if err != nil {
		return nil, nil, fmt.Errorf("create event_redeliveries_total: %w", err)
	}

	m.ConsumerInboxDuplicatesTotal, err = meter.Int64Counter("jobforge_consumer_inbox_duplicates_total",
		metric.WithDescription("Total number of event duplicates absorbed by the consumer inbox"))
	if err != nil {
		return nil, nil, fmt.Errorf("create consumer_inbox_duplicates_total: %w", err)
	}

	return m, mp.Shutdown, nil
}
