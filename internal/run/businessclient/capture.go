// Package businessclient implements the configured snapshot acquisition
// boundary. Requests cannot choose an endpoint, credential, or executable.
package businessclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/xjfyrh/jobforge/internal/business"
	"github.com/xjfyrh/jobforge/internal/jsonstrict"
	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

// Client has a fixed deployment URL and operator credential for each tenant.
type Client struct {
	baseURL string
	keys    map[string]string
	http    *http.Client
}

// New deliberately disables redirects and automatic application retries.
func New(baseURL string, tenantKeys map[string]string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || len(tenantKeys) == 0 {
		return nil, agentrun.ErrInvalidArgument
	}
	c := &Client{baseURL: strings.TrimSuffix(baseURL, "/"), keys: make(map[string]string, len(tenantKeys)),
		http: &http.Client{Timeout: 10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}
	for tenant, key := range tenantKeys {
		if !agentrun.ValidIdentifier(tenant) || len(key) < 16 || len(key) > 256 || strings.ContainsAny(key, " \t\r\n") {
			return nil, agentrun.ErrInvalidArgument
		}
		c.keys[tenant] = key
	}
	return c, nil
}

type snapshotMetadata struct {
	ID            string                  `json:"snapshot_id"`
	TenantID      string                  `json:"tenant_id"`
	SchemaVersion int                     `json:"schema_version"`
	AsOf          time.Time               `json:"as_of"`
	CreatedAt     time.Time               `json:"created_at"`
	ContentHash   string                  `json:"content_hash"`
	Ticket        business.Ticket         `json:"ticket"`
	Policy        business.PolicyVersion  `json:"policy"`
	Index         business.PublishedIndex `json:"index"`
	VersionVector business.VersionVector  `json:"version_vector"`
}

// Capture sends exactly one bounded POST. Its stable acquisition key allows a
// later explicit request to resolve an uncertain capture without a hidden retry.
func (c *Client) Capture(ctx context.Context, tenant, ticketID, key string) (agentrun.SnapshotBinding, error) {
	var empty agentrun.SnapshotBinding
	credential, ok := c.keys[tenant]
	if !ok {
		return empty, agentrun.ErrNotFound
	}
	if !agentrun.ValidIdentifier(ticketID) || !agentrun.ValidIdentifier(key) {
		return empty, agentrun.ErrInvalidArgument
	}
	body, err := json.Marshal(business.SnapshotRequest{SchemaVersion: 1, TicketID: ticketID, RequestKey: key})
	if err != nil {
		return empty, agentrun.ErrInternal
	}
	ctx, span := otel.Tracer("jobforge/run").Start(ctx, "run.capture_snapshot")
	defer span.End()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/business/v1/snapshots", bytes.NewReader(body))
	if err != nil {
		return empty, agentrun.ErrInternal
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+credential)
	propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(req.Header))
	response, err := c.http.Do(req)
	if err != nil {
		return empty, agentrun.ErrDependencyUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, business.MaxToolBytes+1))
	if err != nil || len(data) > business.MaxToolBytes {
		return empty, agentrun.ErrDependencyUnavailable
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return empty, captureError(response.StatusCode)
	}
	if media := strings.Split(response.Header.Get("Content-Type"), ";")[0]; media != "application/json" {
		return empty, agentrun.ErrDependencyUnavailable
	}
	var snapshot snapshotMetadata
	if jsonstrict.Decode(data, &snapshot) != nil || !completeShape(data, snapshot) ||
		!validSnapshot(snapshot, tenant, ticketID) {
		return empty, agentrun.ErrDependencyUnavailable
	}
	vector, err := json.Marshal(snapshot.VersionVector)
	if err != nil {
		return empty, agentrun.ErrInternal
	}
	ticket, err := json.Marshal(snapshot.Ticket)
	if err != nil {
		return empty, agentrun.ErrInternal
	}
	return agentrun.SnapshotBinding{TenantID: tenant, TicketID: ticketID, ID: snapshot.ID, ContentHash: snapshot.ContentHash,
		VersionVector: vector, Ticket: ticket, IndexID: snapshot.Index.ID, IndexProfileHash: snapshot.Index.ProfileHash}, nil
}

func captureError(status int) error {
	switch status {
	case http.StatusNotFound:
		return agentrun.ErrNotFound
	case http.StatusConflict:
		return agentrun.ErrConflict
	default:
		// Authentication/configuration errors of this trusted dependency must not
		// be misreported as invalid credentials supplied to the public Run API.
		return agentrun.ErrDependencyUnavailable
	}
}

func hash(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && value == strings.ToLower(value)
}

func canonicalUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id.String() == value
}

func revision(value int64) bool { return value > 0 && value <= agentrun.MaxSafeInteger }

func validSnapshot(s snapshotMetadata, tenant, ticketID string) bool {
	v, p, index := s.VersionVector, s.Policy, s.Index
	if s.SchemaVersion != 1 || v.SchemaVersion != 1 || !canonicalUUID(s.ID) || !hash(s.ContentHash) ||
		s.TenantID != tenant || s.Ticket.TenantID != tenant || s.Ticket.TicketID != ticketID ||
		!revision(s.Ticket.Revision) || s.AsOf.IsZero() || s.CreatedAt.IsZero() || s.Ticket.ObservedAt.IsZero() ||
		v.Ticket.ID != ticketID || v.Ticket.Revision != s.Ticket.Revision ||
		p.TenantID != tenant || p.PolicyVersion != s.Ticket.PolicyVersion || !agentrun.ValidIdentifier(p.PolicyVersion) ||
		!revision(p.Revision) || !hash(p.CorpusSHA256) || v.Policy.Version != p.PolicyVersion ||
		v.Policy.Revision != p.Revision || v.Policy.CorpusSHA256 != p.CorpusSHA256 ||
		index.TenantID != tenant || !canonicalUUID(index.ID) || !hash(index.ContentHash) || !hash(index.ProfileHash) ||
		index.PublishedAt.IsZero() || v.Index.ID != index.ID || v.Index.ProfileHash != index.ProfileHash || v.Index.ContentHash != index.ContentHash {
		return false
	}
	profile := index.Profile
	if profile.PolicyVersion != p.PolicyVersion || profile.CorpusSHA256 != p.CorpusSHA256 ||
		profile.ChunkerVersion != business.ChunkerVersion || profile.EmbeddingModel != business.EmbeddingModel ||
		profile.EmbeddingDigest != business.EmbeddingDigest || profile.Dimensions != business.Dimensions {
		return false
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		return false
	}
	digest := sha256.Sum256(encoded)
	if hex.EncodeToString(digest[:]) != index.ProfileHash || !equalOptionalID(v.Order.ID, s.Ticket.OrderID) ||
		!validOptionalFact(v.Order.ID, v.Order.Exists, v.Order.Revision) ||
		!validOptionalFact(v.Delivery.ID, v.Delivery.Exists, v.Delivery.AggregateRevision) {
		return false
	}
	return v.Order.Exists || v.Delivery.ID == nil
}

func equalOptionalID(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func validOptionalFact(id *string, exists bool, version *int64) bool {
	if id != nil && !agentrun.ValidIdentifier(*id) {
		return false
	}
	if exists {
		return id != nil && version != nil && revision(*version)
	}
	return version == nil
}

// completeShape distinguishes required explicit null/missing facts from an
// incomplete response silently decoded into Go zero values.
func completeShape(data []byte, value any) bool {
	expected, err := json.Marshal(value)
	return err == nil && sameFields(data, expected)
}

func sameFields(actual, expected []byte) bool {
	var a, e map[string]json.RawMessage
	if json.Unmarshal(actual, &a) != nil || json.Unmarshal(expected, &e) != nil || len(a) != len(e) {
		return false
	}
	for key, value := range e {
		present, ok := a[key]
		if !ok {
			return false
		}
		if len(value) > 0 && value[0] == '{' && !sameFields(present, value) {
			return false
		}
	}
	return true
}
