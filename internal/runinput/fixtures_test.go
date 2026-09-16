package runinput

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	runprotocol "github.com/xjfyrh/jobforge/internal/runprotocol/v2"
)

func makeFixtures(t *testing.T) fixtureFile {
	t.Helper()
	vectors := fixtureFile{SchemaVersion: 1, Valid: []fixture{}, Invalid: []fixture{}, InvalidJSON: []rawFixture{}}
	frames := map[string][]byte{}
	for _, kind := range []string{"read_ticket", "get_order", "get_delivery", "search_policy", "model_proposal", "submit_proposal"} {
		line, err := runprotocol.Encode(exampleFrame(t, kind))
		if err != nil {
			t.Fatal(err)
		}
		frames[kind] = bytes.TrimSuffix(line, []byte("\n"))
		vectors.Valid = append(vectors.Valid, fixture{Name: kind, Frame: frames[kind]})
	}
	mutate := func(name, kind, path string, value any) {
		var object map[string]any
		decoder := json.NewDecoder(bytes.NewReader(frames[kind]))
		decoder.UseNumber()
		if decoder.Decode(&object) != nil {
			t.Fatal("cannot decode synthetic vector")
		}
		parts := strings.Split(path, ".")
		var target any = object
		for _, key := range parts[:len(parts)-1] {
			switch node := target.(type) {
			case map[string]any:
				target = node[key]
			case []any:
				index, err := strconv.Atoi(key)
				if err != nil {
					t.Fatal(err)
				}
				target = node[index]
			}
		}
		last := parts[len(parts)-1]
		target.(map[string]any)[last] = value
		vectors.Invalid = append(vectors.Invalid, fixture{Name: name, Frame: marshal(t, object)})
	}
	mutate("input_extra_parameters", "read_ticket", "input.parameters", map[string]any{})
	mutate("input_null_adapter", "read_ticket", "input.adapter_id", nil)
	mutate("input_schema_boolean", "read_ticket", "input.schema_version", true)
	mutate("input_schema_float", "read_ticket", "input.schema_version", json.Number("1.0"))
	mutate("input_schema_string", "read_ticket", "input.schema_version", "1")
	mutate("mixed_executor_version", "read_ticket", "input.executor_version", "linux-v2-runtime-0")
	mutate("invalid_adapter_identifier", "read_ticket", "input.adapter_id", "module/path")
	mutate("read_ticket_has_tool_id", "read_ticket", "input.tool_invocation_id", uuidAt(100))
	mutate("search_missing_tool_id", "search_policy", "input.tool_invocation_id", "")
	mutate("model_has_tool_id", "model_proposal", "input.tool_invocation_id", uuidAt(100))
	mutate("checkpoint_extra_field", "read_ticket", "checkpoint.extra", true)
	mutate("null_next_step", "read_ticket", "checkpoint.next_step", nil)
	mutate("null_steps", "read_ticket", "checkpoint.steps", nil)
	mutate("protojson_cursor_string", "read_ticket", "checkpoint.cursor_version", "0")
	mutate("protojson_sequence_string", "read_ticket", "checkpoint.next_step.sequence", "1")
	mutate("null_cursor", "read_ticket", "checkpoint.cursor_version", nil)
	mutate("cursor_out_of_range", "read_ticket", "checkpoint.cursor_version", 32)
	mutate("next_binding_mismatch", "read_ticket", "checkpoint.next_step.step_id", uuidAt(888))
	mutate("snapshot_tenant_mismatch", "read_ticket", "checkpoint.snapshot.tenant_id", "other-tenant")
	mutate("snapshot_hash_mismatch", "read_ticket", "checkpoint.snapshot.snapshot_hash", strings.Repeat("f", 64))
	mutate("ticket_identity_mismatch", "read_ticket", "checkpoint.snapshot.ticket_binding_json.ticket_id", "other-ticket")
	mutate("ticket_null_revision", "read_ticket", "checkpoint.snapshot.ticket_binding_json.revision", nil)
	mutate("ticket_extra_field", "read_ticket", "checkpoint.snapshot.ticket_binding_json.endpoint", "https://invalid.local")
	mutate("vector_index_mismatch", "read_ticket", "checkpoint.snapshot.version_vector_json.index.id", uuidAt(900))
	mutate("vector_null_ticket", "read_ticket", "checkpoint.snapshot.version_vector_json.ticket", nil)
	mutate("missing_order_has_revision", "read_ticket", "checkpoint.snapshot.version_vector_json.order.revision", 1)
	mutate("wrong_result_ref", "search_policy", "checkpoint.steps.0.result_ref", "run-step:"+uuidAt(900)+":1")
	mutate("wrong_accepted_sequence", "search_policy", "checkpoint.steps.0.step.sequence", 2)
	mutate("wrong_accepted_resource", "search_policy", "checkpoint.steps.0.step.profile_hash", strings.Repeat("f", 64))
	mutate("wrong_commit_chain", "search_policy", "checkpoint.steps.0.commit_hash", strings.Repeat("f", 64))
	mutate("wrong_initial_input_hash", "search_policy", "checkpoint.steps.0.step.input_hash", strings.Repeat("f", 64))
	mutate("wrong_next_input_hash", "search_policy", "checkpoint.next_step.input_hash", strings.Repeat("f", 64))
	mutate("result_null_evidence", "search_policy", "checkpoint.steps.0.result_json.evidence_refs", nil)
	mutate("result_extra_field", "search_policy", "checkpoint.steps.0.result_json.extra", true)
	mutate("result_schema_boolean", "search_policy", "checkpoint.steps.0.result_json.schema_version", true)
	mutate("input_oversized", "read_ticket", "input.adapter_id", strings.Repeat("x", 16385))
	read := string(frames["read_ticket"])
	vectors.InvalidJSON = append(vectors.InvalidJSON,
		rawFixture{Name: "duplicate_input_schema", JSON: strings.Replace(read, `"input":{`, `"input":{"schema_version":1,`, 1) + "\n"},
		rawFixture{Name: "duplicate_checkpoint_cursor", JSON: strings.Replace(read, `"checkpoint":{"cursor_version":`, `"checkpoint":{"cursor_version":0,"cursor_version":`, 1) + "\n"},
		rawFixture{Name: "duplicate_ticket_id", JSON: strings.Replace(read, `"ticket_binding_json":{`, `"ticket_binding_json":{"ticket_id":"synthetic-ticket",`, 1) + "\n"},
	)
	return vectors
}
