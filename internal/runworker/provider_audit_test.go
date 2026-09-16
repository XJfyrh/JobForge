package runworker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/runexecutor"
	v2 "github.com/xjfyrh/jobforge/internal/runprotocol/v2"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

func TestAuditedWorkerRejectsLegacyOrDriftedProfile(t *testing.T) {
	profile := run.Profile{ID: "audit-test", Hash: strings.Repeat("a", 64), ExecutorVersion: run.ProviderAuditExecutorVersion,
		ProviderAuditPolicy: run.ProviderAuditPolicyDeepSeekV1, ExpectedResponseModel: "deepseek-flash", MaxInputTokens: 100, MaxOutputTokens: 1024,
		Pricing: run.Pricing{Hash: strings.Repeat("b", 64), Denominator: 1}}
	manifest := Manifest{SchemaVersion: 1, ExecutorVersion: run.ProviderAuditExecutorVersion,
		Profiles: []ManifestProfile{{ProfileID: profile.ID, ProfileHash: profile.Hash, AdapterID: "support-fixed-v1"}}}
	for _, mode := range []string{"valid", "legacy_version", "absent_policy", "wrong_policy", "absent_model", "different_model"} {
		t.Run(mode, func(t *testing.T) {
			p := profile
			switch mode {
			case "legacy_version":
				p.ExecutorVersion = "linux-v2-ack-runtime-1"
			case "absent_policy":
				p.ProviderAuditPolicy = ""
			case "wrong_policy":
				p.ProviderAuditPolicy = "another-policy"
			case "absent_model":
				p.ExpectedResponseModel = ""
			case "different_model":
				p.ExpectedResponseModel = "another-model"
			}
			_, err := New(&coordinatorClient{}, manifest, Config{Profiles: []run.Profile{p}, Environments: map[string]runexecutor.Environment{"tenant": {}}})
			if (mode == "valid") != (err == nil) {
				t.Fatalf("profile gate result: %v", err)
			}
		})
	}
}

func TestUnconfirmedChatReserveStopsWorkerWithoutInventingReservation(t *testing.T) {
	c, _, process := coordinatorFixture(t, "model_proposal")
	call := &callRecord{id: "00000000-0000-4000-8000-000000000006", intent: v2.Frame{Subcall: "chat"}}
	c.calls[call.id] = call
	c.complete(context.Background(), completion{kind: "reserve", callID: call.id, err: status.Error(codes.Unavailable, "synthetic uncertainty")})
	outcome := c.finish(context.Background(), cleanReceipt())
	if !c.fatal || !process.stopped.Load() || call.reservation != nil || !errors.Is(outcome.Fatal, ErrBatchStopped) {
		t.Fatal("unconfirmed Reserve ACK retained local Claim rights or invented a reservation")
	}
}

func TestUnconfirmedChatCommitStopsWorkerAfterNarrowRead(t *testing.T) {
	for _, persisted := range []bool{false, true} {
		c, client, _ := coordinatorFixture(t, "model_proposal")
		confirmedFixtureCall(t, c, true)
		coordinatorResult(t, c, false)
		var accepted *agentv1.AcceptedStep
		client.commit = func(_ context.Context, request *agentv1.CommitStepRequest) (*agentv1.CommitStepResponse, error) {
			accepted = successfulCommit(request).AcceptedStep
			return nil, status.Error(codes.Unavailable, "synthetic uncertainty")
		}
		reads := 0
		client.lookup = func(_ context.Context, _ *agentv1.GetAcceptedCommitRequest) (*agentv1.GetAcceptedCommitResponse, error) {
			reads++
			return &agentv1.GetAcceptedCommitResponse{Found: persisted, AcceptedStep: accepted}, nil
		}
		outcome := c.finish(context.Background(), cleanReceipt())
		if reads != 1 || !errors.Is(outcome.Fatal, ErrBatchStopped) || outcome.Commit != nil {
			t.Fatal("narrow commit read granted new local execution")
		}
	}
}

func fixtureAudit() *run.ProviderAudit {
	id, model, fingerprint, digest, created := "synthetic-chat", "deepseek-flash", "fp-synthetic", strings.Repeat("e", 64), int64(1)
	a := &run.ProviderAudit{SchemaVersion: 1, Provider: "deepseek", ResponseComplete: true, HTTPStatus: 200, ResponseSHA256: &digest,
		IdentityState: run.ProviderIdentityCompatible, ResponseID: &id, ResponseModel: &model, SystemFingerprint: &fingerprint, Created: &created,
		UsageEvidence: run.UsageEvidenceComplete, ReasoningState: run.ReasoningAbsent, ModeState: run.ProviderModeNonthinking}
	a.AuditHash = a.Hash()
	return a
}

func completeFixtureReport(t testing.TB, f *v2.Frame) {
	t.Helper()
	if f.ProviderAudit == nil {
		f.ProviderAudit = fixtureAudit()
	}
	f.ProviderAudit.AuditHash = f.ProviderAudit.Hash()
	if f.Usage != nil {
		hash, err := f.ProviderAudit.ReceiptHash(f.PhysicalCallID)
		if err != nil {
			t.Fatal(err)
		}
		f.Usage.ReceiptHash = hash
		f.Usage.UsageHash = f.Usage.Hash()
	}
	binding, err := v2.ReportBinding(*f, "deepseek-flash")
	if err != nil {
		t.Fatal(err)
	}
	f.ReportHash = v2.Report(*f).Hash(binding)
}

func fixtureSettledResponse(r *agentv1.CallReservation, report *v2.Frame) *agentv1.SettleUsageResponse {
	r.PersistedReportHash, r.PersistedAuditHash = report.ReportHash, reportAuditHash(report)
	return &agentv1.SettleUsageResponse{Reservation: r, PersistedReportHash: r.PersistedReportHash, PersistedAuditHash: r.PersistedAuditHash}
}

func TestCoordinatorUnknownChatHasNoOrdinaryACK(t *testing.T) {
	c, _, process, call, observation := activeCoordinator(t, "model_proposal", false)
	c.event(context.Background(), runexecutor.Event{Kind: runexecutor.FrameReceived, Channel: runexecutor.Ordinary, Frame: &observation})
	if !c.fatal || !c.stopped || !c.conversation.Closed() || c.tasks != 0 || call.confirmed || len(process.writes) != 0 {
		t.Fatal("unknown chat retained worker continuation or generated an ordinary ACK")
	}
}

func TestCoordinatorReportConfirmationCannotTrustEchoOrRecorded(t *testing.T) {
	for _, mode := range []string{"wrong_report", "wrong_audit", "reservation_report", "conflict", "unconfirmed", "recorded", "mode_stop"} {
		t.Run(mode, func(t *testing.T) {
			c, client, process, call, observation := activeCoordinator(t, "model_proposal", true)
			report := c.base("metering_report", observation.EmittedMonoMS)
			report.CallSequence, report.PhysicalCallID, report.ParameterHash = 1, call.id, call.intent.ParameterHash
			report.Usage = &v2.Usage{InputTokens: 1, OutputTokens: 1}
			completeFixtureReport(t, &report)
			if mode == "recorded" {
				report.Usage = nil
				report.ProviderAudit.UsageEvidence = run.UsageEvidenceAbsent
				report.ProviderAudit.ReasoningState = run.ReasoningUnavailable
				report.ProviderAudit.ModeState = run.ProviderModeUnavailable
				completeFixtureReport(t, &report)
			}
			if mode == "mode_stop" {
				report.ProviderAudit.ModeState = run.ProviderModeUnexpected
				completeFixtureReport(t, &report)
			}
			client.settle = func(_ context.Context, request *agentv1.SettleUsageRequest) (*agentv1.SettleUsageResponse, error) {
				if request.ReportHash != report.ReportHash || !proto.Equal(request.ProviderAudit, providerAuditToWire(report.ProviderAudit)) {
					t.Error("report content changed")
				}
				r := proto.Clone(call.reservation).(*agentv1.CallReservation)
				r.UsageKnown = true
				response := fixtureSettledResponse(r, &report)
				switch mode {
				case "wrong_report":
					response.PersistedReportHash = strings.Repeat("f", 64)
				case "wrong_audit":
					response.PersistedAuditHash = strings.Repeat("f", 64)
				case "reservation_report":
					response.Reservation.PersistedReportHash = strings.Repeat("f", 64)
				case "conflict":
					response.ReportConflict = true
					response.BatchFrozen = true
					response.BatchStopCode = "REPORT_CONFLICT"
				case "unconfirmed":
					return nil, nil
				case "recorded":
					r.UsageKnown = false
					response.BatchFrozen = true
					response.BatchStopCode = "CHAT_USAGE_UNKNOWN"
				case "mode_stop":
					response.BatchFrozen = true
					response.BatchStopCode = "PROVIDER_MODE_INVALID"
				}
				return response, nil
			}
			c.event(context.Background(), runexecutor.Event{Kind: runexecutor.FrameReceived, Channel: runexecutor.Metering, Frame: &report})
			joinCoordinatorTasks(t, c)
			if !c.fatal || !c.stopped || call.confirmed || call.settled {
				t.Fatal("unsafe report confirmation retained continuation")
			}
			ack := nextWritten(t, process)
			want := map[string]string{"conflict": "conflict", "recorded": "recorded", "mode_stop": "settled"}[mode]
			if want == "" {
				want = "unconfirmed"
			}
			if ack.Kind != "metering_ack" || ack.ReportHash != report.ReportHash || ack.Settlement != want {
				t.Fatal("incorrect report acknowledgement")
			}
		})
	}
}
