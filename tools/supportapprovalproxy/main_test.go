package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xjfyrh/jobforge/internal/business"
)

func TestActionProxyOneUpstreamExchangeAndNoRedirect(t *testing.T) {
	for _, mode := range []string{"closed", "redirect", "business_absent"} {
		t.Run(mode, func(t *testing.T) {
			var sent, redirected atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { redirected.Add(1); w.WriteHeader(200) }))
			defer target.Close()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				sent.Add(1)
				switch mode {
				case "closed":
					connection, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = connection.Close()
				case "redirect":
					w.Header().Set("Location", target.URL)
					w.WriteHeader(302)
					_, _ = w.Write([]byte(`{"redirect":true}`))
				default:
					w.WriteHeader(404)
					_, _ = w.Write([]byte(`{"error":{"code":"NOT_FOUND","message":"NOT_FOUND"}}`))
				}
			}))
			defer upstream.Close()
			e := &experiment{directory: t.TempDir(), used: map[string]bool{}}
			proxy := httptest.NewServer(e.httpForward(upstream.URL))
			defer proxy.Close()
			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			response, err := client.Get(proxy.URL + "/business/v1/actions/00000000-0000-4000-8000-000000000001/receipt")
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if sent.Load() != 1 || redirected.Load() != 0 {
				t.Fatal("proxy retried or redirected a physical exchange", sent.Load(), redirected.Load())
			}
			files, _ := filepath.Glob(filepath.Join(e.directory, "*.dispatch.json"))
			if len(files) != 1 {
				t.Fatal("physical send trace is incomplete")
			}
		})
	}
}

func TestActionProxyBarrierRequiresMatchingActualReceiptAndCancels(t *testing.T) {
	raw, err := os.ReadFile("../../api/business-action/v1/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Action  business.SignedAction  `json:"action"`
		Receipt business.ActionReceipt `json:"receipt"`
	}
	if json.Unmarshal(raw, &fixture) != nil {
		t.Fatal("shared synthetic transport fixture")
	}
	e := &experiment{directory: t.TempDir(), used: map[string]bool{}}
	f := fault{RunID: fixture.Action.Authorization.RunID, TenantID: fixture.Action.Authorization.TenantID, ProfileHash: strings.Repeat("a", 64), Boundary: "business_committed"}
	if err := e.publish("fault.json", f); err != nil {
		t.Fatal(err)
	}
	var sent atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sent.Add(1)
		_ = json.NewEncoder(w).Encode(fixture.Receipt)
	}))
	defer upstream.Close()
	proxy := httptest.NewServer(e.httpForward(upstream.URL))
	defer proxy.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	body, _ := json.Marshal(fixture.Action)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, proxy.URL+"/business/v1/actions/apply_ticket_resolution", strings.NewReader(string(body)))
	done := make(chan struct{})
	go func() {
		defer close(done)
		response, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = response.Body.Close()
		}
	}()
	barrier := filepath.Join(e.directory, f.RunID+".barrier.json")
	until := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(barrier); err == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("actual receipt barrier was not published")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(e.directory, f.RunID+".active")); err != nil || sent.Load() != 1 {
		t.Fatal("active boundary lost or repeated", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("physical client did not stop")
	}
	until = time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(filepath.Join(e.directory, f.RunID+".active")); os.IsNotExist(err) {
			break
		}
		if time.Now().After(until) {
			t.Fatal("stale active marker survived cancellation")
		}
		time.Sleep(time.Millisecond)
	}
}
