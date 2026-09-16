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
