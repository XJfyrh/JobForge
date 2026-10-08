package observability

import (
	"context"
	"errors"
	"math"
	"os"
	"strconv"
	"time"
)

// SetupRunTracing uses the same bounded exporter as the historical runtime,
// with tracing disabled by default for standalone Run/business processes.
func SetupRunTracing(ctx context.Context, service string) (func(), error) {
	exporter := os.Getenv("JOBFORGE_OTEL_EXPORTER")
	if exporter == "" {
		exporter = "none"
	}
	ratio := 1.0
	if value := os.Getenv("JOBFORGE_OTEL_SAMPLE_RATIO"); value != "" {
		var err error
		ratio, err = strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(ratio) || ratio < 0 || ratio > 1 {
			return nil, errors.New("invalid trace configuration")
		}
	}
	shutdown, err := SetupTracing(ctx, Config{ServiceName: service, ServiceVersion: "agent-v3-s5", ExporterType: exporter, SampleRatio: ratio})
	if err != nil {
		return nil, errors.New("trace setup unavailable")
	}
	return func() {
		flush, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = shutdown(flush)
	}, nil
}
