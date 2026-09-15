// Package tasks contains pre-registered Agent/RAG business adapters. It owns
// model calls and business artifacts, but never reads or writes queue state.
package tasks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"

	"github.com/xjfyrh/jobforge/internal/worker"
)

// Fixed acceptance models, immutable manifest digests, and resource budgets.
const (
	EmbedModel       = "all-minilm:22m"
	EmbedDigest      = "1b226e2802dbb772b5fc32a58f103ca1804ef7501331012de126ab22f67475ef"
	ChatModel        = "qwen2.5:0.5b"
	ChatDigest       = "a8b0c51577010a279d933d14c2a8ab4b268079d44c5c8830c0a93900f1827c67"
	MaxArtifactBytes = 2 * 1024 * 1024
	maxDocumentBytes = 64 * 1024
	maxOutputBytes   = 16 * 1024
)

var (
	// ErrNotFound also hides artifacts owned by other tenants.
	ErrNotFound        = errors.New("artifact not found")
	businessKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// Error carries a bounded, public machine code without backend response data.
type Error struct{ Code string }

func (e *Error) Error() string { return e.Code }

// ErrorCode lets the Runtime classify failures without importing business code.
func (e *Error) ErrorCode() string { return e.Code }

func permanent(code string) error { return &Error{Code: code} }
func transient(code string) error { return worker.NewRetryableError(permanent(code)) }

// Input is the complete, versioned task input. Sources are pre-registered;
// arbitrary files, URLs, tools, tenant IDs, and model options are not accepted.
type Input struct {
	Version         int    `json:"version"`
	BusinessKey     string `json:"business_key"`
	CorpusVersion   string `json:"corpus_version,omitempty"`
	DocumentVersion string `json:"document_version,omitempty"`
	SchemaVersion   string `json:"schema_version,omitempty"`
}

func parseInput(taskType string, data []byte) (Input, error) {
	var p Input
	if len(data) > maxDocumentBytes || strictJSON(data, &p) != nil || p.Version != 1 || !businessKeyPattern.MatchString(p.BusinessKey) {
		return p, permanent("BUSINESS_INPUT_INVALID")
	}
	switch taskType {
	case "rag.index":
		if p.CorpusVersion != "handbook-v1" || p.DocumentVersion != "" || p.SchemaVersion != "" {
			return p, permanent("BUSINESS_INPUT_INVALID")
		}
	case "agent.extract":
		if p.DocumentVersion != "purchase-order-v1" || p.SchemaVersion != "purchase-order-v1" || p.CorpusVersion != "" {
			return p, permanent("BUSINESS_INPUT_INVALID")
		}
	default:
		return p, permanent("BUSINESS_INPUT_INVALID")
	}
	return p, nil
}

func strictJSON(data []byte, value any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return err
	}
	if dec.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func digest(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// Model performs bounded inference. Tests explicitly provide a substitute;
// production always wires Ollama. Neither implementation owns a task lease.
type Model interface {
	Embed(context.Context, []string) ([][]float64, error)
	Extract(context.Context, string, string) (string, error)
}

// ArtifactStore atomically publishes one immutable business effect per key.
type ArtifactStore interface {
	Lookup(context.Context, string, string, string) (*Artifact, error)
	Publish(context.Context, *Artifact) (*Artifact, bool, error)
	Get(context.Context, string, string) (*Artifact, error)
}
