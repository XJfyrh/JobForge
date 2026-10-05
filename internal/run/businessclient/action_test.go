package businessclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xjfyrh/jobforge/internal/business"
	"github.com/xjfyrh/jobforge/internal/run"
)

func TestReceiptQueryOnlyStrictBusinessNotFoundMeansAbsent(t *testing.T) {
	for _, check := range []struct {
		name, body string
		status     int
		timeout    bool
		absent     bool
	}{
		{"business not found", `{"error":{"code":"NOT_FOUND","message":"NOT_FOUND"}}`, 404, false, true},
		{"html", `<html>routing error</html>`, 404, false, false},
		{"missing message", `{"error":{"code":"NOT_FOUND"}}`, 404, false, false},
		{"unknown field", `{"error":{"code":"NOT_FOUND","message":"NOT_FOUND","extra":1}}`, 404, false, false},
		{"duplicate", `{"error":{"code":"NOT_FOUND","code":"NOT_FOUND","message":"NOT_FOUND"}}`, 404, false, false},
		{"invalid code", `{"error":{"code":"DEPENDENCY_UNAVAILABLE","message":"DEPENDENCY_UNAVAILABLE"}}`, 404, false, false},
		{"server failure", `{"error":{"code":"NOT_FOUND","message":"NOT_FOUND"}}`, 503, false, false},
		{"redirect", `{"error":{"code":"NOT_FOUND","message":"NOT_FOUND"}}`, 302, false, false},
		{"timeout", "", 200, true, false},
	} {
		t.Run(check.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				requests.Add(1)
				if check.timeout {
					<-req.Context().Done()
					return
				}
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(check.status)
				_, _ = w.Write([]byte(check.body))
			}))
			defer server.Close()
			client, err := NewActionClient(map[string]ActionCredentials{"tenant-north": {Origin: server.URL, ReaderKey: "synthetic-reader-key"}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			if check.timeout {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), 50*time.Millisecond)
			}
			defer cancel()
			result, err := client.request(ctx, http.MethodGet, server.URL+"/receipt", "synthetic-reader-key", nil, business.SignedAction{})
			if check.absent && err != nil || !check.absent && !errors.Is(err, run.ErrDependencyUnavailable) || result.Found || requests.Load() != 1 {
				t.Fatal("receipt absence/one exchange", err, result.Found, requests.Load())
			}
		})
	}
}
