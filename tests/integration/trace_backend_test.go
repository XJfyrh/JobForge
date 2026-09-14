package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"
)

// assertTraceBackend checks persisted Jaeger spans, not an in-memory exporter.
// A killed process can lose its unfinished/buffered spans; surviving reports
// and the scheduler recovery still share the persisted submission context.
func assertTraceBackend(t *testing.T, traceID string, completeGraph bool, required ...string) {
	t.Helper()
	endpoint := os.Getenv("JOBFORGE_TEST_JAEGER_URL")
	if endpoint == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if provider, ok := otel.GetTracerProvider().(*trace.TracerProvider); ok {
		if err := provider.ForceFlush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var missing []string
	for ctx.Err() == nil {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/api/traces/"+traceID, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"Northwind Components", "PO-2026-0042", "dev-api-key", "fault-key", "business-a", "\"payload\"", "\"Authorization\""} {
			if strings.Contains(string(body), forbidden) {
				t.Fatalf("sensitive content found in trace: %q", forbidden)
			}
		}
		var result struct {
			Data []struct {
				Spans []struct {
					SpanID     string `json:"spanID"`
					Name       string `json:"operationName"`
					References []struct {
						Type    string `json:"refType"`
						TraceID string `json:"traceID"`
						SpanID  string `json:"spanID"`
					} `json:"references"`
				} `json:"spans"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatalf("Jaeger response is not JSON: HTTP %d", resp.StatusCode)
		}
		names, ids := map[string]int{}, map[string]bool{}
		for _, data := range result.Data {
			for _, span := range data.Spans {
				names[span.Name]++
				ids[span.SpanID] = true
			}
		}
		missing = nil
		for _, name := range required {
			if names[name] == 0 {
				missing = append(missing, name)
			}
		}
		if completeGraph {
			for _, data := range result.Data {
				for _, span := range data.Spans {
					for _, ref := range span.References {
						if ref.Type == "CHILD_OF" && ref.TraceID == traceID && !ids[ref.SpanID] {
							missing = append(missing, "parent of "+span.Name)
						}
					}
				}
			}
		}
		if len(missing) == 0 {
			t.Logf("JAEGER VERIFIED trace_id=%s spans=%v complete_graph=%t", traceID, names, completeGraph)
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatalf("Jaeger trace %s missing %v", traceID, missing)
}
