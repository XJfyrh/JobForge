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
)

type fixtures struct {
	ValidFrames   []json.RawMessage                      `json:"valid_frames"`
	InvalidFrames []struct{ Name, Wire, Channel string } `json:"invalid_frames"`
	Conversations []struct {
		Name   string `json:"name"`
		Events []struct {
			Op     string          `json:"op"`
			Frame  json.RawMessage `json:"frame"`
			Now    int64           `json:"now_mono_ms"`
			ID     string          `json:"physical_call_id"`
			Accept bool            `json:"accept"`
		} `json:"events"`
	} `json:"conversations"`
}

func loadFixtures(t *testing.T) fixtures {
	t.Helper()
	content, err := os.ReadFile("../../../api/executor/v2/fixtures/frames.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixtures
	if err = json.Unmarshal(content, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func decodeFixture(t *testing.T, raw json.RawMessage) Frame {
	t.Helper()
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		t.Fatal(err)
	}
	f, err := Decode(append(compact.Bytes(), '\n'))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestSharedExecutorV2Fixtures(t *testing.T) {
	fixture := loadFixtures(t)
	for _, raw := range fixture.ValidFrames {
		f := decodeFixture(t, raw)
		t.Run("frame_"+f.Kind, func(t *testing.T) {
			wire, err := Encode(f)
			if err != nil {
				t.Fatal(err)
			}
			roundtrip, err := Decode(wire)
			if err != nil || !reflect.DeepEqual(f, roundtrip) {
				t.Fatalf("roundtrip: %v", err)
			}
		})
	}
	for _, test := range fixture.InvalidFrames {
		t.Run(test.Name, func(t *testing.T) {
			decode := DecodeOrdinary
			if test.Channel == "metering" {
				decode = DecodeMetering
			}
			if _, err := decode([]byte(test.Wire)); !errors.Is(err, ErrProtocol) {
				t.Fatalf("accepted invalid frame: %v", err)
			}
		})
	}
	for _, test := range fixture.Conversations {
		t.Run(test.Name, func(t *testing.T) {
			var c Conversation
			for index, event := range test.Events {
				var err error
				switch event.Op {
				case "ordinary":
					err = c.Accept(decodeFixture(t, event.Frame), event.Now)
				case "metering":
					err = c.AcceptMetering(decodeFixture(t, event.Frame), event.Now)
				case "dispatch":
					err = c.CanDispatch(event.ID, event.Now)
				case "stop":
					c.Stop()
				default:
					t.Fatal("unknown fixture event")
				}
				if (err == nil) != event.Accept {
					t.Fatalf("event %d (%s) accepted=%t want=%t: %v", index, event.Op, err == nil, event.Accept, err)
				}
				if err != nil && !c.Closed() {
					t.Fatal("error retained execution authority")
				}
			}
		})
	}
}

func TestSchemaMatchesV2FieldSets(t *testing.T) {
	content, err := os.ReadFile("../../../api/executor/v2/schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		MaxFrame int `json:"x-max-frame-bytes"`
		MaxMeter int `json:"x-max-metering-frame-bytes"`
		Defs     map[string]struct {
			Required   []string `json:"required"`
			Additional bool     `json:"additionalProperties"`
		} `json:"$defs"`
	}
	if err = json.Unmarshal(content, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.MaxFrame != MaxFrameBytes || schema.MaxMeter != MaxMeteringFrameBytes {
		t.Fatal("schema bounds drifted")
	}
	sets := map[string][]string{"binding": bindingFields, "usage": usageFields}
	for kind, fields := range kindFields {
		sets[kind] = append(append([]string{}, commonFields...), fields...)
	}
	for kind, fields := range sets {
		d, ok := schema.Defs[kind]
		if !ok || d.Additional {
			t.Fatalf("%s must be closed", kind)
		}
		want := append([]string{}, fields...)
		sort.Strings(want)
		sort.Strings(d.Required)
		if !reflect.DeepEqual(want, d.Required) {
			t.Fatalf("%s fields drifted", kind)
		}
	}
}

func TestDedicatedPipeBoundedReadsAndNoCrossPipeFrames(t *testing.T) {
	f := loadFixtures(t)
	ordinary := decodeFixture(t, f.ValidFrames[0])
	meter := decodeFixture(t, f.ValidFrames[5])
	ow, err := EncodeOrdinary(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	mw, err := EncodeMetering(meter)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		limit int
		wire  []byte
		read  func(io.Reader) (Frame, error)
	}{
		{"ordinary", MaxFrameBytes, ow, ReadFrame}, {"metering", MaxMeteringFrameBytes, mw, ReadMeteringFrame},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, size := range []int{test.limit, test.limit + 1} {
				wire := append(bytes.Repeat([]byte(" "), size-len(test.wire)), test.wire...)
				_, err := test.read(bytes.NewReader(wire))
				if (err == nil) != (size == test.limit) {
					t.Fatalf("size %d: %v", size, err)
				}
			}
			r := bytes.NewReader(append(append([]byte{}, test.wire...), test.wire...))
			for range 2 {
				if _, err := test.read(r); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := test.read(r); !errors.Is(err, io.EOF) {
				t.Fatal("expected EOF")
			}
			if _, err := test.read(bytes.NewReader(test.wire[:len(test.wire)-1])); !errors.Is(err, ErrProtocol) {
				t.Fatal("accepted partial frame")
			}
		})
	}
	if _, err := DecodeOrdinary(mw); !errors.Is(err, ErrProtocol) {
		t.Fatal("metering on ordinary pipe")
	}
	if _, err := DecodeMetering(ow); !errors.Is(err, ErrProtocol) {
		t.Fatal("ordinary on metering pipe")
	}
	if _, err := EncodeOrdinary(meter); !errors.Is(err, ErrProtocol) {
		t.Fatal("metering output on ordinary pipe")
	}
	if _, err := EncodeMetering(ordinary); !errors.Is(err, ErrProtocol) {
		t.Fatal("ordinary output on metering pipe")
	}
}

func TestV2ProtectedBoundsAndInvalidUnicode(t *testing.T) {
	f := loadFixtures(t)
	start := decodeFixture(t, f.ValidFrames[0])
	for _, size := range []int{MaxCheckpointBytes, MaxCheckpointBytes + 1} {
		start.Checkpoint = json.RawMessage(`{"value":"` + strings.Repeat("x", size-12) + `"}`)
		_, err := Encode(start)
		if (err == nil) != (size == MaxCheckpointBytes) {
			t.Fatalf("checkpoint %d: %v", size, err)
		}
	}
	start.Checkpoint = json.RawMessage(`{}`)
	wire, err := Encode(start)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Decode(append([]byte{0xff}, wire...)); !errors.Is(err, ErrProtocol) {
		t.Fatal("accepted invalid UTF-8")
	}
	start.Checkpoint = json.RawMessage(`{"value":` + strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65) + "}")
	if _, err = Encode(start); !errors.Is(err, ErrProtocol) {
		t.Fatal("accepted excessive nesting")
	}
}

func TestAnchoredDeadlineSafeIntegerAndQueueDelay(t *testing.T) {
	for _, test := range []struct {
		emitted, remaining, now, want int64
		valid                         bool
	}{
		{1000, 100, 1050, 1100, true}, {1000, 100, 1100, 0, false}, {1000, 100, 999, 0, false},
		{MaxInteger - 1, 2, MaxInteger - 1, 0, false}, {0, 1, 0, 1, true}, {-1, 1, 0, 0, false},
		{0, MaxInteger, MaxInteger - 1, MaxInteger, true}, {0, 0, 0, 0, false},
	} {
		got, err := AnchoredDeadline(test.emitted, test.remaining, test.now)
		if (err == nil) != test.valid || got != test.want {
			t.Fatalf("%+v: %d %v", test, got, err)
		}
	}
}

func TestObservationHashIsSnapshotAcrossMeteringJoin(t *testing.T) {
	for _, matching := range []bool{false, true} {
		name := "mismatch_cannot_be_repaired_by_caller"
		if matching {
			name = "match_cannot_be_corrupted_by_caller"
		}
		t.Run(name, func(t *testing.T) {
			frames := map[string]Frame{}
			for _, raw := range loadFixtures(t).ValidFrames {
				frame := decodeFixture(t, raw)
				frame.EmittedMonoMS = 1000
				if _, exists := frames[frame.Kind]; !exists {
					frames[frame.Kind] = frame
				}
			}
			var c Conversation
			for _, kind := range []string{"execute_step", "call_intent", "call_permit"} {
				if err := c.Accept(frames[kind], 1000); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.CanDispatch(frames["call_permit"].PhysicalCallID, 1000); err != nil {
				t.Fatal(err)
			}
			observation := frames["call_observation"]
			correctHash := frames["metering_report"].Usage.UsageHash
			if !matching {
				*observation.UsageHash = strings.Repeat("f", 64)
			}
			if err := c.Accept(observation, 1000); err != nil {
				t.Fatal(err)
			}
			// Change the original pointer after acceptance, before the other FD.
			*observation.UsageHash = correctHash
			if matching {
				*observation.UsageHash = strings.Repeat("f", 64)
			}
			for _, kind := range []string{"metering_report", "metering_ack"} {
				if err := c.AcceptMetering(frames[kind], 1000); err != nil {
					t.Fatal(err)
				}
			}
			if c.Closed() == matching {
				t.Fatal("caller mutation changed accepted observation identity")
			}
			if err := c.Accept(frames["step_result"], 1000); (err == nil) != matching {
				t.Fatalf("original matching=%t result error=%v", matching, err)
			}
		})
	}
}
