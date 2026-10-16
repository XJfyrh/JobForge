package business_test

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/xjfyrh/jobforge/internal/business"
)

func TestCompletePolicyBundleFrozenTenantBinding(t *testing.T) {
	for _, variant := range []string{"complete", "missing", "extra", "wrong_alias"} {
		t.Run(variant, func(t *testing.T) {
			ctx, _, loaderPool, readerPool := isolatedPools(t)
			loader, reader := business.NewStore(loaderPool), business.NewStore(readerPool)
			dataset, upload := fixture()
			upload.Chunks = nil
			for _, alias := range business.PolicyBundleAliases() {
				vector := make([]float64, business.Dimensions)
				vector[0] = 1
				upload.Chunks = append(upload.Chunks, business.IndexChunk{ChunkID: alias, Source: alias[:3] + ".md", Text: strings.Repeat("x", 500), Embedding: vector})
			}
			switch variant {
			case "missing":
				upload.Chunks = upload.Chunks[:19]
			case "extra":
				extra := upload.Chunks[0]
				extra.ChunkID = "P11.1"
				upload.Chunks = append(upload.Chunks, extra)
			case "wrong_alias":
				upload.Chunks[0].ChunkID = "P11.1"
			}
			// Database order, not upload order, defines the response.
			slices.Reverse(upload.Chunks)
			if err := loader.ImportDataset(ctx, dataset); err != nil {
				t.Fatal(err)
			}
			if _, _, err := loader.PublishIndex(ctx, upload); err != nil {
				t.Fatal(err)
			}
			snapshot, _, err := reader.CreateSnapshot(ctx, "tenant-north", business.SnapshotRequest{SchemaVersion: 1, TicketID: "ticket-1", RequestKey: "bundle"})
			if err != nil {
				t.Fatal(err)
			}
			hits, err := reader.ReadPolicyBundle(ctx, "tenant-north", snapshot.ID)
			if variant != "complete" {
				if !errors.Is(err, business.ErrProfileUnavailable) {
					t.Fatal("incomplete corpus", err)
				}
				return
			}
			if err != nil || len(hits) != 20 {
				t.Fatal("bundle", err)
			}
			for i, alias := range business.PolicyBundleAliases() {
				if hits[i].ChunkID != alias || hits[i].IndexID != snapshot.Index.ID || hits[i].PolicyVersion != "policy-v1" || hits[i].Distance != nil {
					t.Fatal("source binding")
				}
			}
			if _, err := reader.ReadPolicyBundle(ctx, "tenant-south", snapshot.ID); !errors.Is(err, business.ErrNotFound) {
				t.Fatal("cross tenant", err)
			}
			handler, err := business.NewHTTPHandler(reader, map[string]business.Identity{"bundle-test-reader-key": {TenantID: "tenant-north", Role: "reader"}})
			if err != nil {
				t.Fatal(err)
			}
			for _, query := range []string{"", "?index_id=other", "?aliases=P01.1"} {
				request := httptest.NewRequest("GET", "/business/v1/snapshots/"+snapshot.ID+"/policies/bundle"+query, nil)
				request.Header.Set("Authorization", "Bearer bundle-test-reader-key")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if query != "" {
					if response.Code != 400 {
						t.Fatal("query accepted", response.Code)
					}
					continue
				}
				if response.Code != 200 || response.Body.Len() <= 8192 || response.Body.Len() > business.PolicyBundleMaxBytes {
					t.Fatal("bundle response limit", response.Code, response.Body.Len())
				}
				var content struct {
					Matches []map[string]any `json:"matches"`
				}
				if json.Unmarshal(response.Body.Bytes(), &content) != nil || len(content.Matches) != 20 {
					t.Fatal("response shape")
				}
				for _, hit := range content.Matches {
					if len(hit) != 6 {
						t.Fatal("synthetic distance or missing source")
					}
				}
			}
		})
	}
}
