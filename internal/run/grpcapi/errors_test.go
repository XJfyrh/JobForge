package grpcapi

import (
	"errors"
	"fmt"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xjfyrh/jobforge/internal/run"
)

func TestBoundaryKeepsPermanentResultReasonsAndRedactsUnknownFailures(t *testing.T) {
	for _, test := range []struct {
		err    error
		code   codes.Code
		reason string
	}{
		{fmt.Errorf("protected source: %w", run.ErrCheckpointTooLarge), codes.InvalidArgument, "CHECKPOINT_TOO_LARGE"},
		{fmt.Errorf("protected source: %w", run.ErrModelProtocol), codes.InvalidArgument, "MODEL_PROTOCOL_ERROR"},
		{errors.New("protected source"), codes.Internal, "INTERNAL"},
		{run.ErrorCode("UNREGISTERED_PROTECTED_DETAIL"), codes.Internal, "INTERNAL"},
	} {
		actual := status.Convert(mapError(test.err))
		if actual.Code() != test.code || actual.Message() != test.reason || len(actual.Details()) != 1 {
			t.Fatalf("unexpected boundary classification: %s", actual.Code())
		}
		detail, ok := actual.Details()[0].(*errdetails.ErrorInfo)
		if !ok || detail.Reason != test.reason || detail.Domain != "jobforge.agent.v1" || len(detail.Metadata) != 0 {
			t.Fatal("missing safe stable domain reason")
		}
	}
}
