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

	return m, mp.Shutdown, nil
}
