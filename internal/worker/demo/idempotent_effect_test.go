package demo

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	promclient "github.com/prometheus/client_golang/prometheus"

	"github.com/xjfyrh/jobforge/internal/observability"
	"github.com/xjfyrh/jobforge/internal/worker"
)

type effectStoreFunc func(context.Context, string) (EffectResult, error)

func (f effectStoreFunc) Apply(ctx context.Context, jobID string) (EffectResult, error) {
	return f(ctx, jobID)
}

func TestIdempotentEffectHandlerOutcomes(t *testing.T) {
	ctx := context.Background()
	registry := promclient.NewRegistry()
	metrics, shutdown, err := observability.SetupMetrics(ctx, registry)
	if err != nil {
		t.Fatalf("setup metrics: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	job := &worker.ClaimedJob{
		ID:      "11111111-1111-1111-1111-111111111111",
		Payload: []byte(`{"secret":"must-not-be-logged"}`),
	}

	t.Run("applied", func(t *testing.T) {
		var calls atomic.Int32
		handler := NewIdempotentEffectHandler(effectStoreFunc(
			func(_ context.Context, jobID string) (EffectResult, error) {
				calls.Add(1)
				return EffectResult{ResultRef: "effect:" + jobID, Applied: true}, nil
			},
		), logger, metrics)

		result, err := handler.Execute(ctx, job)
		if err != nil {
			t.Fatalf("execute applied effect: %v", err)
		}
		if result != "effect:"+job.ID || calls.Load() != 1 {
			t.Fatalf("result=%q calls=%d", result, calls.Load())
		}
	})

	t.Run("deduplicated skips post effect delay", func(t *testing.T) {
		handler := NewIdempotentEffectHandler(effectStoreFunc(
			func(_ context.Context, jobID string) (EffectResult, error) {
				return EffectResult{ResultRef: "effect:" + jobID, Applied: false}, nil
			},
		), logger, metrics)
		delayedJob := *job
		delayedJob.Payload = []byte(`{"post_effect_delay_ms":60000}`)

		started := time.Now()
		result, err := handler.Execute(ctx, &delayedJob)
		if err != nil {
			t.Fatalf("execute duplicate effect: %v", err)
		}
		if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
			t.Fatalf("duplicate delivery waited for post-effect delay: %s", elapsed)
		}
		if result != "effect:"+job.ID {
			t.Fatalf("duplicate result=%q", result)
		}
	})

	t.Run("store failure is retryable", func(t *testing.T) {
		handler := NewIdempotentEffectHandler(effectStoreFunc(
			func(context.Context, string) (EffectResult, error) {
				return EffectResult{}, errors.New("database unavailable")
			},
		), logger, metrics)
		_, err := handler.Execute(ctx, job)
		if err == nil || !worker.IsRetryable(err) {
			t.Fatalf("store error must be retryable, got %v", err)
		}
	})

	if strings.Contains(logs.String(), "must-not-be-logged") {
		t.Fatal("structured log leaked payload content")
	}
	for _, want := range []string{job.ID, `"effect_outcome":"applied"`, `"effect_outcome":"deduplicated"`, `"effect_outcome":"failed"`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("structured logs missing %q: %s", want, logs.String())
		}
	}

	wantCounters := map[string]float64{
		"applied":      1,
		"deduplicated": 1,
		"failed":       1,
	}
	for outcome, want := range wantCounters {
		if got := gatheredCounter(t, registry, "jobforge_demo_idempotent_effects_total", "outcome", outcome); got != want {
			t.Errorf("outcome %s counter=%v, want %v", outcome, got, want)
		}
	}
}

func TestIdempotentEffectHandlerValidationAndCancellation(t *testing.T) {
	var calls atomic.Int32
	store := effectStoreFunc(func(_ context.Context, jobID string) (EffectResult, error) {
		calls.Add(1)
		return EffectResult{ResultRef: "effect:" + jobID, Applied: true}, nil
	})
	handler := NewIdempotentEffectHandler(store, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), nil)

	for name, payload := range map[string][]byte{
		"malformed":      []byte(`{"post_effect_delay_ms":`),
		"negative delay": []byte(`{"post_effect_delay_ms":-1}`),
		"oversized delay": []byte(
			`{"post_effect_delay_ms":60001}`,
		),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := handler.Execute(context.Background(), &worker.ClaimedJob{
				ID:      "22222222-2222-2222-2222-222222222222",
				Payload: payload,
			})
			if err == nil || worker.IsRetryable(err) {
				t.Fatalf("validation error must be fatal, got %v", err)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid payload reached effect store %d times", calls.Load())
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := handler.Execute(ctx, &worker.ClaimedJob{
		ID:      "33333333-3333-3333-3333-333333333333",
		Payload: []byte(`{"post_effect_delay_ms":60000}`),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("post-effect delay did not honor cancellation: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("cancel scenario applied calls=%d, want 1", calls.Load())
	}
}

func TestIdempotentEffectHandlerPreservesStoreContextError(t *testing.T) {
	handler := NewIdempotentEffectHandler(effectStoreFunc(
		func(context.Context, string) (EffectResult, error) {
			return EffectResult{}, context.Canceled
		},
	), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), nil)

	_, err := handler.Execute(context.Background(), &worker.ClaimedJob{
		ID: "44444444-4444-4444-4444-444444444444",
	})
	if !errors.Is(err, context.Canceled) || worker.IsRetryable(err) {
		t.Fatalf("context error must be preserved and non-retryable, got %v", err)
	}
}

func gatheredCounter(
	t *testing.T,
	registry *promclient.Registry,
	metricName, labelName, labelValue string,
) float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != metricName {
			continue
		}
		for _, sample := range family.Metric {
			for _, label := range sample.Label {
				if label.GetName() == labelName && label.GetValue() == labelValue {
					return sample.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}
