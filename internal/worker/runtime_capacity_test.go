package worker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc"

	workerv1 "github.com/xjfyrh/jobforge/proto/jobforge/worker/v1"
)

type capacityWorkerClient struct {
	fakeWorkerClient
	polls  atomic.Int32
	onPoll func(*workerv1.PollRequest) (*workerv1.PollResponse, error)
}

func (f *capacityWorkerClient) Poll(_ context.Context, req *workerv1.PollRequest, _ ...grpc.CallOption) (*workerv1.PollResponse, error) {
	f.polls.Add(1)
	if f.onPoll != nil {
		return f.onPoll(req)
	}
	return &workerv1.PollResponse{}, nil
}

func TestPollWakesOnCapacityRelease(t *testing.T) {
	for _, staleHint := range []bool{false, true} {
		name := "full"
		if staleHint {
			name = "stale notification while full"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := &capacityWorkerClient{onPoll: func(req *workerv1.PollRequest) (*workerv1.PollResponse, error) {
					if req.MaxJobs != 1 || req.AvailableCapacity != 1 {
						t.Errorf("poll capacity: max=%d available=%d, want 1", req.MaxJobs, req.AvailableCapacity)
					}
					return &workerv1.PollResponse{}, nil
				}}
				r := newTestRuntime(client)
				r.inflight = r.cfg.Capacity
				if staleHint {
					r.capacityAvailable <- struct{}{}
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() {
					_, err := r.poll(ctx)
					done <- err
				}()
				synctest.Wait()
				if client.polls.Load() != 0 {
					t.Fatal("full worker issued a Poll RPC")
				}
				select {
				case <-done:
					t.Fatal("poll returned while capacity was exhausted")
				default:
				}

				start := time.Now()
				r.releaseCapacity()
				synctest.Wait()
				select {
				case err := <-done:
					if err != nil {
						t.Fatalf("poll after release: %v", err)
					}
				default:
					t.Fatal("capacity release did not wake poll")
				}
				if client.polls.Load() != 1 || time.Since(start) != 0 {
					t.Fatalf("refill must issue one RPC without a timer: polls=%d elapsed=%v", client.polls.Load(), time.Since(start))
				}
			})
		})
	}
}

func TestPollCoalescesCompletionsBeforeWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &capacityWorkerClient{}
		r := newTestRuntime(client)
		r.inflight = r.cfg.Capacity
		// All completions precede Poll. A single buffered hint must preserve
		// the wakeup without blocking any of the finishing goroutines.
		for range r.cfg.Capacity {
			go r.releaseCapacity()
		}
		synctest.Wait()
		client.onPoll = func(req *workerv1.PollRequest) (*workerv1.PollResponse, error) {
			if int(req.AvailableCapacity) != r.cfg.Capacity || int(req.MaxJobs) != r.cfg.Capacity {
				t.Errorf("coalesced completions lost capacity: max=%d available=%d", req.MaxJobs, req.AvailableCapacity)
			}
			return &workerv1.PollResponse{}, nil
		}
		if _, err := r.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
		if r.inflight != 0 || client.polls.Load() != 1 {
			t.Fatalf("inflight=%d polls=%d, want 0 and 1", r.inflight, client.polls.Load())
		}
	})
}

func TestPollCapacityWaitCancellation(t *testing.T) {
	for _, release := range []bool{false, true} {
		name := "cancel while full"
		if release {
			name = "cancel and completion both ready"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := &capacityWorkerClient{}
				r := newTestRuntime(client)
				r.inflight = r.cfg.Capacity
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() {
					_, err := r.poll(ctx)
					done <- err
				}()
				synctest.Wait()
				cancel()
				if release {
					r.releaseCapacity()
				}
				synctest.Wait()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("poll error=%v, want context.Canceled", err)
				}
				if client.polls.Load() != 0 {
					t.Fatal("cancelled worker issued a Poll RPC")
				}
			})
		})
	}
}

func TestPollCapacityWaitDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &capacityWorkerClient{}
		r := newTestRuntime(client)
		r.inflight = r.cfg.Capacity
		ctx, cancel := context.WithTimeout(context.Background(), 37*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := r.poll(ctx)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 37*time.Millisecond {
			t.Fatalf("poll deadline: err=%v elapsed=%v", err, time.Since(start))
		}
		if client.polls.Load() != 0 {
			t.Fatal("full worker issued a Poll RPC before its deadline")
		}
	})
}

func TestPollStoppingDoesNotClaim(t *testing.T) {
	client := &capacityWorkerClient{}
	r := newTestRuntime(client)
	r.stopping = true
	if _, err := r.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.polls.Load() != 0 {
		t.Fatal("stopping worker issued a Poll RPC")
	}
}

func TestPollFailureDoesNotConsumeReleasedCapacity(t *testing.T) {
	wantErr := errors.New("gateway unavailable")
	client := &capacityWorkerClient{onPoll: func(*workerv1.PollRequest) (*workerv1.PollResponse, error) {
		return nil, wantErr
	}}
	r := newTestRuntime(client)
	r.inflight = r.cfg.Capacity
	r.releaseCapacity()
	if _, err := r.poll(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("poll error=%v, want %v", err, wantErr)
	}
	client.onPoll = nil
	if _, err := r.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.inflight != r.cfg.Capacity-1 || client.polls.Load() != 2 {
		t.Fatalf("poll failure consumed capacity: inflight=%d polls=%d", r.inflight, client.polls.Load())
	}
}
