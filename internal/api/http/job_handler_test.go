package http

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakePinger is a test double for the Pinger interface.
type fakePinger struct {
	err error
}

func (f *fakePinger) Ping(_ context.Context) error {
	return f.err
}

func TestHealthReady_Success(t *testing.T) {
	pinger := &fakePinger{err: nil}
	handler := NewJobHandler(nil, pinger, slog.Default())

	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	rec := httptest.NewRecorder()

	handler.HealthReady(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "ready" {
		t.Fatalf("expected status field \"ready\", got %q", body["status"])
	}
}

func TestHealthReady_PingFails_Returns503(t *testing.T) {
	pinger := &fakePinger{err: errors.New("connection refused")}
	handler := NewJobHandler(nil, pinger, slog.Default())

	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	rec := httptest.NewRecorder()

	handler.HealthReady(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status 503, got %d", rec.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "unavailable" {
		t.Fatalf("expected status field \"unavailable\", got %q", body["status"])
	}
}

func TestHealthReady_ResponseStructureCompatible(t *testing.T) {
	pinger := &fakePinger{err: nil}
	handler := NewJobHandler(nil, pinger, slog.Default())

	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	rec := httptest.NewRecorder()

	handler.HealthReady(rec, req)

	// Verify Content-Type header is preserved.
	ct := rec.Header().Get("Content-Type")
	if ct != "application/json; charset=utf-8" {
		t.Fatalf("expected Content-Type application/json; charset=utf-8, got %q", ct)
	}

	// Verify the response is a flat JSON object with only "status" key.
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(rec.Body).Decode(&raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if len(raw) != 1 {
		t.Fatalf("expected exactly 1 field in response, got %d", len(raw))
	}
	if _, ok := raw["status"]; !ok {
		t.Fatal("response missing \"status\" field")
	}
}

func TestHealthLive_Unchanged(t *testing.T) {
	handler := NewJobHandler(nil, &fakePinger{}, slog.Default())

	req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	rec := httptest.NewRecorder()

	handler.HealthLive(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "alive" {
		t.Fatalf("expected status field \"alive\", got %q", body["status"])
	}
}
