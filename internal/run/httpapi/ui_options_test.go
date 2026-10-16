package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUIOptionsAreOperatorOnlyTenantScopedAndCopied(t *testing.T) {
	api := &testAPI{}
	keys := map[string]Identity{
		"operator-north": {TenantID: "north", Role: "operator"}, "operator-south": {TenantID: "south", Role: "operator"},
		"reader": {TenantID: "north", Role: "reader"}, "approver": {TenantID: "north", Role: "approver", ActorID: "reviewer"},
	}
	options := []UIOption{{"north", "标准工单处理", "registered-north", "batch-north"}, {"south", "其他团队方式", "registered-south", "batch-south"}}
	router, err := NewRouter(api, keys, options...)
	if err != nil {
		t.Fatal(err)
	}
	options[0].Name = "changed-after-construction"
	for _, tc := range []struct {
		key, path, name string
		status          int
	}{
		{"", "/v2/ui-options", "", http.StatusUnauthorized},
		{"reader", "/v2/ui-options", "", http.StatusForbidden},
		{"approver", "/v2/ui-options", "", http.StatusForbidden},
		{"operator-north", "/v2/ui-options?tenant=south", "", http.StatusBadRequest},
		{"operator-north", "/v2/ui-options", "标准工单处理", http.StatusOK},
		{"operator-south", "/v2/ui-options", "其他团队方式", http.StatusOK},
	} {
		r := httptest.NewRequest(http.MethodGet, tc.path, nil)
		if tc.key != "" {
			r.Header.Set("Authorization", "Bearer "+tc.key)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != tc.status || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s %s: %d", tc.key, tc.path, w.Code)
		}
		if tc.status != http.StatusOK {
			continue
		}
		var body struct {
			Items []map[string]string `json:"items"`
		}
		if json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Items) != 1 || len(body.Items[0]) != 3 || body.Items[0]["name"] != tc.name {
			t.Fatalf("incorrect option projection: %s", w.Body.String())
		}
		if _, exposed := body.Items[0]["tenant_id"]; exposed {
			t.Fatal("configuration tenant leaked into option")
		}
	}
	if len(api.calls) != 0 {
		t.Fatal("reading UI options invoked the execution service")
	}
	empty, err := NewRouter(api, keys)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/v2/ui-options", nil)
	r.Header.Set("Authorization", "Bearer operator-north")
	w := httptest.NewRecorder()
	empty.ServeHTTP(w, r)
	if strings.TrimSpace(w.Body.String()) != `{"items":[]}` {
		t.Fatalf("unconfigured options must be empty: %s", w.Body.String())
	}
}

func TestUIOptionsRejectAmbiguousConfiguration(t *testing.T) {
	valid := UIOption{"north", "标准工单处理", "registered", "batch"}
	for _, options := range [][]UIOption{
		{{"", "处理", "registered", "batch"}}, {{"north", "处理", "", "batch"}},
		{{"north", "处理", "registered", ""}}, {{"north", "\n", "registered", "batch"}},
		{{"north", strings.Repeat("a", 161), "registered", "batch"}}, {valid, valid}, make([]UIOption, 513),
	} {
		if _, err := NewRouter(&testAPI{}, map[string]Identity{"key": {TenantID: "north", Role: "operator"}}, options...); err == nil {
			t.Fatal("invalid UI option accepted")
		}
	}
}
