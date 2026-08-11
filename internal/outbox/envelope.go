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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/xjfyrh/jobforge/internal/store"
)

var traceparentPattern = regexp.MustCompile(
	`^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`,
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

// DecodeEnvelopeFields validates and decodes the stream-field encoding written
// by RedisStreamsTransport. Aggregate version zero is accepted for legacy
// outbox rows, and an empty or absent traceparent is valid.
func DecodeEnvelopeFields(fields map[string]any) (*Envelope, error) {
	str := func(key string) (string, bool) {
		v, ok := fields[key]
		if !ok {
			return "", false
		}
		s, ok := v.(string)
		return s, ok
	}

	required := func(key string) (string, error) {
		value, ok := str(key)
		if !ok || strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("missing %s field", key)
		}
		return value, nil
	}

	schemaVersion, err := required("schema_version")
	if err != nil {
		return nil, err
	}
	parsedSchemaVersion, err := strconv.Atoi(schemaVersion)
	if err != nil || parsedSchemaVersion != EnvelopeSchemaVersion {
		return nil, fmt.Errorf("unsupported schema_version")
	}
	eventID, err := required("event_id")
	if err != nil {
		return nil, err
	}
	parsedEventID, err := strconv.ParseInt(eventID, 10, 64)
	if err != nil || parsedEventID < 1 || strconv.FormatInt(parsedEventID, 10) != eventID {
		return nil, fmt.Errorf("invalid event_id field")
	}
	aggregateID, err := required("aggregate_id")
	if err != nil {
		return nil, err
	}
	eventType, err := required("event_type")
	if err != nil {
		return nil, err
	}

	aggregateVersion, err := required("aggregate_version")
	if err != nil {
		return nil, err
	}
	parsedAggregateVersion, err := strconv.ParseInt(aggregateVersion, 10, 64)
	if err != nil || parsedAggregateVersion < 0 {
		return nil, fmt.Errorf("invalid aggregate_version field")
	}
	occurredAt, err := required("occurred_at")
	if err != nil {
		return nil, err
	}
	parsedOccurredAt, err := time.Parse(time.RFC3339Nano, occurredAt)
	if err != nil {
		return nil, fmt.Errorf("invalid occurred_at field")
	}
	payload, err := required("payload")
	if err != nil {
		return nil, err
	}
	if !json.Valid([]byte(payload)) {
		return nil, fmt.Errorf("invalid payload JSON")
	}

	traceparent, _ := str("traceparent")
	if traceparent != "" && !validTraceparent(traceparent) {
		return nil, fmt.Errorf("invalid traceparent field")
	}
	return &Envelope{
		SchemaVersion:    parsedSchemaVersion,
		EventID:          eventID,
		AggregateID:      aggregateID,
		AggregateVersion: parsedAggregateVersion,
		EventType:        eventType,
		OccurredAt:       parsedOccurredAt,
		Payload:          []byte(payload),
		Traceparent:      traceparent,
	}, nil
}

func validTraceparent(value string) bool {
	if !traceparentPattern.MatchString(value) {
		return false
	}
	parts := strings.Split(value, "-")
	return parts[1] != strings.Repeat("0", 32) && parts[2] != strings.Repeat("0", 16)
}
