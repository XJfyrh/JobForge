package observability

import (
	"context"
	"fmt"
	"io"
	"os"

	"go.opentelemetry.io/otel"
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
func SetupTracing(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
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
	default:
		return nil, fmt.Errorf("unsupported trace exporter type: %q", cfg.ExporterType)
	}
	if err != nil {
		return nil, fmt.Errorf("create trace exporter: %w", err)
	}

	res, err := buildResource(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("build otel resource: %w", err)
	}

	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)

	otel.SetTracerProvider(tp)
	// W3C TraceContext + Baggage propagation (standard for cross-process trace).
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

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
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp.Shutdown, nil
}

func buildResource(_ context.Context, cfg Config) (*resource.Resource, error) {
	return resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.ServiceVersion),
	))
}
