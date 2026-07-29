package domain

import (
	"testing"
	"time"
)

func TestCanTransition(t *testing.T) {
	tests := []struct {
		from, to JobState
		want     bool
	}{
		// Valid transitions.
		{StateScheduled, StateReady, true},
		{StateScheduled, StateCancelled, true},
		{StateReady, StateRunning, true},
		{StateReady, StateCancelled, true},
		{StateRunning, StateSucceeded, true},
		{StateRunning, StateRetryWait, true},
		{StateRunning, StateDead, true},
		{StateRunning, StateReady, true}, // lease expired recovery
		{StateRunning, StateCancelling, true},
		{StateCancelling, StateCancelled, true},
		{StateRetryWait, StateReady, true},
		{StateRetryWait, StateCancelled, true},

		// Invalid transitions.
		{StateSucceeded, StateRunning, false},
		{StateDead, StateReady, false},
		{StateCancelled, StateRunning, false},
		{StateScheduled, StateRunning, false},
		{StateReady, StateSucceeded, false},
		{StateCancelling, StateSucceeded, false},
		{StateCancelling, StateRetryWait, false},
		{StateRetryWait, StateRunning, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.from)+"->"+string(tt.to), func(t *testing.T) {
			got := CanTransition(tt.from, tt.to)
			if got != tt.want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", tt.from, tt.to, got, tt.want)
			}
		})
	}
}

func TestIsTerminal(t *testing.T) {
	terminal := []JobState{StateSucceeded, StateDead, StateCancelled}
	nonTerminal := []JobState{StateScheduled, StateReady, StateRunning, StateCancelling, StateRetryWait}

	for _, s := range terminal {
		if !s.IsTerminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	for _, s := range nonTerminal {
		if s.IsTerminal() {
			t.Errorf("%s should not be terminal", s)
		}
	}
}

func TestNewJob(t *testing.T) {
	now := time.Now()

	t.Run("immediate job starts ready", func(t *testing.T) {
		job, err := NewJob("id-1", NewJobParams{
			TenantID: "t1",
			Queue:    "q1",
			Type:     "demo.echo",
			Payload:  []byte(`{}`),
		}, now)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if job.State != StateReady {
			t.Errorf("expected ready, got %s", job.State)
		}
		if job.Attempt != 0 {
			t.Errorf("expected attempt 0, got %d", job.Attempt)
		}
		if job.MaxAttempts != DefaultMaxAttempts {
			t.Errorf("expected default max_attempts %d, got %d", DefaultMaxAttempts, job.MaxAttempts)
		}
	})

	t.Run("future run_at starts scheduled", func(t *testing.T) {
		future := now.Add(time.Hour)
		job, err := NewJob("id-2", NewJobParams{
			TenantID: "t1",
			Queue:    "q1",
			Type:     "demo.echo",
			Payload:  []byte(`{}`),
			RunAt:    &future,
		}, now)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if job.State != StateScheduled {
			t.Errorf("expected scheduled, got %s", job.State)
		}
	})

	t.Run("missing tenant rejected", func(t *testing.T) {
		_, err := NewJob("id-3", NewJobParams{
			Queue:   "q1",
			Type:    "demo.echo",
			Payload: []byte(`{}`),
		}, now)
		if err == nil {
			t.Fatal("expected error for missing tenant")
		}
	})

	t.Run("oversized payload rejected", func(t *testing.T) {
		big := make([]byte, MaxPayloadSize+1)
		_, err := NewJob("id-4", NewJobParams{
			TenantID: "t1",
			Queue:    "q1",
			Type:     "demo.echo",
			Payload:  big,
		}, now)
		if err == nil {
			t.Fatal("expected error for oversized payload")
		}
	})

	t.Run("invalid max_attempts rejected", func(t *testing.T) {
		_, err := NewJob("id-5", NewJobParams{
			TenantID:    "t1",
			Queue:       "q1",
			Type:        "demo.echo",
			Payload:     []byte(`{}`),
			MaxAttempts: 11,
		}, now)
		if err == nil {
			t.Fatal("expected error for max_attempts > 10")
		}
	})
}

func TestJobClaim(t *testing.T) {
	now := time.Now()
	job, _ := NewJob("id-1", NewJobParams{
		TenantID: "t1",
		Queue:    "q1",
		Type:     "demo.echo",
		Payload:  []byte(`{}`),
	}, now)

	err := job.Claim("worker-1", 30*time.Second, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if job.State != StateRunning {
		t.Errorf("expected running, got %s", job.State)
	}
	if job.Attempt != 1 {
		t.Errorf("expected attempt 1, got %d", job.Attempt)
	}
	if job.FencingToken != 1 {
		t.Errorf("expected fencing token 1, got %d", job.FencingToken)
	}
	if *job.LeaseOwner != "worker-1" {
		t.Errorf("expected owner worker-1, got %s", *job.LeaseOwner)
	}

	// Claim again should fail (not ready).
	err = job.Claim("worker-2", 30*time.Second, now)
	if err == nil {
		t.Fatal("expected error claiming non-ready job")
	}
}

func TestJobComplete(t *testing.T) {
	now := time.Now()
	job, _ := NewJob("id-1", NewJobParams{
		TenantID: "t1",
		Queue:    "q1",
		Type:     "demo.echo",
		Payload:  []byte(`{}`),
	}, now)
	_ = job.Claim("worker-1", 30*time.Second, now)

	// Wrong token.
	err := job.Complete("worker-1", 999, now)
	if err == nil {
		t.Fatal("expected error for wrong token")
	}

	// Correct token.
	err = job.Complete("worker-1", 1, now)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if job.State != StateSucceeded {
		t.Errorf("expected succeeded, got %s", job.State)
	}
}

func TestJobFail(t *testing.T) {
	now := time.Now()

	t.Run("retryable with remaining attempts", func(t *testing.T) {
		job, _ := NewJob("id-1", NewJobParams{
			TenantID:    "t1",
			Queue:       "q1",
			Type:        "demo.fail",
			Payload:     []byte(`{}`),
			MaxAttempts: 3,
		}, now)
		_ = job.Claim("w1", 30*time.Second, now)

		nextRetry := now.Add(time.Second)
		err := job.Fail("w1", 1, true, nextRetry, now)
		if err != nil {
			t.Fatalf("fail: %v", err)
		}
		if job.State != StateRetryWait {
			t.Errorf("expected retry_wait, got %s", job.State)
		}
	})

	t.Run("retryable but attempts exhausted", func(t *testing.T) {
		job, _ := NewJob("id-2", NewJobParams{
			TenantID:    "t1",
			Queue:       "q1",
			Type:        "demo.fail",
			Payload:     []byte(`{}`),
			MaxAttempts: 1,
		}, now)
		_ = job.Claim("w1", 30*time.Second, now)

		err := job.Fail("w1", 1, true, now, now)
		if err != nil {
			t.Fatalf("fail: %v", err)
		}
		if job.State != StateDead {
			t.Errorf("expected dead, got %s", job.State)
		}
	})

	t.Run("non-retryable goes dead", func(t *testing.T) {
		job, _ := NewJob("id-3", NewJobParams{
			TenantID:    "t1",
			Queue:       "q1",
			Type:        "demo.fail",
			Payload:     []byte(`{}`),
			MaxAttempts: 5,
		}, now)
		_ = job.Claim("w1", 30*time.Second, now)

		err := job.Fail("w1", 1, false, now, now)
		if err != nil {
			t.Fatalf("fail: %v", err)
		}
		if job.State != StateDead {
			t.Errorf("expected dead, got %s", job.State)
		}
	})

	t.Run("fail in cancelling goes cancelled", func(t *testing.T) {
		job, _ := NewJob("id-4", NewJobParams{
			TenantID: "t1",
			Queue:    "q1",
			Type:     "demo.echo",
			Payload:  []byte(`{}`),
		}, now)
		_ = job.Claim("w1", 30*time.Second, now)
		_ = job.Cancel(now) // running -> cancelling

		err := job.Fail("w1", 1, true, now, now)
		if err != nil {
			t.Fatalf("fail: %v", err)
		}
		if job.State != StateCancelled {
			t.Errorf("expected cancelled, got %s", job.State)
		}
	})
}

func TestJobCancel(t *testing.T) {
	now := time.Now()

	t.Run("cancel ready job", func(t *testing.T) {
		job, _ := NewJob("id-1", NewJobParams{
			TenantID: "t1", Queue: "q1", Type: "demo.echo", Payload: []byte(`{}`),
		}, now)
		err := job.Cancel(now)
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if job.State != StateCancelled {
			t.Errorf("expected cancelled, got %s", job.State)
		}
	})

	t.Run("cancel running job enters cancelling", func(t *testing.T) {
		job, _ := NewJob("id-2", NewJobParams{
			TenantID: "t1", Queue: "q1", Type: "demo.echo", Payload: []byte(`{}`),
		}, now)
		_ = job.Claim("w1", 30*time.Second, now)
		err := job.Cancel(now)
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if job.State != StateCancelling {
			t.Errorf("expected cancelling, got %s", job.State)
		}
	})

	t.Run("cancel terminal job fails", func(t *testing.T) {
		job, _ := NewJob("id-3", NewJobParams{
			TenantID: "t1", Queue: "q1", Type: "demo.echo", Payload: []byte(`{}`),
		}, now)
		_ = job.Claim("w1", 30*time.Second, now)
		_ = job.Complete("w1", 1, now)
		err := job.Cancel(now)
		if err == nil {
			t.Fatal("expected error cancelling terminal job")
		}
	})
}

func TestBackoff(t *testing.T) {
	tests := []struct {
		attempt int
		base    time.Duration
		max     time.Duration
		jitter  time.Duration
		wantMax time.Duration
	}{
		{1, time.Second, 5 * time.Minute, 0, time.Second},
		{2, time.Second, 5 * time.Minute, 0, 2 * time.Second},
		{3, time.Second, 5 * time.Minute, 0, 4 * time.Second},
		{10, time.Second, 5 * time.Minute, 0, 5 * time.Minute}, // capped
		{1, time.Second, 5 * time.Minute, 500 * time.Millisecond, 1500 * time.Millisecond},
	}

	for _, tt := range tests {
		got := Backoff(tt.attempt, tt.base, tt.max, tt.jitter)
		if got > tt.wantMax {
			t.Errorf("Backoff(%d) = %v, want <= %v", tt.attempt, got, tt.wantMax)
		}
	}
}
