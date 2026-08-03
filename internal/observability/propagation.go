package observability

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// TraceParentKey is the W3C TraceContext header key used to serialize span
// contexts across process boundaries (API -> DB -> Gateway -> Worker).
const TraceParentKey = "traceparent"

// InjectTraceParent serializes the span context carried by ctx into a W3C
// traceparent string. Returns an empty string when ctx holds no valid span
// context (e.g. tracing disabled or no active span).
func InjectTraceParent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier[TraceParentKey]
}

// ContextWithTraceParent returns a context carrying the remote span context
// parsed from a W3C traceparent value. Spans started from the returned
// context become children of the remote span. If traceparent is empty or
// invalid, ctx is returned unchanged.
func ContextWithTraceParent(ctx context.Context, traceparent string) context.Context {
	if traceparent == "" {
		return ctx
	}
	carrier := propagation.MapCarrier{TraceParentKey: traceparent}
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}
