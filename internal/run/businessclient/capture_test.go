package businessclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/xjfyrh/jobforge/internal/business"
	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

func metadataFixture(t *testing.T) snapshotMetadata {
	t.Helper()
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	profile := business.IndexProfile{PolicyVersion: "policy-v1", CorpusSHA256: strings.Repeat("c", 64),
		ChunkerVersion: business.ChunkerVersion, EmbeddingModel: business.EmbeddingModel,
		EmbeddingDigest: business.EmbeddingDigest, Dimensions: business.Dimensions}
	data, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	index := business.PublishedIndex{ID: uuid.NewString(), TenantID: "tenant-a", Profile: profile,
		ProfileHash: hex.EncodeToString(digest[:]), ContentHash: strings.Repeat("d", 64), PublishedAt: now}
	s := business.Snapshot{ID: uuid.NewString(), TenantID: "tenant-a", SchemaVersion: 1,
		AsOf: now, CreatedAt: now, ContentHash: strings.Repeat("e", 64), Index: index,
		Ticket: business.Ticket{TenantID: "tenant-a", TicketID: "ticket-1", Revision: 1, ObservedAt: now,
			PolicyVersion: "policy-v1", Subject: "Contract fixture", Description: "Missing order", Status: "open"},
		Policy: business.PolicyVersion{TenantID: "tenant-a", PolicyVersion: "policy-v1", Revision: 1, CorpusSHA256: profile.CorpusSHA256}}
	return snapshotMetadata{ID: s.ID, TenantID: s.TenantID, SchemaVersion: 1, AsOf: now, CreatedAt: now,
		ContentHash: s.ContentHash, Ticket: s.Ticket, Policy: s.Policy, Index: s.Index, VersionVector: s.VersionVector()}
}

func TestCaptureValidatesCompleteFrozenBindings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(*snapshotMetadata)
		valid bool
	}{
		{"explicit missing relationships", func(_ *snapshotMetadata) {}, true},
		{"foreign tenant", func(s *snapshotMetadata) { s.TenantID = "tenant-b" }, false},
		{"wrong ticket", func(s *snapshotMetadata) { s.Ticket.TicketID = "ticket-2" }, false},
		{"changed revision", func(s *snapshotMetadata) { s.VersionVector.Ticket.Revision++ }, false},
		{"missing fact has revision", func(s *snapshotMetadata) { r := int64(1); s.VersionVector.Order.Revision = &r }, false},
		{"inaccessible order cannot supply delivery", func(s *snapshotMetadata) { id := "delivery-1"; s.VersionVector.Delivery.ID = &id }, false},
		{"unregistered embedding", func(s *snapshotMetadata) { s.Index.Profile.EmbeddingModel = "other-model" }, false},
		{"tampered profile hash", func(s *snapshotMetadata) {
			s.Index.ProfileHash = strings.Repeat("a", 64)
			s.VersionVector.Index.ProfileHash = s.Index.ProfileHash
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := metadataFixture(t)
			tc.edit(&fixture)
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/business/v1/snapshots" ||
					r.Header.Get("Authorization") != "Bearer contract-operator-key" {
					t.Error("capture used an unexpected configured boundary")
				}
				var request business.SnapshotRequest
				if json.NewDecoder(r.Body).Decode(&request) != nil || request.TicketID != "ticket-1" || request.RequestKey != "capture-key" {
					t.Error("capture identity changed")
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(fixture)
			}))
			t.Cleanup(server.Close)
			client, err := New(server.URL, map[string]string{"tenant-a": "contract-operator-key"})
			if err != nil {
				t.Fatal(err)
			}
			binding, err := client.Capture(t.Context(), "tenant-a", "ticket-1", "capture-key")
			if tc.valid && (err != nil || binding.ID != fixture.ID || binding.IndexID != fixture.Index.ID) {
				t.Fatalf("valid snapshot rejected: %v", err)
			}
			if !tc.valid && err != agentrun.ErrDependencyUnavailable {
				t.Fatalf("malformed trusted response was accepted: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("capture performed %d physical requests", calls.Load())
			}
		})
	}
}

func TestCaptureRejectsIncompleteAndOversizedResponsesWithoutRetry(t *testing.T) {
	fixture := metadataFixture(t)
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"missing null relationship", strings.Replace(string(data), `"order_id":null,`, "", 1), http.StatusOK},
		{"duplicate key", strings.Replace(string(data), `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1), http.StatusOK},
		{"oversized", strings.Repeat(" ", business.MaxToolBytes+1), http.StatusOK},
		{"unavailable", `{"error":{"code":"TEMPORARY","message":"private detail"}}`, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			client, err := New(server.URL, map[string]string{"tenant-a": "contract-operator-key"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Capture(t.Context(), "tenant-a", "ticket-1", "capture-key"); err != agentrun.ErrDependencyUnavailable || calls.Load() != 1 {
				t.Fatalf("capture must fail once with a safe error: calls=%d error=%v", calls.Load(), err)
			}
		})
	}
}

func TestCaptureNeverForwardsCredentialOnRedirect(t *testing.T) {
	var destinationCalls atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { destinationCalls.Add(1) }))
	t.Cleanup(destination.Close)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)
	client, err := New(source.URL, map[string]string{"tenant-a": "contract-operator-key"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Capture(context.Background(), "tenant-a", "ticket-1", "capture-key"); err != agentrun.ErrDependencyUnavailable || destinationCalls.Load() != 0 {
		t.Fatal("redirect must not send a second request or credential")
	}
}
