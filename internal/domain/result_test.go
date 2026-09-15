package domain

import (
	"strings"
	"testing"
	"time"
)

func TestResultRefBoundaries(t *testing.T) {
	for _, ref := range []string{"", "artifact:tenant/key", strings.Repeat("x", 2048), strings.Repeat("界", 682)} {
		if err := ValidateResultRef(ref); err != nil {
			t.Fatalf("valid %d-byte reference: %v", len(ref), err)
		}
	}
	for _, ref := range []string{strings.Repeat("x", 2049), strings.Repeat("界", 683), "x\x00", "x\n", "x\x7f", string([]byte{0xff})} {
		if err := ValidateResultRef(ref); err == nil {
			t.Fatalf("accepted invalid %d-byte reference", len(ref))
		}
	}
}

func TestRejectedCompletionDoesNotAttachResult(t *testing.T) {
	owner := "owner"
	for _, state := range []JobState{StateCancelling, StateSucceeded, StateDead, StateReady} {
		job := &Job{State: state, LeaseOwner: &owner, FencingToken: 1}
		if err := job.CompleteWithResult(owner, 1, "artifact:test", time.Now()); err == nil || job.ResultRef != nil {
			t.Fatalf("state=%s err=%v ref=%v", state, err, job.ResultRef)
		}
	}
}
