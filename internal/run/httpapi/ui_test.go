package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xjfyrh/jobforge/internal/run"
)

func TestUIResourcesAndIdentityBoundary(t *testing.T) {
	api := &testAPI{}
	router, err := NewRouter(api, map[string]Identity{"operator-key": {TenantID: "north", Role: "operator"}, "approver-key": {TenantID: "south", Role: "approver", ActorID: "reviewer-1"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/ui/", "/ui/app.js", "/ui/style.css"} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" ||
			!strings.Contains(response.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
			strings.Contains(response.Body.String(), "approver-key") {
			t.Fatalf("unsafe static resource %s: %d", path, response.Code)
		}
	}
	for _, path := range []string{"/ui/missing", "/ui/index.html?credential=secret", "/ui/../router.go"} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("non-resource exposed: %s", path)
		}
	}
	for _, key := range []string{"", "operator-key", "approver-key"} {
		request := httptest.NewRequest(http.MethodGet, "/v2/identity", nil)
		if key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if key == "" {
			if response.Code != http.StatusUnauthorized {
				t.Fatal("identity without credential")
			}
			continue
		}
		var identity map[string]string
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &identity) != nil || len(identity) != 3 ||
			identity["role"] != strings.TrimSuffix(key, "-key") {
			t.Fatalf("identity not server-derived: %s", response.Body.String())
		}
	}
	if len(api.calls) != 0 {
		t.Fatal("static/session requests invoked execution service")
	}
}

func TestBrowserMutationRejectsForeignOriginBeforeExecution(t *testing.T) {
	api := &testAPI{}
	router, err := NewRouter(api, map[string]Identity{"operator-key": {TenantID: "north", Role: "operator"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"https://foreign.test", "null", "http://example.com/path", "http://example.com?query", "http://user@example.com", "https://example.com"} {
		request := httptest.NewRequest(http.MethodPost, "http://example.com/v2/runs", strings.NewReader(`{}`))
		request.Header.Set("Authorization", "Bearer operator-key")
		request.Header.Set("Origin", origin)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden || len(api.calls) != 0 {
			t.Fatalf("foreign origin reached domain: %s", origin)
		}
	}
	for _, origin := range []string{"", "http://example.com"} {
		request := httptest.NewRequest(http.MethodPost, "http://example.com/v2/runs", strings.NewReader(`{}`))
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		if !sameOrigin(request) {
			t.Fatalf("same-origin or bearer SDK refused: %s", origin)
		}
	}
	_, status := publicError(run.ErrForbidden)
	if status != http.StatusForbidden {
		t.Fatal("unstable origin rejection")
	}
}
