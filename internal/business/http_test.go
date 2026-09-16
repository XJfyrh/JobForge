package business

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type boundedHTTPStore struct {
	HTTPStore
	order func(context.Context) (*OrderEvidence, error)
}

func (s boundedHTTPStore) GetOrder(ctx context.Context, _, _ string) (*OrderEvidence, error) {
	return s.order(ctx)
}

func TestHTTPBackpressureAndCancellation(t *testing.T) {
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	store := boundedHTTPStore{order: func(ctx context.Context) (*OrderEvidence, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return &OrderEvidence{Missing: true}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	handler, err := NewHTTPHandler(store, map[string]Identity{"bounded-test-reader": {TenantID: "tenant", Role: "reader"}})
	if err != nil {
		t.Fatal(err)
	}
	request := func(ctx context.Context) *http.Request {
		r := httptest.NewRequestWithContext(ctx, "GET", "/business/v1/snapshots/id/order", nil)
		r.Header.Set("Authorization", "Bearer bounded-test-reader")
		return r
	}
	var wait sync.WaitGroup
	for range 8 {
		wait.Go(func() {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, request(t.Context()))
			if w.Code != 200 {
				t.Errorf("admitted request status=%d", w.Code)
			}
		})
	}
	for range 8 {
		<-entered
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, request(t.Context()))
	if w.Code != 429 {
		t.Errorf("full service status=%d", w.Code)
	}
	close(release)
	wait.Wait()
	// A separate blocked store proves client cancellation reaches the operation.
	store.order = func(ctx context.Context) (*OrderEvidence, error) { <-ctx.Done(); return nil, ctx.Err() }
	handler, err = NewHTTPHandler(store, map[string]Identity{"bounded-test-reader": {TenantID: "tenant", Role: "reader"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, request(ctx))
	if w.Code != 503 {
		t.Fatalf("canceled operation status=%d", w.Code)
	}
}

func TestHTTPOutputAndAmbiguousJSONBounds(t *testing.T) {
	w := httptest.NewRecorder()
	writeBusinessJSON(w, http.StatusOK, strings.Repeat("secret synthetic output", MaxToolBytes))
	if w.Code != 500 || strings.Contains(w.Body.String(), "secret") {
		t.Fatal("oversized output leaked")
	}
	for _, raw := range []string{`{"ticket_id":"a","ticket_id":"b"}`, `{"schema_version":1} {}`, `{"unknown":1}`} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		if err := decodeBusinessJSON(httptest.NewRecorder(), r, 4096, new(SnapshotRequest)); err == nil {
			t.Fatalf("ambiguous input accepted: %s", raw)
		}
	}
}
