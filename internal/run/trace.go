package run

import (
	"context"
	"regexp"
)

type traceContextKey struct{}

var traceParentPattern = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`)

// ValidTraceContext accepts only the bounded representation emitted by the
// trusted transport. Trace metadata never grants authority or changes a hash.
func ValidTraceContext(value string) bool {
	return value == "" || traceParentPattern.MatchString(value) &&
		value[3:35] != "00000000000000000000000000000000" && value[36:52] != "0000000000000000"
}

// WithTraceContext carries presentation-only metadata into admission without
// coupling the domain service to a telemetry SDK.
func WithTraceContext(ctx context.Context, value string) context.Context {
	if !ValidTraceContext(value) {
		return ctx
	}
	return context.WithValue(ctx, traceContextKey{}, value)
}

func admissionTraceContext(ctx context.Context) string {
	value, _ := ctx.Value(traceContextKey{}).(string)
	return value
}
