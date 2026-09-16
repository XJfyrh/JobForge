package runprotocol

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// BenchmarkExecutorV2Session measures the complete local codec/guard work per
// fixture session, including the extra confirmation frames of the new contract.
// It does not measure IPC, PostgreSQL, provider HTTP, or business throughput.
func BenchmarkExecutorV2Session(b *testing.B) {
	content, err := os.ReadFile("../../../api/executor/v2/fixtures/frames.json")
	if err != nil {
		b.Fatal(err)
	}
	var corpus fixtures
	if err := json.Unmarshal(content, &corpus); err != nil {
		b.Fatal(err)
	}
	for _, scenario := range []struct {
		name, fixture string
		calls         int
	}{
		{"chat_1_http", "metering_before_observation", 1},
		{"search_4_http", "search_policy_sequence_and_late_duplicate_metering", 4},
	} {
		b.Run(scenario.name, func(b *testing.B) {
			type operation struct {
				kind, id string
				wire     []byte
				now      int64
			}
			var operations []operation
			var wireBytes int64
			frames := 0
			for _, conversation := range corpus.Conversations {
				if conversation.Name != scenario.fixture {
					continue
				}
				for _, event := range conversation.Events {
					if !event.Accept {
						b.Fatal("benchmark requires a complete successful session")
					}
					op := operation{kind: event.Op, id: event.ID, now: event.Now}
					if event.Op == "ordinary" || event.Op == "metering" {
						var compact bytes.Buffer
						if err := json.Compact(&compact, event.Frame); err != nil {
							b.Fatal(err)
						}
						op.wire = append(compact.Bytes(), '\n')
						wireBytes += int64(len(op.wire))
						frames++
					}
					operations = append(operations, op)
				}
			}
			if len(operations) == 0 {
				b.Fatal("missing benchmark fixture")
			}
			b.ReportAllocs()
			b.SetBytes(wireBytes)
			b.ResetTimer()
			for b.Loop() {
				var conversation Conversation
				for _, op := range operations {
					var err error
					switch op.kind {
					case "ordinary":
						var frame Frame
						frame, err = DecodeOrdinary(op.wire)
						if err == nil {
							err = conversation.Accept(frame, op.now)
						}
					case "metering":
						var frame Frame
						frame, err = DecodeMetering(op.wire)
						if err == nil {
							err = conversation.AcceptMetering(frame, op.now)
						}
					case "dispatch":
						err = conversation.CanDispatch(op.id, op.now)
					case "stop":
						conversation.Stop()
					default:
						b.Fatal("unknown benchmark operation")
					}
					if err != nil {
						b.Fatal(err)
					}
				}
				if !conversation.Closed() {
					b.Fatal("benchmark session did not finish")
				}
			}
			b.ReportMetric(float64(len(operations)), "events/session")
			b.ReportMetric(float64(frames), "frames/session")
			b.ReportMetric(float64(scenario.calls), "http/session")
		})
	}
}
