package observability

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	tracecollector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestOTLPHTTPExportAndUnavailableBackend(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "export", true: "backend unavailable"}[failure], func(t *testing.T) {
			oldProvider, oldPropagator, oldErrors := otel.GetTracerProvider(), otel.GetTextMapPropagator(), otel.GetErrorHandler()
			t.Cleanup(func() {
				otel.SetTracerProvider(oldProvider)
				otel.SetTextMapPropagator(oldPropagator)
				otel.SetErrorHandler(oldErrors)
			})
			received := make(chan *tracecollector.ExportTraceServiceRequest, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/traces" {
					t.Error("incorrect OTLP path")
				}
				data, _ := io.ReadAll(r.Body)
				var request tracecollector.ExportTraceServiceRequest
				if err := proto.Unmarshal(data, &request); err != nil {
					t.Error(err)
				}
				received <- &request
				if failure {
					w.WriteHeader(503)
					return
				}
				w.Header().Set("Content-Type", "application/x-protobuf")
			}))
			defer server.Close()
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
			cfg := DefaultConfig()
			cfg.ExporterType = "otlp"
			cfg.ServiceName = "jobforge-otlp-test"
			shutdown, err := SetupTracing(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			_, span := Tracer("test").Start(t.Context(), "task")
			span.End()
			if time.Since(started) > time.Second {
				t.Fatal("span end blocked on exporter")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			_ = shutdown(ctx)
			select {
			case req := <-received:
				if len(req.ResourceSpans) != 1 || len(req.ResourceSpans[0].ScopeSpans[0].Spans) != 1 {
					t.Fatal("missing exported span")
				}
			case <-ctx.Done():
				t.Fatal("no OTLP request")
			}
		})
	}
}
