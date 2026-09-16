package business

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"testing/fstest"
)

func registeredFixture() (IndexUpload, fstest.MapFS) {
	registry := make(fstest.MapFS)
	manifest := policyManifest{SchemaVersion: 1, PolicyVersion: "policy-v1", ChunkingVersion: ChunkerVersion}
	upload := IndexUpload{SchemaVersion: 1, TenantID: "tenant-a", Profile: IndexProfile{
		PolicyVersion: "policy-v1", ChunkerVersion: ChunkerVersion, EmbeddingModel: EmbeddingModel, EmbeddingDigest: EmbeddingDigest, Dimensions: Dimensions,
	}}
	hash := sha256.New()
	for i := 1; i <= 10; i++ {
		name := fmt.Sprintf("P%02d.md", i)
		body := fmt.Sprintf("# Policy %d\n\n<!-- paragraph_id: P%02d.1 -->\nFirst rule.\n\n<!-- paragraph_id: P%02d.2 -->\nSecond rule.\n", i, i, i)
		registry["policies/"+name] = &fstest.MapFile{Data: []byte(body)}
		manifest.Files = append(manifest.Files, name)
		_, _ = hash.Write([]byte(name + "\n" + body + "\n"))
		for j, text := range []string{"First rule.", "Second rule."} {
			vector := make([]float64, Dimensions)
			vector[0] = 1
			upload.Chunks = append(upload.Chunks, IndexChunk{ChunkID: fmt.Sprintf("P%02d.%d", i, j+1), Source: name, Text: text, Embedding: vector})
		}
	}
	manifest.CorpusSHA256 = hex.EncodeToString(hash.Sum(nil))
	upload.Profile.CorpusSHA256 = manifest.CorpusSHA256
	manifestBytes, _ := json.Marshal(manifest)
	registry["policies/manifest.json"] = &fstest.MapFile{Data: manifestBytes}
	return upload, registry
}

func TestRegisteredIndexRejectsClaimedHashWithDifferentContent(t *testing.T) {
	for _, scenario := range []string{"valid", "text", "source", "missing", "duplicate", "raw_file", "profile", "manifest_path"} {
		t.Run(scenario, func(t *testing.T) {
			upload, registry := registeredFixture()
			switch scenario {
			case "text":
				upload.Chunks[0].Text = "Unregistered policy supplied by the uploader."
			case "source":
				upload.Chunks[0].Source = "P99.md"
			case "missing":
				upload.Chunks = upload.Chunks[1:]
			case "duplicate":
				upload.Chunks[1] = upload.Chunks[0]
			case "raw_file":
				registry["policies/P01.md"].Data = append(registry["policies/P01.md"].Data, '\n')
			case "profile":
				upload.Profile.EmbeddingDigest = string(make([]byte, 64))
			case "manifest_path":
				var manifest policyManifest
				_ = json.Unmarshal(registry["policies/manifest.json"].Data, &manifest)
				manifest.Files[0] = "../outside.md"
				registry["policies/manifest.json"].Data, _ = json.Marshal(manifest)
			}
			err := ValidateRegisteredIndex(upload, registry)
			if scenario == "valid" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrConflict) && !errors.Is(err, ErrProfileUnavailable) && !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("invalid registered content accepted: %v", err)
			}
		})
	}
}
