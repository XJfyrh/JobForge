package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/xjfyrh/jobforge/internal/observability"
	"github.com/xjfyrh/jobforge/internal/store"
)

type depthStore struct {
	Store
	rows []store.QueueDepthRow
	err  error
}

func (d *depthStore) QueueDepthMetrics(context.Context) ([]store.QueueDepthRow, error) {
	return d.rows, d.err
}

func TestQueueDepthClearsDisappearedSeriesAndPreservesOnError(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, shutdown, err := observability.SetupMetrics(t.Context(), reg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	d := &depthStore{rows: []store.QueueDepthRow{{TenantID: "a", Queue: "q", State: "ready", Count: 3}}}
	s := New(d, nil, nil, DefaultConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)), m)
	assertDepth := func(want float64) {
		t.Helper()
		families, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range families {
			if f.GetName() == "jobforge_queue_depth" {
				if len(f.Metric) != 1 || f.Metric[0].GetGauge().GetValue() != want {
					t.Fatalf("depth=%v want %v", f.Metric, want)
				}
				return
			}
		}
		t.Fatal("missing depth metric")
	}
	s.recordQueueDepth(t.Context())
	assertDepth(3)
	d.rows, d.err = nil, errors.New("database temporarily unavailable")
	s.recordQueueDepth(t.Context())
	assertDepth(3)
	d.err = nil
	s.recordQueueDepth(t.Context())
	assertDepth(0)
	s.recordQueueDepth(t.Context())
	assertDepth(0)
}
