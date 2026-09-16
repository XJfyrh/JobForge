package runprotocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/xjfyrh/jobforge/internal/run"
)

func observationACK(t testing.TB, observation Frame, emitted int64) Frame {
	t.Helper()
	hash, err := ObservationHash(observation)
	if err != nil {
		t.Fatal(err)
	}
	return Frame{Version: 2, Kind: "call_observation_ack", RequestID: observation.RequestID,
		Binding: observation.Binding, EmittedMonoMS: emitted, CallSequence: observation.CallSequence,
		PhysicalCallID: observation.PhysicalCallID, ObservationHash: hash}
}

func observationFrames(t testing.TB) map[string]Frame {
	t.Helper()
	frames := map[string]Frame{}
	for _, raw := range loadFixtures(t).ValidFrames {
		f := decodeFixture(t, raw)
		f.EmittedMonoMS = 1000
		if _, exists := frames[f.Kind]; !exists {
			frames[f.Kind] = f
		}
	}
	return frames
}

func observationConversation(t testing.TB, frames map[string]Frame) *Conversation {
	t.Helper()
	c := &Conversation{}
	for _, kind := range []string{"execute_step", "call_intent", "call_permit"} {
		if err := c.Accept(frames[kind], 1000); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.CanDispatch(frames["call_permit"].PhysicalCallID, 1000); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestObservationErrorCodeClosedMapping(t *testing.T) {
	for _, test := range []struct{ wire, domain string }{
		{"OUTPUT_INVALID", "MODEL_PROTOCOL_ERROR"}, {"INPUT_INVALID", "INVALID_ARGUMENT"},
		{"PROTOCOL_ERROR", "EXECUTOR_PROTOCOL_ERROR"}, {"", ""}, {"TIMEOUT", "TIMEOUT"},
		{"DEPENDENCY_UNAVAILABLE", "DEPENDENCY_UNAVAILABLE"}, {"PROFILE_UNAVAILABLE", "PROFILE_UNAVAILABLE"},
		{"BUDGET_EXHAUSTED", "BUDGET_EXHAUSTED"},
	} {
		if got, err := ObservationErrorCode(test.wire); err != nil || got != test.domain {
			t.Fatalf("mapping %q: got %q error=%v", test.wire, got, err)
		}
	}
	for _, code := range []string{"STOP_REQUESTED", "STALE_LEASE", "CALL_CONFLICT", "MODEL_PROTOCOL_ERROR", "HTTP_ERROR", "CHECKPOINT_TOO_LARGE", "private-text"} {
		if got, err := ObservationErrorCode(code); got != "" || !errors.Is(err, ErrProtocol) {
			t.Fatalf("accepted non-observation code %q", code)
		}
	}
}

func TestSharedObservationHashesMatchLedger(t *testing.T) {
	fixture := loadFixtures(t)
	if len(fixture.ObservationHashCases) == 0 {
		t.Fatal("shared observation hash cases are required")
	}
	usageByHash := map[string]run.UsageReport{}
	for _, raw := range fixture.ValidFrames {
		frame := decodeFixture(t, raw)
		if frame.Kind == "metering_report" {
			u := frame.Usage
			usageByHash[u.UsageHash] = run.UsageReport{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
				CachedInputTokens: u.CachedInputTokens, ReceiptHash: u.ReceiptHash, UsageHash: u.UsageHash}
		}
	}
	for _, test := range fixture.ObservationHashCases {
		t.Run(test.Name, func(t *testing.T) {
			var compact bytes.Buffer
			if err := json.Compact(&compact, test.Frame); err != nil {
				t.Fatal(err)
			}
			frame, err := DecodeOrdinary(append(compact.Bytes(), '\n'))
			var hash string
			if err == nil {
				hash, err = ObservationHash(frame)
			}
			if !test.Accept {
				if !errors.Is(err, ErrProtocol) {
					t.Fatalf("invalid hash case: %v", err)
				}
				return
			}
			if err != nil || hash != test.Hash {
				t.Fatalf("hash got=%s want=%s error=%v", hash, test.Hash, err)
			}
			code, err := ObservationErrorCode(frame.ErrorCode)
			if err != nil || code != test.DomainError {
				t.Fatalf("domain code got=%q want=%q error=%v", code, test.DomainError, err)
			}
			request := run.ObserveCallRequest{PhysicalCallID: frame.PhysicalCallID,
				TransportOutcome: frame.TransportOutcome, HTTPStatus: int(frame.HTTPStatus),
				BusinessOutcome: frame.BusinessOutcome, ErrorCode: code}
			if frame.UsageDisposition == "reported" {
				u, exists := usageByHash[*frame.UsageHash]
				if !exists {
					t.Fatal("reported hash case has no complete shared usage report")
				}
				request.UsageKnown, request.Usage = true, &u
			}
			if err = request.Validate(); err != nil {
				t.Fatalf("ledger request validation: %v", err)
			}
			auditHash := ""
			if frame.AuditHash != nil {
				auditHash = *frame.AuditHash
			}
			ledgerHash, ledgerErr := run.ObservationHashV2(request, auditHash)
			if ledgerErr != nil || ledgerHash != hash {
				t.Fatal("wire and ledger observation hashes differ")
			}
			if code != frame.ErrorCode {
				request.ErrorCode = frame.ErrorCode
				unmapped, _ := run.ObservationHashV2(request, auditHash)
				if unmapped == hash {
					t.Fatal("wire error was not mapped before hashing")
				}
			}
		})
	}
}

func TestObservationHashRequiresStrictObservation(t *testing.T) {
	for _, mutate := range []struct {
		name  string
		apply func(*Frame)
	}{
		{"wrong_kind", func(f *Frame) { f.Kind = "step_result" }},
		{"wrong_version", func(f *Frame) { f.Version = 1 }},
		{"invalid_binding", func(f *Frame) { f.Binding.AttemptNo = 0 }},
		{"control_error", func(f *Frame) { f.ErrorCode, f.BusinessOutcome = "STOP_REQUESTED", "rejected" }},
		{"missing_usage_hash", func(f *Frame) { f.UsageHash = nil }},
		{"inconsistent_outcome", func(f *Frame) { f.ErrorCode = "OUTPUT_INVALID" }},
		{"invalid_clock", func(f *Frame) { f.EmittedMonoMS = MaxInteger + 1 }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			f := observationFrames(t)["call_observation"]
			mutate.apply(&f)
			if hash, err := ObservationHash(f); hash != "" || !errors.Is(err, ErrProtocol) {
				t.Fatalf("invalid observation hashed: %s %v", hash, err)
			}
		})
	}
}

func TestObservationACKSnapshotWaitsForMetering(t *testing.T) {
	frames := observationFrames(t)
	c := observationConversation(t, frames)
	observation := frames["call_observation"]
	observation.EmittedMonoMS = 1010
	if err := c.Accept(observation, 1010); err != nil {
		t.Fatal(err)
	}
	ack := observationACK(t, observation, 1020)
	if err := c.Accept(ack, 1020); err != nil {
		t.Fatal(err)
	}
	if !c.MeteringPending() || c.callIndex != 0 {
		t.Fatal("ordinary ACK bypassed pending metering")
	}
	ack.EmittedMonoMS, ack.ObservationHash, ack.Binding.AttemptNo = 0, strings.Repeat("f", 64), 99
	report := frames["metering_report"]
	report.EmittedMonoMS = 1005
	if err := c.AcceptMetering(report, 1025); err != nil {
		t.Fatal(err)
	}
	meteringACK := frames["metering_ack"]
	meteringACK.EmittedMonoMS = 1015
	if err := c.AcceptMetering(meteringACK, 1030); err != nil {
		t.Fatal(err)
	}
	if c.Closed() || c.MeteringPending() || c.callIndex != 1 {
		t.Fatal("caller mutation changed the pending ACK snapshot")
	}
	result := frames["step_result"]
	result.EmittedMonoMS = 1020
	if err := c.Accept(result, 1040); err != nil {
		t.Fatal(err)
	}
}

func TestObservationACKCausalOrderAcrossBothArrivalOrders(t *testing.T) {
	for _, ordinaryFirst := range []bool{false, true} {
		name := "settlement_first"
		if ordinaryFirst {
			name = "ordinary_first"
		}
		t.Run(name, func(t *testing.T) {
			frames := observationFrames(t)
			c := observationConversation(t, frames)
			if err := c.Accept(frames["call_observation"], 1000); err != nil {
				t.Fatal(err)
			}
			if err := c.AcceptMetering(frames["metering_report"], 1000); err != nil {
				t.Fatal(err)
			}
			ack := observationACK(t, frames["call_observation"], 1010)
			meteringACK := frames["metering_ack"]
			meteringACK.EmittedMonoMS = 1020
			var err error
			if ordinaryFirst {
				if err = c.Accept(ack, 1010); err != nil {
					t.Fatal(err)
				}
				err = c.AcceptMetering(meteringACK, 1020)
			} else {
				if err = c.AcceptMetering(meteringACK, 1020); err != nil {
					t.Fatal(err)
				}
				err = c.Accept(ack, 1025)
			}
			if !errors.Is(err, ErrProtocol) || !c.Closed() || c.pendingAck != nil {
				t.Fatal("ACK issued before settlement retained execution rights")
			}
			call := c.metering.calls[ack.PhysicalCallID]
			if call.report == nil || call.settlement != "settled" {
				t.Fatal("ordinary causal failure discarded independently accepted metering")
			}
			if err = c.AcceptMetering(meteringACK, 1030); err != nil {
				t.Fatal(err)
			}
			if err = c.Accept(frames["step_result"], 1030); !errors.Is(err, ErrProtocol) {
				t.Fatal("late metering confirmation reopened execution")
			}
		})
	}
}

func TestMeteringPendingExcludesOrdinaryACKWait(t *testing.T) {
	for _, reported := range []bool{false, true} {
		name := "unknown"
		if reported {
			name = "reported"
		}
		t.Run(name, func(t *testing.T) {
			frames := observationFrames(t)
			if !reported {
				freeObservationFrames(frames)
			}
			c := observationConversation(t, frames)
			observation := frames["call_observation"]
			if !reported {
				observation.UsageDisposition, observation.UsageHash = "unknown", nil
			}
			if err := c.Accept(observation, 1000); err != nil {
				t.Fatal(err)
			}
			if c.MeteringPending() != reported {
				t.Fatal("metering pending does not reflect observation disposition")
			}
			if reported {
				for _, kind := range []string{"metering_report", "metering_ack"} {
					if err := c.AcceptMetering(frames[kind], 1000); err != nil {
						t.Fatal(err)
					}
				}
			}
			if c.MeteringPending() || c.phase != "observation" || c.callIndex != 0 {
				t.Fatal("settled or free observation skipped ordinary ACK wait")
			}
			if err := c.Accept(observationACK(t, observation, 1000), 1000); err != nil {
				t.Fatal(err)
			}
			if c.MeteringPending() || c.phase != "idle" || c.callIndex != 1 {
				t.Fatal("ACK failed to finish exactly one observation")
			}
		})
	}
}

func TestAbandonedObservationACKDoesNotBlockLateSettlement(t *testing.T) {
	frames := observationFrames(t)
	c := observationConversation(t, frames)
	if err := c.Accept(frames["call_observation"], 1000); err != nil {
		t.Fatal(err)
	}
	ack := observationACK(t, frames["call_observation"], 1010)
	if err := c.Accept(ack, 1010); err != nil {
		t.Fatal(err)
	}
	if err := c.AcceptMetering(frames["metering_report"], 1015); err != nil {
		t.Fatal(err)
	}
	meteringACK := frames["metering_ack"]
	meteringACK.Settlement, meteringACK.EmittedMonoMS = "unconfirmed", 1020
	if err := c.AcceptMetering(meteringACK, 1020); err != nil {
		t.Fatal(err)
	}
	if !c.Closed() || c.pendingAck != nil {
		t.Fatal("unconfirmed settlement retained pending execution rights")
	}
	// The later settlement may be emitted after the discarded ordinary ACK.
	// Its only effect is to settle the original report, never to revive output.
	meteringACK.Settlement, meteringACK.EmittedMonoMS = "settled", 1030
	if err := c.AcceptMetering(meteringACK, 1030); err != nil {
		t.Fatal(err)
	}
	if !c.Closed() || c.MeteringPending() || c.metering.calls[ack.PhysicalCallID].settlement != "settled" {
		t.Fatal("late settlement lost its narrow accounting effect")
	}
	if err := c.Accept(ack, 1040); !errors.Is(err, ErrProtocol) {
		t.Fatal("late ordinary ACK reopened execution")
	}
	if err := c.Accept(frames["step_result"], 1040); !errors.Is(err, ErrProtocol) {
		t.Fatal("late settlement authorized a result")
	}
}

func TestObservationACKSuccessorKeepsOriginalCallDeadline(t *testing.T) {
	frames := observationFrames(t)
	freeObservationFrames(frames)
	permit := frames["call_permit"]
	permit.CallMS, permit.DispatchMS = 100, 50
	frames["call_permit"] = permit
	c := observationConversation(t, frames)
	observation := frames["call_observation"]
	observation.UsageDisposition, observation.UsageHash = "unknown", nil
	if err := c.Accept(observation, 1000); err != nil {
		t.Fatal(err)
	}
	if err := c.Accept(observationACK(t, observation, 1000), 1000); err != nil {
		t.Fatal(err)
	}
	if err := c.Accept(frames["step_result"], 1100); !errors.Is(err, ErrProtocol) {
		t.Fatal("result reception extended the acknowledged call deadline")
	}
}

func TestObservationACKNextIntentDeadlineDoesNotBindNewPermit(t *testing.T) {
	for _, nextIntentNow := range []int64{1099, 1100} {
		name := "before_expiry"
		if nextIntentNow == 1100 {
			name = "at_expiry"
		}
		t.Run(name, func(t *testing.T) {
			frames := observationFrames(t)
			for kind, frame := range frames {
				frame.Binding.StepKind = "search_policy"
				frame.AuditHash = nil
				if oneOf(kind, "call_intent", "call_permit") {
					frame.Subcall, frame.ToolInvocationID = "profile_version", "00000000-0000-4000-8000-000000000099"
				}
				if kind == "call_permit" {
					frame.CallMS, frame.DispatchMS = 100, 50
					frame.InputTokenLimit, frame.OutputTokenLimit = 0, 0
				}
				frames[kind] = frame
			}
			c := observationConversation(t, frames)
			observation := frames["call_observation"]
			observation.UsageDisposition, observation.UsageHash = "unknown", nil
			if err := c.Accept(observation, 1000); err != nil {
				t.Fatal(err)
			}
			if err := c.Accept(observationACK(t, observation, 1000), 1000); err != nil {
				t.Fatal(err)
			}
			intent := frames["call_intent"]
			intent.Subcall, intent.CallSequence, intent.EmittedMonoMS = "profile_tags", 2, 1099
			err := c.Accept(intent, nextIntentNow)
			if nextIntentNow == 1100 {
				if !errors.Is(err, ErrProtocol) || !c.Closed() {
					t.Fatal("late next intent bypassed previous call deadline")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			permit := frames["call_permit"]
			permit.Subcall, permit.CallSequence, permit.EmittedMonoMS = "profile_tags", 2, 1120
			permit.PhysicalCallID = "00000000-0000-4000-8000-000000000088"
			if err = c.Accept(permit, 1120); err != nil {
				t.Fatal("previous call deadline leaked into new permit", err)
			}
			if err = c.CanDispatch(permit.PhysicalCallID, 1121); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func freeObservationFrames(frames map[string]Frame) {
	for kind, f := range frames {
		f.Binding.StepKind = "get_order"
		f.AuditHash = nil
		if oneOf(kind, "call_intent", "call_permit") {
			f.Subcall, f.ToolInvocationID = "get_order", "00000000-0000-4000-8000-000000000099"
		}
		if kind == "call_permit" {
			f.CallMS, f.DispatchMS, f.InputTokenLimit, f.OutputTokenLimit = 10000, 1000, 0, 0
		}
		frames[kind] = f
	}
}

func TestEmbeddingUnknownOrdinaryACKAllowsNextSearch(t *testing.T) {
	frames := observationFrames(t)
	execute := frames["execute_step"]
	execute.Binding.StepKind = "search_policy"
	c := &Conversation{}
	if err := c.Accept(execute, 1000); err != nil {
		t.Fatal(err)
	}
	for index, subcall := range subcalls("search_policy") {
		intent := frames["call_intent"]
		intent.Binding, intent.Subcall, intent.CallSequence = execute.Binding, subcall, int64(index+1)
		intent.ToolInvocationID = "00000000-0000-4000-8000-000000000099"
		if err := c.Accept(intent, 1000); err != nil {
			t.Fatal(err)
		}
		permit := intent
		permit.Kind, permit.Granted = "call_permit", true
		permit.PhysicalCallID = fmt.Sprintf("00000000-0000-4000-8000-%012d", index+1)
		permit.CallMS, permit.DispatchMS = 10000, 1000
		if subcall == "query_embedding" {
			permit.InputTokenLimit = 100
		}
		if err := c.Accept(permit, 1000); err != nil {
			t.Fatal(err)
		}
		if err := c.CanDispatch(permit.PhysicalCallID, 1000); err != nil {
			t.Fatal(err)
		}
		observation := frames["call_observation"]
		observation.Binding, observation.CallSequence, observation.PhysicalCallID = execute.Binding, permit.CallSequence, permit.PhysicalCallID
		observation.UsageDisposition, observation.UsageHash, observation.AuditHash = "unknown", nil, nil
		if err := c.Accept(observation, 1000); err != nil {
			t.Fatal(err)
		}
		if c.Closed() || c.MeteringPending() || c.metering.calls[permit.PhysicalCallID].report != nil {
			t.Fatal("unknown embedding manufactured a report or acquired chat stop semantics")
		}
		if err := c.Accept(observationACK(t, observation, 1000), 1000); err != nil {
			t.Fatal(err)
		}
	}
	result := frames["step_result"]
	result.Binding = execute.Binding
	if err := c.Accept(result, 1000); err != nil {
		t.Fatal("ordinary embedding ACK did not allow the fixed search successor", err)
	}
}
