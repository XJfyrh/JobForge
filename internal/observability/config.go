// Package observability provides OpenTelemetry tracing, Prometheus metrics
// and pprof diagnostics for the JobForge platform. It encapsulates all
// observability SDK initialization so that transport and infrastructure
// layers consume a single setup entry point.
//
// Design constraints (PRD 12, code-standards):
//   - domain layer must not depend on this package;
//   - job_id and trace_id are never used as metric labels;
//   - pprof and metrics endpoints bind to localhost only (PRD 11.4).
package observability

// Config holds observability tuning parameters loaded from environment.
type Config struct {
	// ServiceName is the OTel resource service.name attribute.
	ServiceName string

	// ServiceVersion is the OTel resource service.version attribute.
	ServiceVersion string

	// Environment is the deployment environment (dev, staging, production).
	Environment string

	// ExporterType selects the trace exporter: "stdout" or "none".
	// P0 defaults to stdout (zero external dependencies). OTLP can be
	// added later without changing call sites.
	ExporterType string

	// SampleRatio is the trace sampling ratio in [0.0, 1.0].
	// 1.0 = sample all traces (default for development).
	SampleRatio float64

	// MetricsAddr is the listen address for the Prometheus /metrics and
	// pprof debug HTTP server. Defaults to 127.0.0.1:6060 (localhost only).
	MetricsAddr string
}

// DefaultConfig returns development-safe defaults.
func DefaultConfig() Config {
	return Config{
		ServiceName:    "jobforge",
		ServiceVersion: "0.1.0",
		Environment:    "development",
		ExporterType:   "stdout",
		SampleRatio:    1.0,
		MetricsAddr:    "127.0.0.1:6060",
	}
}
