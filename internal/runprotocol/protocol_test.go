package runprotocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

type fixtures struct {
	ValidFrames   []json.RawMessage `json:"valid_frames"`
	InvalidFrames []struct {
		Name string `json:"name"`
		Wire string `json:"wire"`
	} `json:"invalid_frames"`
	ValidSequences []struct {
		Name   string            `json:"name"`
		Frames []json.RawMessage `json:"frames"`
	} `json:"valid_sequences"`
}

func loadFixtures(t *testing.T) fixtures {
	t.Helper()
	content, err := os.ReadFile("../../api/executor/v1/fixtures/frames.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture fixtures
	if err = json.Unmarshal(content, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func decodeFixture(t *testing.T, raw json.RawMessage) Frame {
	t.Helper()
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		t.Fatal(err)
	}
	frame, err := Decode(append(compact.Bytes(), '\n'))
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func TestSharedExecutorFixtures(t *testing.T) {
	fixture := loadFixtures(t)
	for _, raw := range fixture.ValidFrames {
		frame := decodeFixture(t, raw)
		t.Run(frame.Kind, func(t *testing.T) {
			wire, err := Encode(frame)
			if err != nil {
				t.Fatal(err)
			}
			roundtrip, err := Decode(wire)
			if err != nil || !reflect.DeepEqual(frame, roundtrip) {
				t.Fatalf("roundtrip mismatch: %v", err)
			}
		})
	}
	for _, test := range fixture.InvalidFrames {
		t.Run(test.Name, func(t *testing.T) {
			if _, err := Decode([]byte(test.Wire)); !errors.Is(err, ErrProtocol) {
				t.Fatalf("invalid frame accepted: %v", err)
			}
		})
	}
	for _, test := range fixture.ValidSequences {
		t.Run(test.Name, func(t *testing.T) {
			var conversation Conversation
			for index, raw := range test.Frames {
				frame := decodeFixture(t, raw)
				now := time.UnixMilli(int64(index * 10))
				if err := conversation.Accept(frame, now); err != nil {
					t.Fatalf("frame %d: %v", index, err)
				}
				if frame.Kind == "call_permit" && frame.Granted {
					if err := conversation.CanDispatch(frame.PhysicalCallID, now); err != nil {
						t.Fatal(err)
					}
				}
			}
			if !conversation.closed {
				t.Fatal("step_result left conversation open")
			}
		})
	}
}

func TestFrameAndProtectedDataBounds(t *testing.T) {
	start := decodeFixture(t, loadFixtures(t).ValidFrames[0])
	for _, size := range []int{MaxCheckpointBytes, MaxCheckpointBytes + 1} {
		start.Checkpoint = json.RawMessage(`{"value":"` + strings.Repeat("x", size-12) + `"}`)
		_, err := Encode(start)
		if (err == nil) != (size == MaxCheckpointBytes) {
			t.Fatalf("checkpoint size %d: %v", size, err)
		}
	}
	start.Checkpoint = json.RawMessage(`{}`)
	valid, err := Encode(start)
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{MaxFrameBytes, MaxFrameBytes + 1} {
		wire := append(bytes.Repeat([]byte(" "), size-len(valid)), valid...)
		_, err = Decode(wire)
		if (err == nil) != (size == MaxFrameBytes) {
			t.Fatalf("frame size %d: %v", size, err)
		}
	}
	if _, err = Decode(append([]byte{0xff}, valid...)); !errors.Is(err, ErrProtocol) {
		t.Fatal("invalid UTF-8 accepted")
	}
	start.Checkpoint = json.RawMessage(`{"value":` + strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65) + "}")
	if _, err = Encode(start); !errors.Is(err, ErrProtocol) {
		t.Fatal("excess nesting accepted")
	}
	result := decodeFixture(t, loadFixtures(t).ValidFrames[4])
	result.Binding.StepKind = "get_order"
	result.Result = json.RawMessage(`{"value":"` + strings.Repeat("x", 8192-12) + `"}`)
	if _, err = Encode(result); err != nil {
		t.Fatal(err)
	}
	result.Result = append(result.Result[:len(result.Result)-2], []byte(`x"}`)...)
	if _, err = Encode(result); !errors.Is(err, ErrProtocol) {
		t.Fatal("oversized tool output accepted")
	}
}

func TestReadFrameHasBoundedConsumption(t *testing.T) {
	frame := decodeFixture(t, loadFixtures(t).ValidFrames[0])
	wire, err := Encode(frame)
	if err != nil {
		t.Fatal(err)
	}
	reader := bytes.NewReader(append(append([]byte{}, wire...), wire...))
	for range 2 {
		if _, err = ReadFrame(reader); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = ReadFrame(reader); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF, got %v", err)
	}
	if _, err = ReadFrame(bytes.NewReader(wire[:len(wire)-1])); !errors.Is(err, ErrProtocol) {
		t.Fatal("unterminated frame accepted")
	}
	if _, err = ReadFrame(strings.NewReader(strings.Repeat("x", MaxFrameBytes+1))); !errors.Is(err, ErrFrameLimit) {
		t.Fatal("unbounded frame accepted")
	}
}

func TestSchemaFieldSetsMatchExplicitCodec(t *testing.T) {
	content, err := os.ReadFile("../../api/executor/v1/schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var source struct {
		MaxFrame int `json:"x-max-frame-bytes"`
		Defs     map[string]struct {
			Required   []string `json:"required"`
			Additional bool     `json:"additionalProperties"`
		} `json:"$defs"`
	}
	if err = json.Unmarshal(content, &source); err != nil {
		t.Fatal(err)
	}
	if source.MaxFrame != MaxFrameBytes {
		t.Fatal("schema frame bound drifted")
	}
	sets := map[string][]string{"binding": bindingFields, "usage": usageFields}
	for kind, fields := range kindFields {
		sets[kind] = append(append([]string{}, commonFields...), fields...)
	}
	for kind, fields := range sets {
		definition, ok := source.Defs[kind]
		if !ok || definition.Additional {
			t.Fatalf("schema %s is not a closed object", kind)
		}
		want := append([]string{}, fields...)
		sort.Strings(want)
		sort.Strings(definition.Required)
		if !reflect.DeepEqual(want, definition.Required) {
			t.Fatalf("schema %s field drift", kind)
		}
	}
}

func TestConversationRejectsIdentityOrderReplayAndExpiry(t *testing.T) {
	fixture := loadFixtures(t)
	frames := make([]Frame, len(fixture.ValidFrames))
	for index, raw := range fixture.ValidFrames {
		frames[index] = decodeFixture(t, raw)
	}
	tests := []struct {
		name   string
		before int
		change func(*Frame)
		at     time.Time
	}{
		{"wrong_request", 1, func(f *Frame) { f.RequestID = f.Binding.RunID }, time.UnixMilli(1)},
		{"wrong_session", 1, func(f *Frame) { f.Binding.SessionID = f.Binding.RunID }, time.UnixMilli(1)},
		{"wrong_run", 1, func(f *Frame) { f.Binding.RunID = f.Binding.StepID }, time.UnixMilli(1)},
		{"wrong_cursor", 1, func(f *Frame) { f.Binding.CursorVersion++ }, time.UnixMilli(1)},
		{"wrong_intent", 2, func(f *Frame) { f.ParameterHash = strings.Repeat("a", 64) }, time.UnixMilli(2)},
		{"wrong_call", 3, func(f *Frame) { f.PhysicalCallID = f.Binding.RunID }, time.UnixMilli(3)},
		{"excess_usage", 3, func(f *Frame) {
			f.Usage = &Usage{InputTokens: 101, ReceiptHash: strings.Repeat("e", 64), UsageHash: strings.Repeat("f", 64)}
		}, time.UnixMilli(3)},
		{"step_expired", 1, func(_ *Frame) {}, time.UnixMilli(180000)},
		{"call_expired", 3, func(_ *Frame) {}, time.UnixMilli(60002)},
		{"permit_exceeds_step", 2, func(_ *Frame) {}, time.UnixMilli(150000)},
		{"parallel_intent", 2, func(f *Frame) { *f = frames[1] }, time.UnixMilli(2)},
		{"early_result", 1, func(f *Frame) { *f = frames[4] }, time.UnixMilli(1)},
		{"extra_stdout", 5, func(f *Frame) { *f = frames[4] }, time.UnixMilli(5)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var conversation Conversation
			for index := range test.before {
				now := time.UnixMilli(int64(index))
				if err := conversation.Accept(frames[index], now); err != nil {
					t.Fatal(err)
				}
				if index == 2 {
					if err := conversation.CanDispatch(frames[index].PhysicalCallID, now); err != nil {
						t.Fatal(err)
					}
				}
			}
			frame := frames[min(test.before, 4)]
			test.change(&frame)
			if err := conversation.Accept(frame, test.at); !errors.Is(err, ErrProtocol) || !conversation.closed {
				t.Fatalf("invalid sequence did not fail closed: %v", err)
			}
		})
	}
	for _, scenario := range []string{"replay", "expiry", "stop", "observation_before_send"} {
		t.Run(scenario, func(t *testing.T) {
			var conversation Conversation
			for index := range 3 {
				if err := conversation.Accept(frames[index], time.UnixMilli(0)); err != nil {
					t.Fatal(err)
				}
			}
			now := time.UnixMilli(0)
			switch scenario {
			case "replay":
				if err := conversation.CanDispatch(frames[2].PhysicalCallID, now); err != nil {
					t.Fatal(err)
				}
			case "expiry":
				now = time.UnixMilli(30000)
			case "stop":
				conversation.Stop()
			case "observation_before_send":
				if err := conversation.Accept(frames[3], now); !errors.Is(err, ErrProtocol) {
					t.Fatal("observation accepted before dispatch")
				}
				return
			}
			if err := conversation.CanDispatch(frames[2].PhysicalCallID, now); !errors.Is(err, ErrProtocol) {
				t.Fatal("invalid dispatch accepted")
			}
		})
	}
}
