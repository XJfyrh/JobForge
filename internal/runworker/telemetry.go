package runworker

import (
	"context"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/xjfyrh/jobforge/internal/observability"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

// TraceRPC observes one physical RPC; it never retries a control operation.
func TraceRPC(ctx context.Context, method string, request, response any, connection *grpc.ClientConn, invoke grpc.UnaryInvoker, options ...grpc.CallOption) error {
	started := time.Now()
	ctx, span := otel.Tracer("jobforge/runworker").Start(ctx, "run.control_rpc", trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	span.SetAttributes(attribute.String("rpc.method", method))
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	for key, value := range carrier {
		ctx = metadata.AppendToOutgoingContext(ctx, key, value)
	}
	err := invoke(ctx, method, request, response, connection, options...)
	result := "ok"
	if err != nil {
		result = "error"
	}
	metricMethod := "unknown"
	if strings.HasPrefix(method, "/jobforge.agent.v1.AgentService/") {
		candidate := strings.TrimPrefix(method, "/jobforge.agent.v1.AgentService/")
		for _, rpc := range agentv1.AgentService_ServiceDesc.Methods {
			if rpc.MethodName == candidate {
				metricMethod = candidate
				break
			}
		}
	}
	observability.RunRPCDuration.WithLabelValues(metricMethod, result).Observe(time.Since(started).Seconds())
	span.SetAttributes(attribute.String("rpc.status", status.Code(err).String()))
	if err != nil {
		span.SetStatus(codes.Error, "control RPC failed")
	}
	return err
}

func attemptTrace(ctx context.Context, lease *agentv1.RunLease) (context.Context, trace.Span) {
	options := []trace.SpanStartOption{trace.WithNewRoot(), trace.WithSpanKind(trace.SpanKindConsumer)}
	parent := propagation.TraceContext{}.Extract(context.Background(), propagation.MapCarrier{"traceparent": lease.TraceContext})
	if spanContext := trace.SpanContextFromContext(parent); spanContext.IsValid() {
		options = append(options, trace.WithLinks(trace.Link{SpanContext: spanContext}))
	}
	ctx, span := otel.Tracer("jobforge/runworker").Start(ctx, "run.attempt", options...)
	span.SetAttributes(attribute.String("jobforge.run_id", lease.Execution.RunId),
		attribute.Int64("jobforge.attempt_no", lease.Execution.AttemptNo))
	return ctx, span
}

// beginCallTrace spans the verified permit-to-observation interval on Go's
// clock. An absent observation remains unknown; permit does not prove a send.
func beginCallTrace(ctx context.Context, call *callRecord) {
	_, call.span = otel.Tracer("jobforge/runworker").Start(ctx, "run.external_http", trace.WithSpanKind(trace.SpanKindClient))
	call.span.SetAttributes(attribute.String("jobforge.physical_call_id", call.id),
		attribute.String("jobforge.subcall", call.intent.Subcall),
		attribute.String("jobforge.observation.source", "verified_ipc_go_bridge"),
		attribute.String("jobforge.observation.interval", "permit_to_observation"),
		attribute.String("jobforge.send_evidence", "unknown"))
}

func endCallTrace(call *callRecord) {
	if call.span == nil {
		return
	}
	if f := call.observation; f != nil {
		call.span.SetAttributes(attribute.Int64("http.response.status_code", f.HTTPStatus),
			attribute.String("jobforge.transport_outcome", f.TransportOutcome),
			attribute.String("jobforge.business_outcome", f.BusinessOutcome))
		if f.TransportOutcome == "response" {
			call.span.SetAttributes(attribute.String("jobforge.send_evidence", "http_response"))
		}
	}
	call.span.End()
	call.span = nil
}
