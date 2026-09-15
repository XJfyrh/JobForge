package worker

import (
	"context"
	"testing"
	"time"

	workerv1 "github.com/xjfyrh/jobforge/proto/jobforge/worker/v1"
)

func TestLateSuccessCannotOverrideDeadlineOrCancel(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "cancel"}[cancelled], func(t *testing.T) {
			fc := &fakeWorkerClient{}
			if cancelled {
				fc.heartbeat = func(int) (*workerv1.HeartbeatResponse, error) {
					return &workerv1.HeartbeatResponse{Signal: workerv1.ControlSignal_CONTROL_SIGNAL_CANCEL}, nil
				}
			}
			r := newTestRuntime(fc)
			r.registry.Register("late", HandlerFunc(func(ctx context.Context, _ *ClaimedJob) (string, error) { <-ctx.Done(); return "must-not-commit", nil }))
			r.executeJob(t.Context(), &ClaimedJob{ID: "late", Type: "late", Timeout: 100 * time.Millisecond, LeaseUntil: time.Now().Add(time.Hour)})
			if fc.completes.Load() != 0 || fc.fails.Load() != 1 {
				t.Fatalf("complete=%d fail=%d", fc.completes.Load(), fc.fails.Load())
			}
		})
	}
}
