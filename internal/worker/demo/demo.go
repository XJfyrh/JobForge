// Package demo provides demonstration Handlers for testing and integration
// demos. These handlers exercise various JobForge features: echo (connectivity),
// sleep (timeout/cancel), fail (retry/DLQ), and idempotent_effect (dedup).
package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/xjfyrh/jobforge/internal/worker"
)

// EchoHandler returns the payload as-is. Used for minimal connectivity testing.
type EchoHandler struct{}

// Execute implements worker.Handler.
func (h *EchoHandler) Execute(_ context.Context, job *worker.ClaimedJob) (string, error) {
	return fmt.Sprintf("echo:%s", string(job.Payload)), nil
}

// SleepHandler sleeps for a configurable duration. Supports context cancellation
// for testing cancel propagation and timeout behavior.
type SleepHandler struct{}

// sleepPayload defines the expected payload for demo.sleep.
type sleepPayload struct {
	DurationMs int `json:"duration_ms"`
}

// Execute implements worker.Handler.
func (h *SleepHandler) Execute(ctx context.Context, job *worker.ClaimedJob) (string, error) {
	var p sleepPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return "", fmt.Errorf("invalid sleep payload: %w", err)
	}
	if p.DurationMs <= 0 {
		p.DurationMs = 1000
	}

	select {
	case <-time.After(time.Duration(p.DurationMs) * time.Millisecond):
		return fmt.Sprintf("slept:%dms", p.DurationMs), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// FailHandler always fails. Configurable as retryable or fatal for testing
// retry backoff and DLQ behavior.
type FailHandler struct{}

// failPayload defines the expected payload for demo.fail.
type failPayload struct {
	Retryable bool   `json:"retryable"`
	Message   string `json:"message"`
}

// Execute implements worker.Handler.
func (h *FailHandler) Execute(_ context.Context, job *worker.ClaimedJob) (string, error) {
	var p failPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		p.Retryable = true
		p.Message = "default failure"
	}
	if p.Message == "" {
		p.Message = "demo.fail triggered"
	}

	err := fmt.Errorf("%s", p.Message)
	if p.Retryable {
		return "", worker.NewRetryableError(err)
	}
	return "", err
}

// IdempotentEffectHandler simulates a side-effect that must only happen once.
// It uses an in-memory set to track processed job IDs, demonstrating business
// idempotency. In production, this would be a database table.
// Safe for concurrent use (implements Handler contract).
type IdempotentEffectHandler struct {
	mu sync.Mutex
	// processed tracks job IDs that have already produced their side effect.
	// In production this would be a persistent store (e.g. PostgreSQL table).
	processed map[string]bool
}

// NewIdempotentEffectHandler creates a handler with an empty processed set.
func NewIdempotentEffectHandler() *IdempotentEffectHandler {
	return &IdempotentEffectHandler{processed: make(map[string]bool)}
}

// Execute implements worker.Handler. The "side effect" is recorded only once
// per job ID, even if the handler is called multiple times (at-least-once).
func (h *IdempotentEffectHandler) Execute(_ context.Context, job *worker.ClaimedJob) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.processed[job.ID] {
		// Already processed; idempotent return.
		return "deduplicated", nil
	}

	// Simulate side effect (e.g. write to external system).
	h.processed[job.ID] = true
	return fmt.Sprintf("effect:%s", job.ID), nil
}

// RegisterAll registers all demo handlers with the given registry.
func RegisterAll(reg *worker.Registry) {
	reg.Register("demo.echo", &EchoHandler{})
	reg.Register("demo.sleep", &SleepHandler{})
	reg.Register("demo.fail", &FailHandler{})
	reg.Register("demo.idempotent_effect", NewIdempotentEffectHandler())
}
