package business

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"
	"unicode/utf8"
)

type policyManifest struct {
	SchemaVersion   int      `json:"schema_version"`
	PolicyVersion   string   `json:"policy_version"`
	ChunkingVersion string   `json:"chunking_version"`
	CorpusSHA256    string   `json:"corpus_sha256"`
	Files           []string `json:"files"`
}

var paragraphMarker = regexp.MustCompile(`(?m)^<!-- paragraph_id: (P[0-9]{2}\.[12]) -->\r?$`)

// ValidateRegisteredIndex verifies loader content against a deployment-owned
// embedded registry. The caller must never construct this FS from upload data.
// It authenticates source paragraphs, not whether vectors came from a real model.
func ValidateRegisteredIndex(upload IndexUpload, registry fs.FS) error {
	if err := ValidateIndexUpload(upload); err != nil {
		return err
	}
	manifestBytes, err := readRegistryFile(registry, "policies/manifest.json", 4096)
	if err != nil {
		return err
	}
	var manifest policyManifest
	decoder := json.NewDecoder(bytes.NewReader(manifestBytes))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&manifest) != nil || decoder.Decode(new(any)) != io.EOF ||
		manifest.SchemaVersion != 1 || len(manifest.Files) != 10 || !validHash(manifest.CorpusSHA256) ||
		manifest.PolicyVersion != upload.Profile.PolicyVersion || manifest.ChunkingVersion != upload.Profile.ChunkerVersion ||
		manifest.CorpusSHA256 != upload.Profile.CorpusSHA256 {
		return ErrProfileUnavailable
	}
	hash := sha256.New()
	expected := make(map[string]IndexChunk, 20)
	total := 0
	for i, name := range manifest.Files {
		if name != fmt.Sprintf("P%02d.md", i+1) {
			return ErrProfileUnavailable
		}
		body, err := readRegistryFile(registry, "policies/"+name, 64*1024-total)
		if err != nil {
			return err
		}
		total += len(body)
		_, _ = hash.Write([]byte(name + "\n"))
		_, _ = hash.Write(body)
		_, _ = hash.Write([]byte("\n"))
		markers := paragraphMarker.FindAllSubmatchIndex(body, -1)
		if len(markers) != 2 {
			return ErrProfileUnavailable
		}
		for j, marker := range markers {
			id := string(body[marker[2]:marker[3]])
			if id != fmt.Sprintf("P%02d.%d", i+1, j+1) {
				return ErrProfileUnavailable
			}
			end := len(body)
			if j+1 < len(markers) {
				end = markers[j+1][0]
			}
			text := strings.TrimSpace(string(body[marker[1]:end]))
			if !validText(text, 768) || text == "" {
				return ErrProfileUnavailable
			}
			expected[id] = IndexChunk{ChunkID: id, Source: name, Text: text}
		}
	}
	if hex.EncodeToString(hash.Sum(nil)) != manifest.CorpusSHA256 {
		return ErrProfileUnavailable
	}
	if len(upload.Chunks) != len(expected) {
		return ErrConflict
	}
	for _, chunk := range upload.Chunks {
		registered, found := expected[chunk.ChunkID]
		if !found || chunk.Source != registered.Source || chunk.Text != registered.Text {
			return ErrConflict
		}
	}
	return nil
}

func readRegistryFile(registry fs.FS, path string, limit int) ([]byte, error) {
	if registry == nil || limit < 0 {
		return nil, ErrProfileUnavailable
	}
	file, err := registry.Open(path)
	if err != nil {
		return nil, ErrProfileUnavailable
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil || len(body) > limit || !utf8.Valid(body) {
		return nil, ErrProfileUnavailable
	}
	return body, nil
}
