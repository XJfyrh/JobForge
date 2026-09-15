package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/xjfyrh/jobforge/internal/worker"
)

// These tests use explicit model substitutes. Real model acceptance is in
// tests/integration/real_tasks_test.go and is never inferred from these tests.
type substituteModel struct {
	calls     int
	responses []string
	err       error
}

func (m *substituteModel) Embed(context.Context, []string) ([][]float64, error) {
	return nil, errors.New("unexpected embedding call")
}
func (m *substituteModel) Extract(_ context.Context, _, _ string) (string, error) {
	m.calls++
	if m.err != nil {
		return "", m.err
	}
	return m.responses[min(m.calls-1, len(m.responses)-1)], nil
}

const validOrderJSON = `{"order_id":"PO-2026-0042","supplier":"Northwind Components","quantity":12,"currency":"USD","total_amount":300,"delivery_date":"2026-10-01","source_quote":"Purchase order PO-2026-0042."}`

func TestExtractionRepairBudgetAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		responses []string
		err       error
		calls     int
		ok        bool
	}{
		{"first response", []string{validOrderJSON}, nil, 1, true},
		{"repair", []string{`{"order_id":"wrong"}`, validOrderJSON}, nil, 2, true},
		{"repair exhausted", []string{`{}`}, nil, 2, false},
		{"no implicit transport retry", nil, transient("MODEL_UNAVAILABLE"), 1, false},
		{"invalid JSON", []string{`not JSON`}, nil, 2, false},
		{"trailing JSON", []string{validOrderJSON + `{}`}, nil, 2, false},
		{"untrusted source quote", []string{strings.Replace(validOrderJSON, "Purchase order PO-2026-0042.", "secret fabricated instruction", 1)}, nil, 2, false},
		{"wrong numeric type", []string{strings.Replace(validOrderJSON, `"quantity":12`, `"quantity":"12"`, 1)}, nil, 2, false},
		{"oversize", []string{strings.Repeat(" ", maxOutputBytes) + validOrderJSON}, nil, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &substituteModel{responses: tc.responses, err: tc.err}
			_, err := extractOrder(t.Context(), model)
			if (err == nil) != tc.ok || model.calls != tc.calls {
				t.Fatalf("err=%v calls=%d", err, model.calls)
			}
		})
	}
}

func TestTaskInputRejectsDynamicCapabilities(t *testing.T) {
	for _, extra := range []string{`"tenant_id":"victim"`, `"url":"file:///secret"`, `"command":"echo unsafe"`, `"tools":[]`, `"model":"unregistered"`, `"version":2`} {
		payload := `{"version":1,"business_key":"test","corpus_version":"handbook-v1",` + extra + `}`
		if _, err := parseInput("rag.index", []byte(payload)); err == nil {
			t.Fatalf("accepted %s", extra)
		}
	}
	for _, payload := range []string{`{}`, `null`, `{"version":1,"business_key":"../unsafe","corpus_version":"handbook-v1"}`, `{"version":1,"business_key":"x","corpus_version":"handbook-v2"}`} {
		if _, err := parseInput("rag.index", []byte(payload)); err == nil {
			t.Fatalf("accepted %s", payload)
		}
	}
}

func TestChunkingAndCosineValidation(t *testing.T) {
	chunks := chunkMarkdown("# Heading\n\n" + strings.Repeat("中", 900) + "\n\nlast paragraph")
	if len(chunks) != 4 || len([]rune(chunks[0])) != 480 || !utf8.ValidString(chunks[1]) || chunks[3] != "last paragraph" {
		t.Fatalf("invalid chunks: count=%d", len(chunks))
	}
	a, b := make([]float64, 384), make([]float64, 384)
	a[0], b[1] = 1, 1
	idx := Index{Dimensions: 384, ModelDigest: EmbedDigest, Chunks: []Chunk{{Source: "first", Vector: a}, {Source: "second", Vector: b}}}
	hits, err := idx.Search(b, 1)
	if err != nil || hits[0].Source != "second" || hits[0].Score != 1 {
		t.Fatalf("hits=%v err=%v", hits, err)
	}
	b[0] = math.NaN()
	if _, err = idx.Search(b, 1); err == nil {
		t.Fatal("accepted NaN")
	}
}

func TestOllamaContractBoundsAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		retryable bool
		expected  string
	}{
		{"overload", 429, `private model response`, true, "MODEL_UNAVAILABLE"},
		{"server", 500, `private model response`, true, "MODEL_UNAVAILABLE"},
		{"credentials", 401, `private model response`, false, "MODEL_REQUEST_REJECTED"},
		{"malformed", 200, `{}`, false, "MODEL_RESPONSE_INVALID"},
		{"oversize", 200, strings.Repeat("x", MaxArtifactBytes+1), false, "MODEL_RESPONSE_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/tags" {
					_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{{"name": EmbedModel, "digest": EmbedDigest}}})
					return
				}
				mu.Lock()
				calls++
				mu.Unlock()
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			model, _ := NewOllama(server.URL, "")
			_, err := model.Embed(t.Context(), []string{"document"})
			mu.Lock()
			count := calls
			mu.Unlock()
			if err == nil || err.Error() != tc.expected || worker.IsRetryable(err) != tc.retryable || count != 1 {
				t.Fatalf("err=%v calls=%d", err, count)
			}
		})
	}
	t.Run("cancel active HTTP request", func(t *testing.T) {
		started, cancelled := make(chan struct{}), make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			close(started)
			<-r.Context().Done()
			close(cancelled)
		}))
		defer server.Close()
		model, _ := NewOllama(server.URL, "")
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { _, err := model.Embed(ctx, []string{"document"}); done <- err }()
		<-started
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
		select {
		case <-cancelled:
		case <-time.After(time.Second):
			t.Fatal("HTTP cancellation did not reach backend")
		}
	})
	t.Run("digest mismatch blocks inference", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"models":[]}`)) }))
		defer server.Close()
		model, _ := NewOllama(server.URL, "")
		if _, err := model.Extract(t.Context(), "document", ""); err == nil || err.Error() != "MODEL_VERSION_MISMATCH" {
			t.Fatalf("err=%v", err)
		}
	})
}
