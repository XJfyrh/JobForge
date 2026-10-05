package run

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestApprovalPermissionAndDeadlineExactBoundary(t *testing.T) {
	now := time.Date(2026, 10, 5, 1, 0, 0, 0, time.UTC)
	for _, decision := range []string{"approve", "reject"} {
		for _, boundary := range []string{"permission", "deadline"} {
			t.Run(decision+"/"+boundary, func(t *testing.T) {
				expiry := now.Add(time.Hour)
				r := Run{State: AwaitingApproval, PermissionExpiresAt: &expiry, RunDeadline: expiry, RecoveryCount: 2}
				if boundary == "permission" {
					expiry = now
				} else {
					r.RunDeadline = now
				}
				a := Authority{NextStepID: "proposal", NextStepKind: "submit_proposal", NextInputHash: strings.Repeat("a", 64)}
				beforeRun, beforeAuthority := r, a
				request := ApprovalRequest{SchemaVersion: 1, Decision: decision, ProposalHash: strings.Repeat("b", 64)}
				if err := DecideApproval(&r, &a, request, "commit", "decision", "action", now); !errors.Is(err, ErrApprovalExpired) || !reflect.DeepEqual(r, beforeRun) || a != beforeAuthority {
					t.Fatal("equal expiry must reject without mutation", err)
				}
				if err := DecideApproval(&r, &a, request, "commit", "decision", "action", now.Add(-time.Microsecond)); err != nil || r.RecoveryCount != 2 || !r.PermissionExpiresAt.Equal(expiry) {
					t.Fatal("one microsecond before expiry changed permission/recoveries", err)
				}
			})
		}
	}
}

func TestLegacySupportProfilesNeverEnableApproval(t *testing.T) {
	for schema := 1; schema <= 3; schema++ {
		d := supportDefinitionFixture()
		d.SchemaVersion = schema
		if schema >= 2 {
			d = recoveryDefinitionFixture()
			d.SchemaVersion, d.Program.RecoveryPolicy = schema, ""
		}
		if schema == 3 {
			d.Program.RecoveryPolicy = ConfirmedUncommittedRecovery
		}
		p, err := BuildSupportProfile("legacy-approval-boundary", d)
		if err != nil || p.ApprovalEnabled() {
			t.Fatal("legacy profile acquired write capability", schema, err)
		}
	}
}
