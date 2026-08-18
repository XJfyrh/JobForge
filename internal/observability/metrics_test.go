package observability

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestContractRejectionMetricUsesFixedLabels(t *testing.T) {
	ctx := context.Background()
	registry := prometheus.NewRegistry()
	metrics, shutdown, err := SetupMetrics(ctx, registry)
	if err != nil {
		t.Fatalf("setup metrics: %v", err)
	}
	defer func() { _ = shutdown(ctx) }()

	metrics.RecordContractRejection(ctx, ContractSurfaceSubmit, ContractReasonUnknownType)

	const expected = `
# HELP jobforge_contract_rejections_total Total number of execution contract validation rejections
# TYPE jobforge_contract_rejections_total counter
jobforge_contract_rejections_total{otel_scope_name="jobforge",otel_scope_schema_url="",otel_scope_version="",reason="unknown_type",surface="submit"} 1
`
	if err := testutil.GatherAndCompare(
		registry,
		strings.NewReader(expected),
		"jobforge_contract_rejections_total",
	); err != nil {
		t.Fatalf("gather contract rejection metric: %v", err)
	}
}
