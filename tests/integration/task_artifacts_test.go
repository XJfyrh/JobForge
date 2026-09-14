package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/xjfyrh/jobforge/internal/config"
	"github.com/xjfyrh/jobforge/internal/tasks"
)

func TestTaskArtifactsConcurrentPublicationAndIsolation(t *testing.T) {
	ctx := t.Context()
	s := tasks.NewPostgresArtifacts(testEnv.pool)
	key := uuid.NewString()
	a := &tasks.Artifact{TenantID: "artifact-test", TaskType: "rag.index", BusinessKey: key,
		ResultRef: "jobforge-artifact:" + strings.ReplaceAll(uuid.NewString(), "-", "") + strings.ReplaceAll(uuid.NewString(), "-", ""), Fingerprint: strings.Repeat("a", 64), Body: json.RawMessage(`{"test":true}`)}
	var applied atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			stored, changed, err := s.Publish(ctx, a)
			if err != nil {
				t.Error(err)
				return
			}
			if changed {
				applied.Add(1)
			}
			if stored.ResultRef != a.ResultRef {
				t.Error("result changed")
			}
		})
	}
	wg.Wait()
	if applied.Load() != 1 {
		t.Fatalf("publications=%d", applied.Load())
	}
	otherKey := *a
	otherKey.BusinessKey = uuid.NewString()
	if _, _, err := s.Publish(ctx, &otherKey); err == nil || err.Error() != "BUSINESS_KEY_CONFLICT" {
		t.Fatalf("reference collision did not fail closed: %v", err)
	}
	if _, err := s.Get(ctx, "foreign", a.ResultRef); !errors.Is(err, tasks.ErrNotFound) {
		t.Fatalf("foreign=%v", err)
	}
	a.Fingerprint = strings.Repeat("b", 64)
	if _, _, err := s.Publish(ctx, a); err == nil || err.Error() != "BUSINESS_KEY_CONFLICT" {
		t.Fatalf("conflict=%v", err)
	}
	a.Body = json.RawMessage(`"` + strings.Repeat("x", tasks.MaxArtifactBytes) + `"`)
	if _, _, err := s.Publish(ctx, a); err == nil || err.Error() != "ARTIFACT_TOO_LARGE" {
		t.Fatalf("oversize=%v", err)
	}
	if _, err := testEnv.pool.Exec(ctx, `insert into task_artifacts (tenant_id, task_type, business_key, result_ref, fingerprint, body)
		values ('invalid', 'rag.index', 'k', 'bad-reference', $1, '{}')`, strings.Repeat("c", 64)); err == nil {
		t.Fatal("database accepted invalid reference")
	}
	model := &artifactNoModel{}
	server := httptest.NewServer(tasks.NewArtifactRouter(s, model, &config.Config{APIKeys: map[string]string{"owner": "artifact-test", "foreign": "foreign"}}))
	defer server.Close()
	path := "/v1/artifacts/" + strings.TrimPrefix(a.ResultRef, "jobforge-artifact:")
	for _, tc := range []struct {
		key    string
		status int
	}{{"", 401}, {"foreign", 404}, {"owner", 200}} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+path, nil)
		if tc.key != "" {
			req.Header.Set("Authorization", "Bearer "+tc.key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Fatalf("%s: %d", tc.key, resp.StatusCode)
		}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+path+"/search", strings.NewReader(`{"query":"private","k":1}`))
	req.Header.Set("Authorization", "Bearer foreign")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 404 || model.calls.Load() != 0 {
		t.Fatal("foreign search reached model")
	}
}

// A substitute proving authorization occurs before any inference call.
type artifactNoModel struct{ calls atomic.Int32 }

func (m *artifactNoModel) Embed(context.Context, []string) ([][]float64, error) {
	m.calls.Add(1)
	return nil, errors.New("unexpected model call")
}
func (m *artifactNoModel) Extract(context.Context, string, string) (string, error) {
	m.calls.Add(1)
	return "", errors.New("unexpected model call")
}
