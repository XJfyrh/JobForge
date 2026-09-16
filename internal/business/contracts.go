// Package business owns the isolated, tenant-scoped order and policy facts.
// It never reads or writes the execution control database.
package business

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// SchemaVersion identifies the bounded business wire schema.
	SchemaVersion = 1
	// Dimensions is fixed by the accepted embedding profile.
	Dimensions = 384
	// EmbeddingModel names the fixed resource preparation model.
	EmbeddingModel = "all-minilm:22m"
	// EmbeddingDigest pins the resource preparation model manifest.
	EmbeddingDigest = "1b226e2802dbb772b5fc32a58f103ca1804ef7501331012de126ab22f67475ef"
	// ChunkerVersion identifies the registered paragraph splitter.
	ChunkerVersion = "paragraph-v1"
	// MaxToolBytes is the public response limit; oversized facts fail closed.
	MaxToolBytes     = 8192
	maxSnapshotBytes = 6144
)

// Stable errors deliberately omit database messages and business contents.
var (
	ErrInvalidArgument       = errors.New("invalid business argument")
	ErrNotFound              = errors.New("business resource not found")
	ErrConflict              = errors.New("business content conflict")
	ErrProfileUnavailable    = errors.New("business profile unavailable")
	ErrDependencyUnavailable = errors.New("business dependency unavailable")
	ErrInternal              = errors.New("business operation failed")
	ErrWrongDatabase         = errors.New("database is not an isolated business database")
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// Ticket is one versioned synthetic business request.
type Ticket struct {
	TenantID      string    `json:"tenant_id"`
	TicketID      string    `json:"ticket_id"`
	Revision      int64     `json:"revision"`
	ObservedAt    time.Time `json:"observed_at"`
	OrderID       *string   `json:"order_id"`
	PolicyVersion string    `json:"policy_version"`
	Subject       string    `json:"subject"`
	Description   string    `json:"description"`
	Status        string    `json:"status"`
}

// Order carries the authorized order and its optional delivery association.
type Order struct {
	TenantID           string    `json:"tenant_id"`
	OrderID            string    `json:"order_id"`
	Revision           int64     `json:"revision"`
	DeliveryID         *string   `json:"delivery_id"`
	Status             string    `json:"status"`
	OrderedAt          time.Time `json:"ordered_at"`
	PromisedDeliveryAt time.Time `json:"promised_delivery_at"`
}

// DeliveryEvent is immutable evidence within a versioned delivery aggregate.
type DeliveryEvent struct {
	EventID    string    `json:"event_id"`
	OccurredAt time.Time `json:"occurred_at"`
	Status     string    `json:"status"`
	Note       string    `json:"note"`
}

// Delivery versions the whole event collection, including newly added events.
type Delivery struct {
	TenantID          string          `json:"tenant_id"`
	DeliveryID        string          `json:"delivery_id"`
	OrderID           string          `json:"order_id"`
	AggregateRevision int64           `json:"aggregate_revision"`
	Status            string          `json:"status"`
	Events            []DeliveryEvent `json:"events"`
}

// PolicyVersion binds an immutable named policy to its registered corpus.
type PolicyVersion struct {
	TenantID      string `json:"tenant_id"`
	PolicyVersion string `json:"policy_version"`
	Revision      int64  `json:"revision"`
	CorpusSHA256  string `json:"corpus_sha256"`
}

// Dataset contains runtime facts only; evaluation gold belongs elsewhere.
type Dataset struct {
	SchemaVersion  int             `json:"schema_version"`
	DatasetVersion string          `json:"dataset_version"`
	Policies       []PolicyVersion `json:"policies"`
	Tickets        []Ticket        `json:"tickets"`
	Orders         []Order         `json:"orders"`
	Deliveries     []Delivery      `json:"deliveries"`
}

// IndexProfile fixes every input that changes vector interpretation.
type IndexProfile struct {
	PolicyVersion   string `json:"policy_version"`
	CorpusSHA256    string `json:"corpus_sha256"`
	ChunkerVersion  string `json:"chunker_version"`
	EmbeddingModel  string `json:"embedding_model"`
	EmbeddingDigest string `json:"embedding_digest"`
	Dimensions      int    `json:"dimensions"`
}

// IndexChunk contains a fixed corpus paragraph and a previously computed vector.
type IndexChunk struct {
	ChunkID   string    `json:"chunk_id"`
	Source    string    `json:"source"`
	Text      string    `json:"text"`
	Embedding []float64 `json:"embedding"`
}

// IndexUpload is accepted only by the offline loader identity.
type IndexUpload struct {
	SchemaVersion int          `json:"schema_version"`
	PrepareID     string       `json:"prepare_id,omitempty"`
	TenantID      string       `json:"tenant_id"`
	Profile       IndexProfile `json:"profile"`
	Chunks        []IndexChunk `json:"chunks"`
}

// PublishedIndex is a complete, immutable index visible to new snapshots.
type PublishedIndex struct {
	ID          string       `json:"index_id"`
	TenantID    string       `json:"tenant_id"`
	ProfileHash string       `json:"profile_hash"`
	ContentHash string       `json:"content_hash"`
	Profile     IndexProfile `json:"profile"`
	PublishedAt time.Time    `json:"published_at"`
}

// SnapshotRequest binds an idempotency key to exactly one ticket and schema.
type SnapshotRequest struct {
	SchemaVersion int    `json:"schema_version"`
	TicketID      string `json:"ticket_id"`
	RequestKey    string `json:"request_key"`
}

// Snapshot is a protected immutable copy, not a reference to current source rows.
type Snapshot struct {
	ID            string         `json:"snapshot_id"`
	TenantID      string         `json:"tenant_id"`
	SchemaVersion int            `json:"schema_version"`
	AsOf          time.Time      `json:"as_of"`
	CreatedAt     time.Time      `json:"created_at"`
	ContentHash   string         `json:"content_hash"`
	Ticket        Ticket         `json:"ticket"`
	Order         *Order         `json:"order"`
	Delivery      *Delivery      `json:"delivery"`
	Policy        PolicyVersion  `json:"policy"`
	Index         PublishedIndex `json:"index"`
}

// Evidence is a typed, snapshot-scoped response including explicit missing facts.
type Evidence struct {
	SnapshotID    string    `json:"snapshot_id"`
	EvidenceRef   string    `json:"evidence_ref"`
	Kind          string    `json:"kind"`
	Missing       bool      `json:"missing"`
	MissingReason string    `json:"missing_reason,omitempty"`
	Ticket        *Ticket   `json:"ticket,omitempty"`
	Order         *Order    `json:"order,omitempty"`
	Delivery      *Delivery `json:"delivery,omitempty"`
}

// OrderEvidence retains the immutable evidence envelope for an order.
type OrderEvidence = Evidence

// DeliveryEvidence retains the immutable evidence envelope for a delivery.
type DeliveryEvidence = Evidence

// SearchRequest contains only the fixed profile identity and a bounded vector.
type SearchRequest struct {
	EmbeddingModel  string    `json:"embedding_model"`
	EmbeddingDigest string    `json:"embedding_digest"`
	QueryVector     []float64 `json:"query_vector"`
}

// PolicyHit is a snapshot-authorized, versioned paragraph reference.
type PolicyHit struct {
	IndexID       string   `json:"index_id"`
	ChunkID       string   `json:"chunk_id"`
	PolicyVersion string   `json:"policy_version"`
	EvidenceRef   string   `json:"evidence_ref"`
	Source        string   `json:"source"`
	Text          string   `json:"text"`
	Distance      *float64 `json:"distance,omitempty"`
}

func fingerprint(value any) string {
	data, _ := json.Marshal(value)
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func validID(value string) bool { return identifierPattern.MatchString(value) }
func validText(value string, limit int) bool {
	return utf8.ValidString(value) && len(value) <= limit && !strings.ContainsRune(value, 0)
}
func validHash(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && strings.ToLower(value) == value
}

func validProfile(p IndexProfile) bool {
	return validID(p.PolicyVersion) && validHash(p.CorpusSHA256) && p.ChunkerVersion == ChunkerVersion &&
		p.EmbeddingModel == EmbeddingModel && p.EmbeddingDigest == EmbeddingDigest && p.Dimensions == Dimensions
}

// vectorLiteral accepts only finite, float32-representable nonzero vectors.
// The result is still a bound SQL parameter, never SQL text interpolation.
func vectorLiteral(vector []float64) (string, error) {
	if len(vector) != Dimensions {
		return "", ErrInvalidArgument
	}
	var b strings.Builder
	b.WriteByte('[')
	var norm float32
	for i, value := range vector {
		if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > math.MaxFloat32 {
			return "", ErrInvalidArgument
		}
		f := float32(value)
		// Explicit rounding also catches vectors whose every component's
		// square underflows, even when a float64 sum would remain nonzero.
		product := float32(f * f)
		norm = float32(norm + product)
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	// pgvector's cosine accumulation uses float32 arithmetic. Finite
	// components alone do not prevent a zero/infinite squared norm and a
	// misleading distance; fixed model embeddings are well inside this range.
	if norm < math.SmallestNonzeroFloat32 || norm > math.MaxFloat32/2 {
		return "", ErrInvalidArgument
	}
	b.WriteByte(']')
	return b.String(), nil
}
