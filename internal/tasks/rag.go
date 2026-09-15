package tasks

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf8"
)

//go:embed fixtures/handbook-v1/*.md fixtures/purchase-order-v1.txt fixtures/purchase-order-v1.schema.json
var fixtures embed.FS

const indexVersion = "paragraph-runes480-overlap80-cosine-v1"

// Chunk records the parsed source span and the actual model embedding.
type Chunk struct {
	ID     string    `json:"id"`
	Source string    `json:"source"`
	Text   string    `json:"text"`
	Vector []float64 `json:"vector"`
}

// RetrievalCheck is evaluated against the actual persisted vector matrix.
type RetrievalCheck struct {
	Query          string  `json:"query"`
	ExpectedSource string  `json:"expected_source"`
	ActualSource   string  `json:"actual_source"`
	Score          float64 `json:"score"`
}

// Index is a small, portable business artifact. Its contents never enter jobs.
type Index struct {
	CorpusVersion string           `json:"corpus_version"`
	CorpusSHA256  string           `json:"corpus_sha256"`
	IndexVersion  string           `json:"index_version"`
	Model         string           `json:"model"`
	ModelDigest   string           `json:"model_digest"`
	Dimensions    int              `json:"dimensions"`
	Chunks        []Chunk          `json:"chunks"`
	Checks        []RetrievalCheck `json:"checks"`
}

func loadCorpus() ([]Chunk, string, error) {
	entries, err := fixtures.ReadDir("fixtures/handbook-v1")
	if err != nil {
		return nil, "", permanent("CORPUS_INVALID")
	}
	var chunks []Chunk
	var canonical strings.Builder
	for _, entry := range entries {
		data, readErr := fixtures.ReadFile("fixtures/handbook-v1/" + entry.Name())
		if readErr != nil || !utf8.Valid(data) {
			return nil, "", permanent("CORPUS_INVALID")
		}
		canonical.WriteString(entry.Name() + "\n" + string(data) + "\n")
		if canonical.Len() > maxDocumentBytes {
			return nil, "", permanent("CORPUS_INVALID")
		}
		for _, text := range chunkMarkdown(string(data)) {
			chunks = append(chunks, Chunk{ID: fmt.Sprintf("%s:%d", entry.Name(), len(chunks)), Source: entry.Name(), Text: text})
		}
	}
	if len(chunks) == 0 || len(chunks) > 64 {
		return nil, "", permanent("CORPUS_INVALID")
	}
	return chunks, digest([]byte(canonical.String())), nil
}

// chunkMarkdown reads paragraphs, removes headings, and splits long paragraphs
// at 480 Unicode runes with 80-rune overlap, without splitting UTF-8 bytes.
func chunkMarkdown(document string) []string {
	var chunks []string
	for _, paragraph := range strings.Split(strings.ReplaceAll(document, "\r\n", "\n"), "\n\n") {
		paragraph = strings.TrimSpace(paragraph)
		if paragraph == "" || strings.HasPrefix(paragraph, "#") {
			continue
		}
		runes := []rune(paragraph)
		for start := 0; start < len(runes); start += 400 {
			end := min(start+480, len(runes))
			chunks = append(chunks, string(runes[start:end]))
			if end == len(runes) {
				break
			}
		}
	}
	return chunks
}

func buildIndex(ctx context.Context, model Model) (*Index, error) {
	chunks, sourceHash, err := loadCorpus()
	if err != nil {
		return nil, err
	}
	texts := make([]string, len(chunks))
	for i := range chunks {
		texts[i] = chunks[i].Text
	}
	vectors, err := model.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	if len(vectors) != len(chunks) {
		return nil, permanent("MODEL_RESPONSE_INVALID")
	}
	for i := range chunks {
		if !validVector(vectors[i], 384) {
			return nil, permanent("MODEL_RESPONSE_INVALID")
		}
		chunks[i].Vector = vectors[i]
	}
	idx := &Index{CorpusVersion: "handbook-v1", CorpusSHA256: sourceHash, IndexVersion: indexVersion,
		Model: EmbedModel, ModelDigest: EmbedDigest, Dimensions: 384, Chunks: chunks,
		Checks: []RetrievalCheck{
			{Query: "How is work recovered after a Worker process crashes?", ExpectedSource: "recovery.md"},
			{Query: "How are other tenants prevented from reading my task results?", ExpectedSource: "isolation.md"},
			{Query: "How are documents ranked in a vector index?", ExpectedSource: "retrieval.md"},
		},
	}
	queries := make([]string, len(idx.Checks))
	for i := range idx.Checks {
		queries[i] = idx.Checks[i].Query
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	queryVectors, err := model.Embed(ctx, queries)
	if err != nil {
		return nil, err
	}
	if len(queryVectors) != len(queries) {
		return nil, permanent("MODEL_RESPONSE_INVALID")
	}
	for i := range idx.Checks {
		hits, searchErr := idx.Search(queryVectors[i], 1)
		if searchErr != nil {
			return nil, searchErr
		}
		idx.Checks[i].ActualSource = hits[0].Source
		idx.Checks[i].Score = hits[0].Score
		if hits[0].Source != idx.Checks[i].ExpectedSource {
			return nil, permanent("RETRIEVAL_VALIDATION_FAILED")
		}
	}
	return idx, nil
}

// Hit is one cosine-ranked document chunk.
type Hit struct {
	ChunkID string  `json:"chunk_id"`
	Source  string  `json:"source"`
	Text    string  `json:"text"`
	Score   float64 `json:"score"`
}

// Search inspects real index vectors. A corrupt or incompatible index fails.
func (idx *Index) Search(query []float64, k int) ([]Hit, error) {
	if k < 1 || k > 10 || idx.Dimensions != 384 || idx.ModelDigest != EmbedDigest || !validVector(query, idx.Dimensions) || len(idx.Chunks) == 0 || len(idx.Chunks) > 64 {
		return nil, permanent("INDEX_INVALID")
	}
	hits := make([]Hit, 0, len(idx.Chunks))
	for _, chunk := range idx.Chunks {
		if !validVector(chunk.Vector, idx.Dimensions) {
			return nil, permanent("INDEX_INVALID")
		}
		var dot, a, b float64
		for i, x := range query {
			dot += x * chunk.Vector[i]
			a += x * x
			b += chunk.Vector[i] * chunk.Vector[i]
		}
		hits = append(hits, Hit{ChunkID: chunk.ID, Source: chunk.Source, Text: chunk.Text, Score: dot / math.Sqrt(a*b)})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	return hits[:min(k, len(hits))], nil
}

// SearchArtifact uses the persisted index with a new real query embedding.
func SearchArtifact(ctx context.Context, model Model, a *Artifact, query string, k int) ([]Hit, error) {
	if a.TaskType != "rag.index" || len(query) == 0 || len(query) > 2048 || !utf8.ValidString(query) || k < 1 || k > 10 {
		return nil, permanent("BUSINESS_INPUT_INVALID")
	}
	var idx Index
	if err := json.Unmarshal(a.Body, &idx); err != nil {
		return nil, permanent("INDEX_INVALID")
	}
	vectors, err := model.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vectors) != 1 {
		return nil, permanent("MODEL_RESPONSE_INVALID")
	}
	return idx.Search(vectors[0], k)
}
