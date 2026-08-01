package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/xjfyrh/jobforge/internal/worker"
)

// ReindexHandler simulates a PageWise document reindex operation.
// It demonstrates a real-world Agent/RAG task pattern:
//   - Sleep to simulate processing time
//   - Optional HTTP callback to notify completion
//   - Context cancellation support for graceful shutdown
//   - Business idempotency using job ID
//
// This handler satisfies FR-403: "PageWise 索引重建或 Agent 评测可异步提交、
// 观察、取消和恢复".
//
// Security note: The CallbackURL comes from authenticated job submissions.
// In production, validate URLs against an allowlist and block loopback,
// link-local, metadata, and private network ranges to prevent SSRF.
type ReindexHandler struct {
	client *http.Client
}

// NewReindexHandler creates a ReindexHandler with a default HTTP client.
func NewReindexHandler() *ReindexHandler {
	return &ReindexHandler{
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// reindexPayload defines the expected payload for pagewise.reindex.
type reindexPayload struct {
	// DocumentID is the document to reindex.
	DocumentID string `json:"document_id"`

	// SleepMs simulates processing time (default 1000ms).
	SleepMs int `json:"sleep_ms"`

	// CallbackURL is an optional HTTP endpoint to notify on completion.
	// If set, a POST request is sent with the result.
	CallbackURL string `json:"callback_url"`
}

// reindexResult is sent to the callback URL on completion.
type reindexResult struct {
	JobID      string `json:"job_id"`
	DocumentID string `json:"document_id"`
	Status     string `json:"status"`
	DurationMs int64  `json:"duration_ms"`
}

// Execute implements worker.Handler.
func (h *ReindexHandler) Execute(ctx context.Context, job *worker.ClaimedJob) (string, error) {
	var p reindexPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return "", fmt.Errorf("invalid reindex payload: %w", err)
	}

	if p.DocumentID == "" {
		return "", fmt.Errorf("document_id is required")
	}
	if p.SleepMs <= 0 {
		p.SleepMs = 1000
	}

	start := time.Now()

	// Simulate index rebuild processing.
	// Supports context cancellation for graceful shutdown.
	select {
	case <-time.After(time.Duration(p.SleepMs) * time.Millisecond):
		// Processing complete.
	case <-ctx.Done():
		return "", ctx.Err()
	}

	durationMs := time.Since(start).Milliseconds()

	// Optional callback notification.
	if p.CallbackURL != "" {
		result := reindexResult{
			JobID:      job.ID,
			DocumentID: p.DocumentID,
			Status:     "completed",
			DurationMs: durationMs,
		}

		body, err := json.Marshal(result)
		if err != nil {
			return "", fmt.Errorf("marshal callback: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.CallbackURL, bytes.NewReader(body))
		if err != nil {
			return "", fmt.Errorf("create callback request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := h.client.Do(req)
		if err != nil {
			// Callback failure is retryable (external dependency).
			return "", worker.NewRetryableError(fmt.Errorf("callback failed: %w", err))
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode >= 500 {
			return "", worker.NewRetryableError(fmt.Errorf("callback returned %d", resp.StatusCode))
		}
		if resp.StatusCode >= 400 {
			return "", fmt.Errorf("callback returned %d", resp.StatusCode)
		}
	}

	return fmt.Sprintf("reindex:%s:%dms", p.DocumentID, durationMs), nil
}

// RegisterPagewise registers PageWise demo handlers with the given registry.
func RegisterPagewise(reg *worker.Registry) {
	reg.Register("pagewise.reindex", NewReindexHandler())
}
