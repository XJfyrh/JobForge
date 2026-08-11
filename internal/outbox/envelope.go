// Event envelope v1 (PRD v0.3 FR-703, ADR-0006 §4, PRD appendix B).
//
// The envelope is the public event contract of the durable transport: it is
// built by the publisher from the outbox row alone (all required fields are
// captured at event write time by migration 0016), so no broker or jobs
// lookup is needed at publish time. Future changes may only add fields in a
// backward-compatible way; changing schema_version requires a new ADR.

package outbox

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/xjfyrh/jobforge/internal/store"
)

// EnvelopeSchemaVersion is the fixed schema version of envelope v1.
const EnvelopeSchemaVersion = 1

// Envelope is the versioned event payload delivered to the external
// transport. Consumers deduplicate by EventID (the business dedup key,
// FR-704) and detect duplicates/reordering per aggregate via
// AggregateVersion; neither global ordering nor exactly-once is implied.
type Envelope struct {
	SchemaVersion int `json:"schema_version"`

	// EventID is the outbox event id in decimal string form. It is the
	// business deduplication key; the Redis stream entry id must never be
	// used for dedup (FR-704).
	EventID string `json:"event_id"`

	// AggregateID is the job id the event belongs to. It doubles as the
	// partition key for broker-neutral adapters (FR-706).
	AggregateID string `json:"aggregate_id"`

	// AggregateVersion is jobs.state_version captured at event write time.
	// 0 means unknown (legacy rows written before migration 0016).
	AggregateVersion int64 `json:"aggregate_version"`

	EventType   string    `json:"event_type"`
	OccurredAt  time.Time `json:"occurred_at"`
	Payload     []byte    `json:"payload"`
	Traceparent string    `json:"traceparent"`
}

// NewEnvelope builds envelope v1 from an outbox row. Legacy rows (pre-0016)
// carry nil AggregateVersion/Traceparent and are rendered as 0/"".
func NewEnvelope(ev *store.OutboxEvent) *Envelope {
	env := &Envelope{
		SchemaVersion: EnvelopeSchemaVersion,
		EventID:       strconv.FormatInt(ev.EventID, 10),
		AggregateID:   ev.AggregateID,
		EventType:     ev.EventType,
		OccurredAt:    ev.CreatedAt,
		Payload:       ev.Payload,
	}
	if ev.AggregateVersion != nil {
		env.AggregateVersion = *ev.AggregateVersion
	}
	if ev.Traceparent != nil {
		env.Traceparent = *ev.Traceparent
	}
	return env
}

// MarshalJSON keeps the payload as raw JSON instead of base64-encoding it,
// so the envelope stays human-inspectable on the wire (broker tooling, AOF
// dumps). Payload is always valid JSON written by the outbox writers.
func (e *Envelope) MarshalJSON() ([]byte, error) {
	payload := e.Payload
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	type alias Envelope
	return json.Marshal(struct {
		*alias
		Payload json.RawMessage `json:"payload"`
	}{
		alias:   (*alias)(e),
		Payload: json.RawMessage(payload),
	})
}

// envelopeFromStreamFields decodes the stream-field encoding written by
// RedisStreamsTransport.Publish. Used by the consumer-group reader; missing
// optional fields decode as zero values, missing identity fields are errors.
func envelopeFromStreamFields(fields map[string]any) (*Envelope, error) {
	str := func(key string) (string, bool) {
		v, ok := fields[key]
		if !ok {
			return "", false
		}
		s, ok := v.(string)
		return s, ok
	}

	eventID, ok := str("event_id")
	if !ok || eventID == "" {
		return nil, fmt.Errorf("missing event_id field")
	}
	aggregateID, _ := str("aggregate_id")
	eventType, _ := str("event_type")
	payload, _ := str("payload")
	traceparent, _ := str("traceparent")

	env := &Envelope{
		EventID:     eventID,
		AggregateID: aggregateID,
		EventType:   eventType,
		Payload:     []byte(payload),
		Traceparent: traceparent,
	}
	if sv, ok := str("schema_version"); ok {
		if n, err := strconv.Atoi(sv); err == nil {
			env.SchemaVersion = n
		}
	}
	if av, ok := str("aggregate_version"); ok {
		if n, err := strconv.ParseInt(av, 10, 64); err == nil {
			env.AggregateVersion = n
		}
	}
	if oa, ok := str("occurred_at"); ok {
		if t, err := time.Parse("2006-01-02T15:04:05.999999999Z07:00", oa); err == nil {
			env.OccurredAt = t
		}
	}
	return env, nil
}
