package agentv1_test

import (
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

func TestWorkerServiceHasTwelveTypedOperations(t *testing.T) {
	service := agentv1.File_jobforge_agent_v1_agent_proto.Services().ByName("AgentService")
	names := []string{"Register", "Claim", "Heartbeat", "GetCheckpoint", "BeginTool", "ReserveCall", "ObserveCall", "SettleUsage", "CommitStep", "FailAttempt", "AcknowledgeStopped", "GetAcceptedCommit"}
	if service == nil || service.Methods().Len() != len(names) {
		t.Fatal("worker RPC contract drifted")
	}
	for _, name := range names {
		method := service.Methods().ByName(protoreflect.Name(name))
		if method == nil || method.IsStreamingClient() || method.IsStreamingServer() ||
			string(method.Input().Name()) != name+"Request" || string(method.Output().Name()) != name+"Response" {
			t.Fatalf("RPC %s is not a dedicated unary contract", name)
		}
	}
	request := (&agentv1.ReserveCallRequest{}).ProtoReflect().Descriptor()
	for _, forbidden := range []string{"url", "method", "cost_microyuan", "input_token_limit", "next_cursor", "payload"} {
		if request.Fields().ByName(protoreflect.Name(forbidden)) != nil {
			t.Fatalf("worker request controls forbidden field %s", forbidden)
		}
	}
	for _, name := range []string{"physical_call_id", "tool_invocation_id", "subcall", "parameter_hash", "price_hash", "execution", "step"} {
		if request.Fields().ByName(protoreflect.Name(name)) == nil {
			t.Fatalf("reservation identity missing %s", name)
		}
	}
}

func TestHeartbeatSupportsIdleAndActiveSessionBindings(t *testing.T) {
	for _, wire := range []string{
		`{"session":{"workerId":"worker-a","sessionId":"00000000-0000-4000-8000-000000000001"}}`,
		`{"session":{"workerId":"worker-a","sessionId":"00000000-0000-4000-8000-000000000001"},"execution":{"tenantId":"tenant-a","runId":"00000000-0000-4000-8000-000000000002","session":{"workerId":"worker-a","sessionId":"00000000-0000-4000-8000-000000000001"},"attemptNo":"1","fencingToken":"1"}}`,
	} {
		var request agentv1.HeartbeatRequest
		if err := protojson.Unmarshal([]byte(wire), &request); err != nil {
			t.Fatal(err)
		}
		if request.Session == nil || (request.Execution != nil && !proto.Equal(request.Execution.Session, request.Session)) {
			t.Fatal("heartbeat fixture lost the original session binding")
		}
		binary, err := proto.Marshal(&request)
		if err != nil {
			t.Fatal(err)
		}
		var roundtrip agentv1.HeartbeatRequest
		if err = proto.Unmarshal(binary, &roundtrip); err != nil || !proto.Equal(&request, &roundtrip) {
			t.Fatalf("heartbeat optional execution changed: %v", err)
		}
	}
}

func TestAuditProtocolRetainsExistingFieldNumbers(t *testing.T) {
	for _, test := range []struct {
		message proto.Message
		fields  []string
	}{
		{&agentv1.SettleUsageRequest{}, []string{"execution", "physical_call_id", "usage", "provider_audit", "report_hash"}},
		{&agentv1.SettleUsageResponse{}, []string{"newly_settled", "reservation", "persisted_report_hash", "persisted_audit_hash", "report_conflict", "batch_frozen", "batch_stop_code"}},
		{&agentv1.CallReservation{}, []string{"physical_call_id", "tool_invocation_id", "subcall", "parameter_hash", "price_hash", "reserved_at", "dispatch_expires_at", "call_deadline", "budget", "usage_known", "measurement_anomaly", "execution_binding_hash", "persisted_report_hash", "persisted_audit_hash"}},
		{&agentv1.ObserveCallRequest{}, []string{"execution", "step", "physical_call_id", "transport_outcome", "http_status", "error_code", "usage", "business_outcome", "usage_known", "audit_hash"}},
	} {
		descriptor := test.message.ProtoReflect().Descriptor()
		for index, name := range test.fields {
			field := descriptor.Fields().ByName(protoreflect.Name(name))
			if field == nil || field.Number() != protoreflect.FieldNumber(index+1) {
				t.Fatalf("append-only RPC contract changed %s.%s", descriptor.Name(), name)
			}
		}
	}
}

func TestAuditNullableWirePresenceSurvivesBinaryRoundtrip(t *testing.T) {
	original := &agentv1.ProviderAudit{Created: proto.Int64(0), ReasoningTokens: proto.Int64(0), ResponseId: proto.String("")}
	wire, err := proto.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded agentv1.ProviderAudit
	if err = proto.Unmarshal(wire, &decoded); err != nil || !proto.Equal(original, &decoded) || decoded.Created == nil || decoded.ReasoningTokens == nil || decoded.ResponseId == nil || decoded.ResponseModel != nil {
		t.Fatalf("audit nullable values collapsed: %v", err)
	}
	fields := decoded.ProtoReflect().Descriptor().Fields()
	for _, name := range []string{"response_sha256", "response_id", "response_model", "system_fingerprint", "created", "reasoning_tokens"} {
		if !fields.ByName(protoreflect.Name(name)).HasOptionalKeyword() {
			t.Fatalf("audit nullable %s lost explicit presence", name)
		}
	}
}
