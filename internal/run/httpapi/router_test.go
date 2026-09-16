package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/xjfyrh/jobforge/internal/run"
)

type fixtures struct {
	Submit       json.RawMessage    `json:"submit"`
	Explicit     json.RawMessage    `json:"submit_explicit_default"`
	Retry        json.RawMessage    `json:"retry"`
	Cancel       json.RawMessage    `json:"cancel"`
	Run          run.Run            `json:"run"`
	Submission   run.SubmitResponse `json:"submission"`
	Cancellation run.CancelResponse `json:"cancellation"`
	Page         run.Page           `json:"page"`
	Steps        run.StepPage       `json:"steps"`
	Events       run.EventPage      `json:"events"`
	Result       run.Result         `json:"result"`
	Errors       []errorFixture     `json:"errors"`
}

type errorFixture struct {
	Status int `json:"status"`
	Body   struct {
		Error run.Failure `json:"error"`
	} `json:"body"`
}

type testAPI struct {
	fixture fixtures
	calls   []string
	key     string
	submit  run.SubmitRequest
	retry   run.RetryRequest
	filter  run.ListFilter
	after   int64
	limit   int
	err     error
	ctx     context.Context
}

func (a *testAPI) record(ctx context.Context, method, tenant, id string) error {
	a.calls = append(a.calls, method)
	a.ctx = ctx
	if a.err != nil {
		return a.err
	}
	if tenant != a.fixture.Run.TenantID || (id != "" && id != a.fixture.Run.ID) {
		return run.ErrNotFound
	}
	return nil
}

func (a *testAPI) Submit(ctx context.Context, tenant, key string, req run.SubmitRequest) (run.SubmitResponse, error) {
	a.key, a.submit = key, req
	return a.fixture.Submission, a.record(ctx, "submit", tenant, "")
}

func (a *testAPI) Get(ctx context.Context, tenant, id string) (run.Run, error) {
	return a.fixture.Run, a.record(ctx, "get", tenant, id)
}

func (a *testAPI) List(ctx context.Context, tenant string, filter run.ListFilter) (run.Page, error) {
	a.filter = filter
	return a.fixture.Page, a.record(ctx, "list", tenant, "")
}

func (a *testAPI) Steps(ctx context.Context, tenant, id string, after int64, limit int) (run.StepPage, error) {
	a.after, a.limit = after, limit
	return a.fixture.Steps, a.record(ctx, "steps", tenant, id)
}

func (a *testAPI) Events(ctx context.Context, tenant, id string, after int64, limit int) (run.EventPage, error) {
	a.after, a.limit = after, limit
	return a.fixture.Events, a.record(ctx, "events", tenant, id)
}

func (a *testAPI) Result(ctx context.Context, tenant, id string) (run.Result, error) {
	return a.fixture.Result, a.record(ctx, "result", tenant, id)
}

func (a *testAPI) Cancel(ctx context.Context, tenant, id, key string) (run.CancelResponse, error) {
	a.key = key
	return a.fixture.Cancellation, a.record(ctx, "cancel", tenant, id)
}

func (a *testAPI) Retry(ctx context.Context, tenant, id, key string, req run.RetryRequest) (run.SubmitResponse, error) {
	a.key, a.retry = key, req
	return a.fixture.Submission, a.record(ctx, "retry", tenant, id)
}

func newTestRouter(t *testing.T) (http.Handler, *testAPI) {
	t.Helper()
	body, err := os.ReadFile("../../../api/run/v2/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	api := &testAPI{}
	if err := json.Unmarshal(body, &api.fixture); err != nil {
		t.Fatal(err)
	}
	router, err := NewRouter(api, map[string]Identity{
		"operator": {TenantID: api.fixture.Run.TenantID, Role: "operator"},
		"reader":   {TenantID: api.fixture.Run.TenantID, Role: "reader"},
		"foreign":  {TenantID: "another-tenant", Role: "operator"},
		"outsider": {TenantID: "another-tenant", Role: "reader"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return router, api
}

func request(method, path, key string, body []byte) *http.Request {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "operation-key")
	return r
}

func serve(router http.Handler, r *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	router.ServeHTTP(response, r)
	return response
}

func assertCode(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var body struct {
		Error run.Failure `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid response JSON: %v", err)
	}
	if response.Code != status || body.Error.Code != code {
		t.Fatalf("response status/code = %d/%s; want %d/%s", response.Code, body.Error.Code, status, code)
	}
}

func TestRoutesUseSharedResponseFixture(t *testing.T) {
	router, api := newTestRouter(t)
	base := "/v2/runs/" + api.fixture.Run.ID
	for _, test := range []struct {
		name, method, path string
		body               []byte
		response           any
		status             int
	}{
		{"submit", "POST", "/v2/runs", api.fixture.Submit, api.fixture.Submission, 201},
		{"get", "GET", base, nil, api.fixture.Run, 200},
		{"list", "GET", "/v2/runs", nil, api.fixture.Page, 200},
		{"steps", "GET", base + "/steps", nil, api.fixture.Steps, 200},
		{"events", "GET", base + "/events", nil, api.fixture.Events, 200},
		{"result", "GET", base + "/result", nil, api.fixture.Result, 200},
		{"cancel", "POST", base + "/cancel", api.fixture.Cancel, api.fixture.Cancellation, 200},
		{"retry", "POST", base + "/retry", api.fixture.Retry, api.fixture.Submission, 201},
	} {
		t.Run(test.name, func(t *testing.T) {
			api.calls = nil
			response := serve(router, request(test.method, test.path, "operator", test.body))
			expected, err := json.Marshal(test.response)
			if err != nil {
				t.Fatal(err)
			}
			if response.Code != test.status || !bytes.Equal(bytes.TrimSpace(response.Body.Bytes()), expected) {
				t.Fatalf("response differs from shared %s fixture; status=%d", test.name, response.Code)
			}
			if len(api.calls) != 1 || api.calls[0] != test.name {
				t.Fatalf("thin route called %v", api.calls)
			}
			if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Type") != "application/json" {
				t.Fatal("missing private JSON response headers")
			}
		})
	}
	if api.submit.RunTimeoutSeconds != 3600 || api.retry.RunTimeoutSeconds != 3600 || api.key != "operation-key" {
		t.Fatal("operation identity or frozen default was not passed to service")
	}
	if api.after != 0 || api.limit != 20 || api.filter.Limit != 20 {
		t.Fatal("page defaults differ from source contract")
	}
	api.fixture.Submission.Reused = true
	for _, path := range []string{"/v2/runs", base + "/retry"} {
		body := api.fixture.Submit
		if strings.HasSuffix(path, "/retry") {
			body = api.fixture.Retry
		}
		if got := serve(router, request("POST", path, "operator", body)); got.Code != 200 {
			t.Fatalf("accepted reuse status = %d", got.Code)
		}
	}
}

func TestStrictInputDoesNotReachService(t *testing.T) {
	router, api := newTestRouter(t)
	valid := strings.TrimSpace(string(api.fixture.Submit))
	end := strings.TrimSuffix(valid, "}")
	for name, body := range map[string]string{
		"unknown":      end + `,"tenant_id":"other"}`,
		"duplicate":    end + `,"schema_version":1}`,
		"alias":        end + `,"Run_Timeout_Seconds":1}`,
		"null":         end + `,"run_timeout_seconds":null}`,
		"decimal":      end + `,"run_timeout_seconds":1.0}`,
		"exponent":     end + `,"run_timeout_seconds":1e1}`,
		"boolean":      end + `,"run_timeout_seconds":true}`,
		"zero":         end + `,"run_timeout_seconds":0}`,
		"overflow":     end + `,"run_timeout_seconds":9007199254740992}`,
		"too_large":    end + `,"run_timeout_seconds":86401}`,
		"missing":      `{}`,
		"null_root":    `null`,
		"array":        `[]`,
		"trailing":     valid + `{}`,
		"invalid_utf8": end + ",\"profile_id\":\"\xff\"}",
		"body_limit":   valid + strings.Repeat(" ", 4096),
	} {
		t.Run(name, func(t *testing.T) {
			api.calls = nil
			response := serve(router, request("POST", "/v2/runs", "operator", []byte(body)))
			assertCode(t, response, 400, "INVALID_ARGUMENT")
			if len(api.calls) != 0 {
				t.Fatal("invalid request reached service")
			}
		})
	}
	for _, header := range []string{"", "application/octet-stream", "application/json; charset=latin-1"} {
		r := request("POST", "/v2/runs", "operator", api.fixture.Submit)
		r.Header.Set("Content-Type", header)
		assertCode(t, serve(router, r), 400, "INVALID_ARGUMENT")
	}
	for _, key := range []string{"", " bad", "-bad", "non-ascii-中文", strings.Repeat("a", 129)} {
		r := request("POST", "/v2/runs", "operator", api.fixture.Submit)
		r.Header.Set("Idempotency-Key", key)
		assertCode(t, serve(router, r), 400, "INVALID_ARGUMENT")
	}
	r := request("POST", "/v2/runs", "operator", api.fixture.Submit)
	r.Header.Add("Idempotency-Key", "second")
	assertCode(t, serve(router, r), 400, "INVALID_ARGUMENT")
	for _, path := range []string{"/v2/runs/invalid", "/v2/runs/11111111111141118111111111111111", "/v2/runs/AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"} {
		assertCode(t, serve(router, request("GET", path, "operator", nil)), 400, "INVALID_ARGUMENT")
	}
}

func TestAuthenticationAndRoleDoNotRevealForeignRuns(t *testing.T) {
	router, api := newTestRouter(t)
	base := "/v2/runs/" + api.fixture.Run.ID
	for _, header := range []string{"", "Bearer wrong", "Basic operator", "Bearer operator extra", "Bearer  operator"} {
		r := request("GET", base, "", nil)
		r.Header.Set("Authorization", header)
		assertCode(t, serve(router, r), 401, "UNAUTHORIZED")
	}
	r := request("GET", base, "operator", nil)
	r.Header.Add("Authorization", "Bearer reader")
	assertCode(t, serve(router, r), 401, "UNAUTHORIZED")
	if len(api.calls) != 0 {
		t.Fatal("invalid authentication reached service")
	}
	assertCode(t, serve(router, request("POST", "/v2/runs", "reader", api.fixture.Submit)), 403, "FORBIDDEN")
	for _, operation := range []string{"cancel", "retry"} {
		for key, status := range map[string]int{"reader": 403, "foreign": 404, "outsider": 404} {
			api.calls = nil
			response := serve(router, request("POST", base+"/"+operation, key, api.fixture.Cancel))
			code := "NOT_FOUND"
			if status == 403 {
				code = "FORBIDDEN"
			}
			assertCode(t, response, status, code)
			if len(api.calls) != 1 || (key != "foreign" && api.calls[0] != "get") {
				t.Fatalf("reader must do only tenant-scoped lookup: %v", api.calls)
			}
		}
	}
	for _, suffix := range []string{"", "/steps", "/events", "/result"} {
		assertCode(t, serve(router, request("GET", base+suffix, "foreign", nil)), 404, "NOT_FOUND")
		if response := serve(router, request("GET", base+suffix, "reader", nil)); response.Code != 200 {
			t.Fatalf("reader query status=%d", response.Code)
		}
	}
}

func TestPageSyntaxAndExplicitParameters(t *testing.T) {
	router, api := newTestRouter(t)
	base := "/v2/runs/" + api.fixture.Run.ID
	for _, path := range []string{
		"/v2/runs?state=dead", "/v2/runs?state=ready&state=ready", "/v2/runs?cursor=",
		"/v2/runs?limit=101", "/v2/runs?limit=0", "/v2/runs?limit=1.0", "/v2/runs?limit=01",
		"/v2/runs?limit=%2B1", "/v2/runs?unknown=1", "/v2/runs?limit=%zz",
		"/v2/runs?cursor=" + strings.Repeat("a", 4097), base + "?state=ready",
		base + "/steps?after=-1", base + "/steps?after=9007199254740992",
		base + "/events?after=1&after=2", base + "/events?after=1e2",
	} {
		assertCode(t, serve(router, request("GET", path, "reader", nil)), 400, "INVALID_ARGUMENT")
	}
	if len(api.calls) != 0 {
		t.Fatal("invalid query reached service")
	}
	response := serve(router, request("GET", "/v2/runs?state=failed&limit=7&cursor=opaque_v1", "reader", nil))
	if response.Code != 200 || api.filter != (run.ListFilter{State: run.Failed, Limit: 7, Cursor: "opaque_v1"}) {
		t.Fatalf("list filter changed: %#v", api.filter)
	}
	response = serve(router, request("GET", base+"/events?after=9007199254740991&limit=100", "reader", nil))
	if response.Code != 200 || api.after != run.MaxSafeInteger || api.limit != 100 {
		t.Fatal("history bounds changed")
	}
	api.fixture.Page.Items = nil
	api.fixture.Steps.Items = nil
	api.fixture.Events.Items = nil
	for _, path := range []string{"/v2/runs", base + "/steps", base + "/events"} {
		if body := serve(router, request("GET", path, "reader", nil)).Body.String(); !strings.Contains(body, `"items":[]`) {
			t.Fatal("empty result must be an array, never null")
		}
	}
}

func TestStableErrorsAndBoundedResponses(t *testing.T) {
	router, api := newTestRouter(t)
	base := "/v2/runs/" + api.fixture.Run.ID
	for _, fixture := range api.fixture.Errors {
		api.err = fmt.Errorf("sensitive SQL wrapper: %w", run.ErrorCode(fixture.Body.Error.Code))
		response := serve(router, request("GET", base, "reader", nil))
		assertCode(t, response, fixture.Status, fixture.Body.Error.Code)
		if strings.Contains(response.Body.String(), "sensitive") {
			t.Fatal("internal diagnostic leaked")
		}
	}
	for _, err := range []error{errors.New("sensitive internal failure"), run.ErrorCode("UNKNOWN_SECRET"), run.ErrStaleLease} {
		api.err = err
		response := serve(router, request("GET", base, "reader", nil))
		assertCode(t, response, 500, "INTERNAL")
	}
	api.err = nil
	api.fixture.Run.State = run.Failed
	api.fixture.Run.Error = &run.Failure{Code: "MODEL_PROTOCOL_ERROR", Message: "model protocol error"}
	if response := serve(router, request("GET", base, "reader", nil)); response.Code != 200 {
		t.Fatal("execution failure must remain successful query data")
	}
	api.fixture.Run.BusinessRequestKey = strings.Repeat("x", maxResponseBytes)
	response := serve(router, request("GET", base, "reader", nil))
	assertCode(t, response, 500, "INTERNAL")
	if response.Body.Len() > 256 {
		t.Fatal("oversized response was partly written")
	}
}

func TestRequestContextAndTraceReachOnlyMetadataSpan(t *testing.T) {
	router, api := newTestRouter(t)
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := request("POST", "/v2/runs", "operator", api.fixture.Submit).WithContext(ctx)
	r.Header.Set("traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
	r.Header.Set("tracestate", "vendor=value")
	response := serve(router, r)
	if response.Code != 201 || !errors.Is(api.ctx.Err(), context.Canceled) {
		t.Fatal("transport replaced the caller's canceled context")
	}
	spanContext := trace.SpanContextFromContext(api.ctx)
	if spanContext.TraceID().String() != "11111111111111111111111111111111" || spanContext.TraceState().String() != "vendor=value" {
		t.Fatal("W3C context was not propagated")
	}
	spans := exporter.GetSpans()
	if len(spans) != 1 || len(spans[0].Attributes) != 1 || string(spans[0].Attributes[0].Key) != "http.route" ||
		spans[0].Attributes[0].Value.AsString() != "/v2/runs" {
		t.Fatalf("trace must contain registered route metadata only: %#v", spans)
	}
}

func TestRouterConfigIsValidatedAndCopied(t *testing.T) {
	_, api := newTestRouter(t)
	for _, keys := range []map[string]Identity{
		nil,
		{"": {TenantID: "tenant", Role: "reader"}},
		{"white space": {TenantID: "tenant", Role: "reader"}},
		{"key": {TenantID: "invalid tenant", Role: "reader"}},
		{"key": {TenantID: "tenant", Role: "admin"}},
	} {
		if _, err := NewRouter(api, keys); !errors.Is(err, run.ErrInvalidArgument) {
			t.Fatalf("accepted invalid configuration: %v", err)
		}
	}
	keys := map[string]Identity{"key": {TenantID: api.fixture.Run.TenantID, Role: "reader"}}
	router, err := NewRouter(api, keys)
	if err != nil {
		t.Fatal(err)
	}
	delete(keys, "key")
	if response := serve(router, request("GET", "/v2/runs/"+api.fixture.Run.ID, "key", nil)); response.Code != 200 {
		t.Fatal("router retained the caller's mutable credentials map")
	}
	if _, err := NewRouter(nil, keys); !errors.Is(err, run.ErrInvalidArgument) {
		t.Fatal("accepted nil service")
	}
}
