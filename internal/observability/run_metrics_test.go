package observability

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestSnapshotMetricErrorsAndCache(t *testing.T) {
	c := NewRunCollector(nil)
	if _, err := c.snapshotMetric("current", "ready", "", 1); err != nil {
		t.Fatal(err)
	}
	c.metrics["broken"] = runMetric{desc: prometheus.NewDesc("test_broken", "test", []string{"one", "two"}, nil), labels: 1}
	for _, name := range []string{"missing", "broken"} {
		if _, err := c.snapshotMetric(name, "ready", "", 1); err == nil {
			t.Fatal("invalid labels accepted", name)
		}
	}
	// A nil pool makes any accidental query observable as a panic. Both complete
	// success and explicit failure must reuse the bounded snapshot interval.
	for _, success := range []bool{true, false} {
		c.refreshed, c.available = time.Now(), success
		metrics, available := c.snapshot()
		if metrics != nil || available != success {
			t.Fatal("cached availability changed")
		}
	}
}
