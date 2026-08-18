package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xjfyrh/jobforge/internal/domain"
	workerv1 "github.com/xjfyrh/jobforge/proto/jobforge/worker/v1"
)

func assertWorkerDomainDetail(
	t *testing.T,
	err error,
	wantGRPC codes.Code,
	wantDomain domain.ErrorCode,
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
	if detail.Code != string(wantDomain) || detail.Retryable {
		t.Fatalf("detail = %+v, want code=%s retryable=false", detail, wantDomain)
	}
}

// TestAT31LeaseAndCancelDomainDetails drives real PostgreSQL state through the
// Gateway and proves the two FailedPrecondition outcomes remain distinguishable
// by stable detail, including the corrected CANCEL_REQUESTED mapping.
func TestAT31LeaseAndCancelDomainDetails(t *testing.T) {
	service, jobStore := newWorkerContractService(t)
	ctx := context.Background()
	workerID := "at31-worker-" + uuid.New().String()[:8]
	queue := "at31-queue-" + uuid.New().String()[:8]
	job := createTestJob(t, jobStore, queue, "demo.echo")

	if _, err := service.Register(ctx, &workerv1.RegisterRequest{
		WorkerId:       workerID,
		InstanceId:     "at31-instance",
		Queues:         []string{queue},
		SupportedTypes: []string{"demo.echo"},
		Capacity:       1,
		Version:        "at31",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	pollResponse, err := service.Poll(ctx, &workerv1.PollRequest{
		WorkerId:          workerID,
		MaxJobs:           1,
		AvailableCapacity: 1,
		Queues:            []string{queue},
		Types:             []string{"demo.echo"},
	})
	if err != nil || len(pollResponse.Jobs) != 1 {
		t.Fatalf("poll: jobs=%d err=%v", len(pollResponse.Jobs), err)
	}
	claimed := pollResponse.Jobs[0]

	_, err = service.Complete(ctx, &workerv1.CompleteRequest{
		JobId:        job.ID,
		WorkerId:     workerID,
		FencingToken: claimed.FencingToken + 1,
	})
	assertWorkerDomainDetail(t, err, codes.FailedPrecondition, domain.CodeStaleLease)

	if err := jobStore.Cancel(ctx, "test-tenant", job.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	_, err = service.Complete(ctx, &workerv1.CompleteRequest{
		JobId:        job.ID,
		WorkerId:     workerID,
		FencingToken: claimed.FencingToken,
	})
	assertWorkerDomainDetail(t, err, codes.FailedPrecondition, domain.CodeCancelRequested)
}
