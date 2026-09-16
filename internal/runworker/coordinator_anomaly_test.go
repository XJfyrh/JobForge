package runworker

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

type anomalyClient struct {
	agentv1.AgentServiceClient
	fail func(context.Context, *agentv1.FailAttemptRequest) (*agentv1.FailAttemptResponse, error)
}

func (f *anomalyClient) FailAttempt(ctx context.Context, request *agentv1.FailAttemptRequest, _ ...grpc.CallOption) (*agentv1.FailAttemptResponse, error) {
	return f.fail(ctx, request)
}

func TestMeasurementAnomalyFailurePreservesAuthorityAndOriginalIdentity(t *testing.T) {
	for _, mode := range []string{"live", "stop", "expired", "cancelled", "unconfirmed"} {
		t.Run(mode, func(t *testing.T) {
			c, _, _ := coordinatorFixture(t, "model_proposal")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "stop":
				c.authority.Stop("STOP_REQUESTED")
			case "expired":
				c.authority.Stop("STALE_LEASE")
			case "cancelled":
				cancel()
			}
			calls := 0
			c.worker.client = &anomalyClient{fail: func(bounded context.Context, request *agentv1.FailAttemptRequest) (*agentv1.FailAttemptResponse, error) {
				calls++
				deadline, ok := bounded.Deadline()
				if !ok || time.Until(deadline) > controlTimeout || time.Until(deadline) <= 0 {
					t.Fatal("anomaly failure lacks the bounded control deadline")
				}
				if request.ErrorCode != "MODEL_PROTOCOL_ERROR" || !proto.Equal(request.Execution, c.lease.Execution) || !proto.Equal(request.Step, c.checkpoint.NextStep) {
					t.Fatal("anomaly failure changed its original execution or step binding")
				}
				if mode == "unconfirmed" {
					return nil, status.Error(codes.Unavailable, "fixture")
				}
				return &agentv1.FailAttemptResponse{}, nil
			}}
			c.failMeasurementAnomaly(ctx)
			want := 0
			if mode == "live" || mode == "unconfirmed" {
				want = 1
			}
			if calls != want {
				t.Fatalf("FailAttempt calls = %d, want %d", calls, want)
			}
		})
	}
}
