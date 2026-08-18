package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	apihttp "github.com/xjfyrh/jobforge/internal/api/http"
)

// TestAT28TaskTypeSubmissionContract verifies that a catalog type may be
// submitted while no Worker is online and that an unknown type has no jobs or
// outbox side effect (PRD v0.5 AT-28).
func TestAT28TaskTypeSubmissionContract(t *testing.T) {
	jobStore := setupStore(t)
	handler := apihttp.NewJobHandler(
		jobStore,
		jobStore,
		testTaskTypeCatalog(t),
		slog.Default(),
		nil,
	)
	tenantID := "at28-tenant-" + uuid.New().String()[:8]

	submit := func(taskType string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"queue":   "at28-offline",
			"type":    taskType,
			"payload": map[string]string{"proof": "at28"},
		})
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), apihttp.TenantIDKey, tenantID))
		recorder := httptest.NewRecorder()
		handler.CreateJob(recorder, req)
		return recorder
	}

	valid := submit("demo.echo")
	if valid.Code != http.StatusAccepted {
		t.Fatalf("offline valid submit = %d: %s", valid.Code, valid.Body.String())
	}

	countRows := func() (jobs int, outbox int) {
		t.Helper()
		ctx := context.Background()
		if err := testEnv.pool.QueryRow(ctx,
			`select count(*) from jobs where tenant_id = $1`, tenantID,
		).Scan(&jobs); err != nil {
			t.Fatalf("count jobs: %v", err)
		}
		if err := testEnv.pool.QueryRow(ctx, `
			select count(*)
			from outbox_events oe
			join jobs j on j.id = oe.aggregate_id
			where j.tenant_id = $1`, tenantID,
		).Scan(&outbox); err != nil {
			t.Fatalf("count outbox: %v", err)
		}
		return jobs, outbox
	}

	jobsBefore, outboxBefore := countRows()
	if jobsBefore != 1 || outboxBefore != 0 {
		t.Fatalf("valid submit effects = jobs %d, outbox %d; want 1/0", jobsBefore, outboxBefore)
	}

	unknown := submit("demo.unknown")
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("unknown submit = %d: %s", unknown.Code, unknown.Body.String())
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(unknown.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode unknown response: %v", err)
	}
	if envelope.Error.Code != "INVALID_ARGUMENT" || envelope.Error.Message != "task type is not registered" {
		t.Fatalf("unexpected unknown response: %+v", envelope.Error)
	}

	jobsAfter, outboxAfter := countRows()
	if jobsAfter != jobsBefore || outboxAfter != outboxBefore {
		t.Fatalf("unknown type changed state: jobs %d->%d, outbox %d->%d",
			jobsBefore, jobsAfter, outboxBefore, outboxAfter)
	}
}
