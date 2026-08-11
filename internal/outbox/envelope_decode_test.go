package outbox

import (
	"testing"
	"time"
)

func validEnvelopeFields() map[string]any {
	return map[string]any{
		"schema_version":    "1",
		"event_id":          "42",
		"aggregate_id":      "job-42",
		"aggregate_version": "0",
		"event_type":        "job.succeeded",
		"occurred_at":       time.Now().UTC().Format(time.RFC3339Nano),
		"payload":           `{"ok":true}`,
	}
}

func TestDecodeEnvelopeFieldsStrictV1(t *testing.T) {
	fields := validEnvelopeFields()
	envelope, err := DecodeEnvelopeFields(fields)
	if err != nil {
		t.Fatalf("decode valid legacy envelope: %v", err)
	}
	if envelope.AggregateVersion != 0 || envelope.Traceparent != "" {
		t.Fatalf("legacy zero values not preserved: %+v", envelope)
	}

	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"schema_missing", func(f map[string]any) { delete(f, "schema_version") }},
		{"schema_unsupported", func(f map[string]any) { f["schema_version"] = "2" }},
		{"event_id_missing", func(f map[string]any) { delete(f, "event_id") }},
		{"event_id_invalid", func(f map[string]any) { f["event_id"] = "event-42" }},
		{"event_id_plus_prefix", func(f map[string]any) { f["event_id"] = "+42" }},
		{"event_id_leading_zero", func(f map[string]any) { f["event_id"] = "042" }},
		{"aggregate_id_empty", func(f map[string]any) { f["aggregate_id"] = " " }},
		{"aggregate_version_missing", func(f map[string]any) { delete(f, "aggregate_version") }},
		{"aggregate_version_negative", func(f map[string]any) { f["aggregate_version"] = "-1" }},
		{"event_type_missing", func(f map[string]any) { delete(f, "event_type") }},
		{"occurred_at_invalid", func(f map[string]any) { f["occurred_at"] = "yesterday" }},
		{"payload_invalid", func(f map[string]any) { f["payload"] = "{" }},
		{"payload_missing", func(f map[string]any) { delete(f, "payload") }},
		{"traceparent_invalid", func(f map[string]any) { f["traceparent"] = "trace-123" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := validEnvelopeFields()
			test.mutate(candidate)
			if _, err := DecodeEnvelopeFields(candidate); err == nil {
				t.Fatal("expected strict decoder rejection")
			}
		})
	}
}
