package worker

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	workerv1 "github.com/xjfyrh/jobforge/proto/jobforge/worker/v1"
)

func workerTestStatus(
	t *testing.T,
	grpcCode codes.Code,
	domainCode string,
	retryable bool,
) error {
	t.Helper()
	withDetail, err := status.New(grpcCode, "test error").WithDetails(&workerv1.DomainErrorDetail{
		Code:      domainCode,
		Retryable: retryable,
	})
	if err != nil {
		t.Fatalf("attach domain detail: %v", err)
	}
	return withDetail.Err()
}

func TestRuntimePrefersStableDomainErrorDetail(t *testing.T) {
	permanentResourceExhausted := workerTestStatus(t, codes.ResourceExhausted, "INVALID_ARGUMENT", false)
	if isTransientRPCError(permanentResourceExhausted) {
		t.Fatal("detail retryable=false must override ResourceExhausted fallback")
	}
	if !isPermanentHeartbeatRejection(permanentResourceExhausted) {
		t.Fatal("permanent detail must stop heartbeat lease use")
	}

	retryableInvalidArgument := workerTestStatus(t, codes.InvalidArgument, "INTERNAL", true)
	if !isTransientRPCError(retryableInvalidArgument) {
		t.Fatal("detail retryable=true must override InvalidArgument fallback")
	}
	if isPermanentHeartbeatRejection(retryableInvalidArgument) {
		t.Fatal("retryable detail must not abandon the lease immediately")
	}
}

func TestRuntimeFallsBackForLegacyGatewayErrors(t *testing.T) {
	if !isTransientRPCError(status.Error(codes.Unavailable, "legacy gateway unavailable")) {
		t.Fatal("legacy Unavailable must remain retryable")
	}
	if isTransientRPCError(status.Error(codes.InvalidArgument, "legacy invalid argument")) {
		t.Fatal("legacy InvalidArgument must remain permanent")
	}
	if !isPermanentHeartbeatRejection(status.Error(codes.FailedPrecondition, "legacy stale lease")) {
		t.Fatal("legacy FailedPrecondition must abandon the lease")
	}
	if isPermanentHeartbeatRejection(status.Error(codes.Unavailable, "legacy transient")) {
		t.Fatal("legacy Unavailable must keep retrying within the lease")
	}
}
