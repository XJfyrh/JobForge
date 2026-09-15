package observability

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Tracer returns the named tracer from the global TracerProvider.
// Callers use this to create spans without importing OTel SDK directly.
func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}

// SetupTracing initializes the global TracerProvider with the configured
// exporter and sampling ratio. It returns a shutdown function that must be
// called on process exit to flush buffered spans.
//
// Exporter types:
//   - "stdout": writes spans as JSON to stderr (development default).
//   - "none": disables tracing (noop provider).
//   - "otlp": bounded asynchronous OTLP/HTTP, configured by OTEL_* variables.
func SetupTracing(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	// Exporter diagnostics must not expose endpoint credentials or remote
	// response bodies. Rate limiting also bounds noise during an outage.
	var lastDiagnostic atomic.Int64
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {
		now := time.Now().Unix()
		previous := lastDiagnostic.Load()
		if now-previous >= 30 && lastDiagnostic.CompareAndSwap(previous, now) {
			slog.Warn("telemetry export unavailable")
		}
	}))
	if cfg.ExporterType == "none" {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return func(context.Context) error { return nil }, nil
	}

	var exporter sdktrace.SpanExporter
	switch cfg.ExporterType {
	case "stdout", "":
		exporter, err = stdouttrace.New(
			stdouttrace.WithWriter(os.Stderr),
			stdouttrace.WithPrettyPrint(),
		)
	case "otlp":
		exporter, err = otlptracehttp.New(ctx, otlptracehttp.WithTimeout(2*time.Second),
			otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}))
	default:
		return nil, fmt.Errorf("unsupported trace exporter type: %q", cfg.ExporterType)
	}
	if err != nil {
		return nil, fmt.Errorf("create trace exporter failed")
	}

	res, err := buildResource(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("build otel resource: %w", err)
	}

	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter, sdktrace.WithMaxQueueSize(2048), sdktrace.WithMaxExportBatchSize(256),
			sdktrace.WithBatchTimeout(time.Second), sdktrace.WithExportTimeout(3*time.Second)),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)

	otel.SetTracerProvider(tp)

	return tp.Shutdown, nil
}

// SetupTracingWithWriter is like SetupTracing but writes spans to the given
// writer. Used in integration tests to capture spans in-memory.
func SetupTracingWithWriter(ctx context.Context, cfg Config, w io.Writer) (shutdown func(context.Context) error, err error) {
	exporter, err := stdouttrace.New(
		stdouttrace.WithWriter(w),
		stdouttrace.WithoutTimestamps(),
	)
	if err != nil {
		return nil, fmt.Errorf("create trace exporter: %w", err)
	}

	res, err := buildResource(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("build otel resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return tp.Shutdown, nil
}

func buildResource(_ context.Context, cfg Config) (*resource.Resource, error) {
	return resource.NewSchemaless(
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.ServiceVersion),
	), nil
}
