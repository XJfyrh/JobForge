package runprotocol

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestNestedPayloadSizeReportsFrameLimit(t *testing.T) {
	fixtures := loadFixtures(t)
	var execute, result Frame
	for _, raw := range fixtures.ValidFrames {
		frame := decodeFixture(t, raw)
		if frame.Kind == "execute_step" {
			execute = frame
		}
		if frame.Kind == "step_result" {
			result = frame
		}
	}
	for _, tc := range []struct {
		name, field, stepKind string
		frame                 Frame
		limit                 int
	}{
		{"checkpoint", "checkpoint", "model_proposal", execute, MaxCheckpointBytes},
		{"input", "input", "model_proposal", execute, 16384},
		{"model_result", "result", "model_proposal", result, 16384},
		{"tool_result", "result", "get_order", result, 8192},
		{"local_result", "result", "read_ticket", result, 8192},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := tc.frame
			frame.Binding.StepKind = tc.stepKind
			encoded, err := Encode(frame)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			for _, size := range []int{tc.limit, tc.limit + 1} {
				const prefix, suffix = `{"padding":"`, `"}`
				payload := json.RawMessage(prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix)
				fields[tc.field] = payload
				wire, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				_, decodeErr := Decode(append(wire, '\n'))
				switch tc.field {
				case "checkpoint":
					frame.Checkpoint = payload
				case "input":
					frame.Input = payload
				case "result":
					frame.Result = payload
				}
				_, encodeErr := Encode(frame)
				if size == tc.limit {
					if decodeErr != nil || encodeErr != nil {
						t.Fatalf("legal boundary rejected: decode=%v encode=%v", decodeErr, encodeErr)
					}
				} else if !errors.Is(decodeErr, ErrFrameLimit) || !errors.Is(encodeErr, ErrFrameLimit) {
					t.Fatalf("nested oversize lost fixed classification: decode=%v encode=%v", decodeErr, encodeErr)
				}
			}
		})
	}
}
