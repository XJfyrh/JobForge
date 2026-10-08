package run

import (
	"testing"
	"time"
)

func TestContentRetentionRequiresTerminalAgeAndClosedBatch(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	old := now.Add(-TerminalContentRetention)
	future := now.Add(time.Nanosecond)
	for _, tc := range []struct {
		name       string
		state      State
		at, purged *time.Time
		batch      time.Time
		want       bool
	}{
		{"exact minimum", Failed, &old, nil, now, true},
		{"cancelled", Cancelled, &old, nil, now, true},
		{"applied", Succeeded, &old, nil, now, true},
		{"fresh terminal", Failed, &now, nil, now, false},
		{"pending approval", AwaitingApproval, &old, nil, now, false},
		{"running", Running, &old, nil, now, false},
		{"missing first terminal", Failed, nil, nil, now, false},
		{"open batch needs audit checkpoints", Failed, &old, nil, future, false},
		{"already purged", Failed, &old, &now, now, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Run{State: tc.state, TerminalAt: tc.at, ContentPurgedAt: tc.purged}
			if CanPurgeContent(r, tc.batch, now) != tc.want {
				t.Fatal("incorrect retention boundary")
			}
		})
	}
}
