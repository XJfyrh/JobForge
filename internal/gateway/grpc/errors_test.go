package grpc

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/observability"
	workerv1 "github.com/xjfyrh/jobforge/proto/jobforge/worker/v1"
)

func assertDomainStatus(
	t *testing.T,
	err error,
	wantGRPC codes.Code,
	wantDomain domain.ErrorCode,
	wantRetryable bool,
) {
	t.Helper()
	grpcStatus := status.Convert(err)
	if grpcStatus.Code() != wantGRPC {
		t.Fatalf("gRPC code = %s, want %s: %v", grpcStatus.Code(), wantGRPC, err)
	}
	details := grpcStatus.Details()
	if len(details) != 1 {
		t.Fatalf("details = %d, want exactly 1: %#v", len(details), details)
	}
	detail, ok := details[0].(*workerv1.DomainErrorDetail)
	if !ok {
		t.Fatalf("detail type = %T, want DomainErrorDetail", details[0])
	}
	if detail.Code != string(wantDomain) || detail.Retryable != wantRetryable {
		t.Fatalf("detail = %+v, want code=%s retryable=%v", detail, wantDomain, wantRetryable)
	}
}

func TestAT31DomainErrorStatusMatrix(t *testing.T) {
	tests := []struct {
		domainCode domain.ErrorCode
		grpcCode   codes.Code
		retryable  bool
	}{
		{domain.CodeInvalidArgument, codes.InvalidArgument, false},
		{domain.CodeUnauthorized, codes.Unauthenticated, false},
		{domain.CodeForbidden, codes.PermissionDenied, false},
		{domain.CodeNotFound, codes.NotFound, false},
		{domain.CodeConflict, codes.AlreadyExists, false},
		{domain.CodeAlreadyTerminal, codes.FailedPrecondition, false},
		{domain.CodeStaleLease, codes.FailedPrecondition, false},
		{domain.CodeCancelRequested, codes.FailedPrecondition, false},
		{domain.CodeInvalidTransition, codes.FailedPrecondition, false},
		{domain.CodeQueueOverloaded, codes.ResourceExhausted, true},
		{domain.CodeInternal, codes.Internal, true},
	}

	for _, tt := range tests {
		t.Run(string(tt.domainCode), func(t *testing.T) {
			domainErr := domain.NewError(tt.domainCode, errors.New("sentinel"), "safe message")
			assertDomainStatus(t, mapError(domainErr), tt.grpcCode, tt.domainCode, tt.retryable)
		})
	}

	unknown := mapError(errors.New("sql and credentials must not escape"))
	assertDomainStatus(t, unknown, codes.Internal, domain.CodeInternal, true)
	if status.Convert(unknown).Message() != "internal error" {
		t.Fatalf("unknown internal message leaked: %q", status.Convert(unknown).Message())
	}
	unknownCode := mapError(domain.NewError(domain.ErrorCode("FUTURE_UNKNOWN"), errors.New("secret"), "must not escape"))
	assertDomainStatus(t, unknownCode, codes.Internal, domain.CodeInternal, true)
	if status.Convert(unknownCode).Message() != "internal error" {
		t.Fatalf("unknown domain code message leaked: %q", status.Convert(unknownCode).Message())
	}
}

func TestAT31EveryWorkerRPCValidationHasDetail(t *testing.T) {
	catalog, err := domain.NewTaskTypeCatalog(domain.DefaultTaskTypeNames())
	if err != nil {
		t.Fatalf("create catalog: %v", err)
	}
	service := NewWorkerService(
		nil,
		nil,
		catalog,
		30*time.Second,
		5*time.Second,
		0,
		true,
		slog.Default(),
		nil,
	)

	tests := []struct {
		name string
		call func() error
	}{
		{name: "Register", call: func() error {
			_, err := service.Register(context.Background(), &workerv1.RegisterRequest{})
			return err
		}},
		{name: "Poll", call: func() error { _, err := service.Poll(context.Background(), &workerv1.PollRequest{}); return err }},
		{name: "Heartbeat", call: func() error {
			_, err := service.Heartbeat(context.Background(), &workerv1.HeartbeatRequest{})
			return err
		}},
		{name: "Complete", call: func() error {
			_, err := service.Complete(context.Background(), &workerv1.CompleteRequest{})
			return err
		}},
		{name: "Fail", call: func() error { _, err := service.Fail(context.Background(), &workerv1.FailRequest{}); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertDomainStatus(t, tt.call(), codes.InvalidArgument, domain.CodeInvalidArgument, false)
		})
	}
}

func TestAT31DeadlineInterceptorAddsDetail(t *testing.T) {
	ctx := context.Background()
	registry := prometheus.NewRegistry()
	metrics, shutdown, err := observability.SetupMetrics(ctx, registry)
	if err != nil {
		t.Fatalf("setup metrics: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	interceptor := deadlineInterceptor(slog.Default(), metrics)
	handlerCalled := false
	_, err = interceptor(
		context.Background(),
		nil,
		&grpc.UnaryServerInfo{FullMethod: "/jobforge.worker.v1.WorkerService/Poll"},
		func(context.Context, any) (any, error) {
			handlerCalled = true
			return nil, nil
		},
	)
	if handlerCalled {
		t.Fatal("missing-deadline request reached handler")
	}
	assertDomainStatus(t, err, codes.InvalidArgument, domain.CodeInvalidArgument, false)

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	metricFound := false
	for _, family := range families {
		if family.GetName() != "jobforge_contract_rejections_total" {
			continue
		}
		for _, sample := range family.GetMetric() {
			labels := make(map[string]string)
			for _, label := range sample.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["surface"] == "grpc_interceptor" && labels["reason"] == "missing_deadline" &&
				sample.GetCounter().GetValue() == 1 {
				metricFound = true
			}
		}
	}
	if !metricFound {
		t.Fatal("missing deadline contract rejection metric was not recorded")
	}

	deadlineCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = interceptor(
		deadlineCtx,
		nil,
		&grpc.UnaryServerInfo{FullMethod: "/jobforge.worker.v1.WorkerService/Register"},
		func(context.Context, any) (any, error) {
			return nil, status.Error(codes.Unauthenticated, "missing worker credential")
		},
	)
	assertDomainStatus(t, err, codes.Unauthenticated, domain.CodeUnauthorized, false)
}
