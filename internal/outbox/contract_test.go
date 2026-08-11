// Broker-neutral transport contract suite (FR-706 minimal implementation):
// envelope field completeness, event_id dedup key and error classification
// are verified against an in-memory fake transport, independent of any
// broker client.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/xjfyrh/jobforge/internal/store"
)

// fakeTransport is an in-memory Transport capturing published envelopes.
type fakeTransport struct {
	name      string
	durable   bool
	published []*Envelope
	failNext  error
}

func (f *fakeTransport) Name() string  { return f.name }
func (f *fakeTransport) Durable() bool { return f.durable }
func (f *fakeTransport) Publish(_ context.Context, env *Envelope) error {
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return err
	}
	f.published = append(f.published, env)
	return nil
}

var _ Transport = (*fakeTransport)(nil)

func sampleOutboxEvent() *store.OutboxEvent {
	ver := int64(7)
	tp := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	created := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	return &store.OutboxEvent{
		EventID:          42,
		AggregateID:      "job-uuid",
		EventType:        "job.succeeded",
		Payload:          []byte(`{"job_id":"job-uuid","event_type":"job.succeeded"}`),
		CreatedAt:        created,
		AggregateVersion: &ver,
		Traceparent:      &tp,
	}
}

// TestEnvelopeContract runs the envelope v1 contract table against events in
// both post-0016 and legacy (nil capture fields) shapes.
func TestEnvelopeContract(t *testing.T) {
	cases := []struct {
		name        string
		mutate      func(ev *store.OutboxEvent)
		wantVersion int64
		wantTP      string
	}{
		{name: "full capture", mutate: nil, wantVersion: 7, wantTP: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"},
		{name: "legacy nil fields", mutate: func(ev *store.OutboxEvent) {
			ev.AggregateVersion = nil
			ev.Traceparent = nil
		}, wantVersion: 0, wantTP: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := sampleOutboxEvent()
			if tc.mutate != nil {
				tc.mutate(ev)
			}
			env := NewEnvelope(ev)

			if env.SchemaVersion != EnvelopeSchemaVersion {
				t.Fatalf("schema_version = %d, want %d", env.SchemaVersion, EnvelopeSchemaVersion)
			}
			if env.EventID != "42" {
				t.Fatalf("event_id = %q, want decimal outbox id \"42\" (business dedup key, FR-704)", env.EventID)
			}
			if env.AggregateID != ev.AggregateID {
				t.Fatalf("aggregate_id = %q, want %q (partition key, FR-706)", env.AggregateID, ev.AggregateID)
			}
			if env.AggregateVersion != tc.wantVersion {
				t.Fatalf("aggregate_version = %d, want %d", env.AggregateVersion, tc.wantVersion)
			}
			if env.EventType != ev.EventType {
				t.Fatalf("event_type = %q, want %q", env.EventType, ev.EventType)
			}
			if !env.OccurredAt.Equal(ev.CreatedAt) {
				t.Fatalf("occurred_at = %v, want %v", env.OccurredAt, ev.CreatedAt)
			}
			if env.Traceparent != tc.wantTP {
				t.Fatalf("traceparent = %q, want %q", env.Traceparent, tc.wantTP)
			}
		})
	}
}

// TestEnvelopeJSONPayloadRaw verifies the payload stays raw JSON (not
// base64) and every v1 field is present on the wire.
func TestEnvelopeJSONPayloadRaw(t *testing.T) {
	env := NewEnvelope(sampleOutboxEvent())
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("envelope is not valid JSON: %v", err)
	}
	for _, field := range []string{
		"schema_version", "event_id", "aggregate_id", "aggregate_version",
		"event_type", "occurred_at", "payload", "traceparent",
	} {
		if _, ok := decoded[field]; !ok {
			t.Fatalf("envelope JSON missing field %q: %s", field, raw)
		}
	}
	payload, ok := decoded["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload is not an inline JSON object (base64?): %s", raw)
	}
	if payload["job_id"] != "job-uuid" {
		t.Fatalf("payload job_id = %v, want job-uuid", payload["job_id"])
	}
}

// TestStreamFieldRoundTrip verifies the Redis stream field encoding decodes
// back to an equivalent envelope.
func TestStreamFieldRoundTrip(t *testing.T) {
	env := NewEnvelope(sampleOutboxEvent())
	fields := map[string]any{
		"schema_version":    "1",
		"event_id":          env.EventID,
		"aggregate_id":      env.AggregateID,
		"aggregate_version": "7",
		"event_type":        env.EventType,
		"occurred_at":       env.OccurredAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		"payload":           string(env.Payload),
		"traceparent":       env.Traceparent,
	}
	got, err := DecodeEnvelopeFields(fields)
	if err != nil {
		t.Fatalf("decode fields: %v", err)
	}
	if got.EventID != env.EventID || got.AggregateID != env.AggregateID ||
		got.AggregateVersion != env.AggregateVersion || got.EventType != env.EventType ||
		got.Traceparent != env.Traceparent || string(got.Payload) != string(env.Payload) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, env)
	}
	if !got.OccurredAt.Equal(env.OccurredAt) {
		t.Fatalf("occurred_at round trip: got %v, want %v", got.OccurredAt, env.OccurredAt)
	}

	// Missing identity field is an error (poison entries must not decode
	// silently into valid envelopes).
	delete(fields, "event_id")
	if _, err := DecodeEnvelopeFields(fields); err == nil {
		t.Fatalf("expected error for missing event_id")
	}
}

// TestTransportContractErrorClassification verifies the contract: a failed
// Publish surfaces the broker error unchanged in classification (wrapped)
// and the fake never records a delivery for it.
func TestTransportContractErrorClassification(t *testing.T) {
	brokerErr := errors.New("connection refused")
	ft := &fakeTransport{name: TransportRedisStreams, durable: true, failNext: brokerErr}

	env := NewEnvelope(sampleOutboxEvent())
	err := ft.Publish(context.Background(), env)
	if err == nil || !errors.Is(err, brokerErr) {
		t.Fatalf("publish error = %v, want wrapped %v", err, brokerErr)
	}
	if len(ft.published) != 0 {
		t.Fatalf("failed publish must not record a delivery, got %d", len(ft.published))
	}

	if err := ft.Publish(context.Background(), env); err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if len(ft.published) != 1 || ft.published[0].EventID != "42" {
		t.Fatalf("delivery not recorded correctly: %+v", ft.published)
	}
}

// TestTransportIdentities pins the transport name constants used as metric
// labels (PRD v0.3 §8: bounded label set).
func TestTransportIdentities(t *testing.T) {
	ft := &fakeTransport{name: TransportNotify, durable: false}
	if ft.Name() != "notify" || ft.Durable() {
		t.Fatalf("notify identity wrong: %s durable=%v", ft.Name(), ft.Durable())
	}
	rt := &fakeTransport{name: TransportRedisStreams, durable: true}
	if rt.Name() != "redis_streams" || !rt.Durable() {
		t.Fatalf("redis_streams identity wrong: %s durable=%v", rt.Name(), rt.Durable())
	}
}
