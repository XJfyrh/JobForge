// Command clockcheck checks PostgreSQL time against the client's monotonic
// clock before latency/lease acceptance. It reads only clock_timestamp(); it
// never changes clocks, task data, or SLO thresholds.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
)

type sample struct {
	ElapsedSeconds float64   `json:"elapsed_s"`
	HostUTC        time.Time `json:"host_utc"`
	DatabaseUTC    time.Time `json:"database_utc"`
	RTTMillis      float64   `json:"rtt_ms"`
	OffsetMillis   float64   `json:"offset_ms"`
	StepMillis     float64   `json:"step_ms"`
}

type report struct {
	Passed          bool     `json:"passed"`
	MaxOffsetMillis float64  `json:"max_offset_lower_bound_ms"`
	MaxStepMillis   float64  `json:"max_step_lower_bound_ms"`
	MaxRTTMillis    float64  `json:"max_rtt_ms"`
	Samples         []sample `json:"samples"`
}

// Uncertainty is bounded by the SQL round trip. A slow response alone must
// not be diagnosed as a clock step, but remains visible in the report.
func summarize(samples []sample, maxOffset, maxStep, maxRTT time.Duration) report {
	r := report{Passed: len(samples) >= 2, Samples: samples}
	for i, s := range samples {
		r.MaxRTTMillis = max(r.MaxRTTMillis, s.RTTMillis)
		r.MaxOffsetMillis = max(r.MaxOffsetMillis, math.Abs(s.OffsetMillis)-s.RTTMillis/2)
		if i > 0 {
			uncertainty := (s.RTTMillis + samples[i-1].RTTMillis) / 2
			r.MaxStepMillis = max(r.MaxStepMillis, math.Abs(s.StepMillis)-uncertainty)
		}
	}
	r.Passed = r.Passed && r.MaxOffsetMillis <= milliseconds(maxOffset) &&
		r.MaxStepMillis <= milliseconds(maxStep) && r.MaxRTTMillis <= milliseconds(maxRTT)
	return r
}

func milliseconds(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func probe(ctx context.Context, dsn string, duration, interval time.Duration) ([]sample, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("database connection failed (details suppressed)")
	}
	defer func() { _ = conn.Close(context.Background()) }()
	start := time.Now()
	var previousDB, previousMid time.Time
	var samples []sample
	for time.Since(start) < duration {
		before := time.Now()
		var db time.Time
		queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = conn.QueryRow(queryCtx, "select clock_timestamp()").Scan(&db)
		cancel()
		if err != nil {
			return samples, fmt.Errorf("clock query failed (details suppressed)")
		}
		after := time.Now()
		mid := before.Add(after.Sub(before) / 2)
		var step time.Duration
		if !previousDB.IsZero() {
			// mid retains Go's monotonic reading; PostgreSQL supplies wall time.
			step = db.Sub(previousDB) - mid.Sub(previousMid)
		}
		samples = append(samples, sample{
			ElapsedSeconds: after.Sub(start).Seconds(), HostUTC: mid.UTC(), DatabaseUTC: db.UTC(),
			RTTMillis: milliseconds(after.Sub(before)), OffsetMillis: milliseconds(db.Sub(mid)),
			StepMillis: milliseconds(step),
		})
		previousDB, previousMid = db, mid
		timer := time.NewTimer(interval)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return samples, fmt.Errorf("clock probe interrupted")
		}
	}
	return samples, nil
}

func main() {
	duration := flag.Duration("duration", time.Minute, "sampling duration")
	interval := flag.Duration("interval", 200*time.Millisecond, "delay between samples")
	maxOffset := flag.Duration("max-offset", 100*time.Millisecond, "maximum host/DB clock offset")
	maxStep := flag.Duration("max-step", 100*time.Millisecond, "maximum clock step vs monotonic time")
	maxRTT := flag.Duration("max-rtt", 250*time.Millisecond, "maximum SQL round trip for a healthy test environment")
	flag.Parse()
	if *interval < time.Millisecond || *duration < 2*(*interval) || *duration > time.Hour ||
		*maxOffset <= 0 || *maxStep <= 0 || *maxRTT <= 0 {
		fmt.Fprintln(os.Stderr, "invalid sampling duration, interval, or limits")
		os.Exit(2)
	}
	dsn := os.Getenv("JOBFORGE_TEST_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "JOBFORGE_TEST_DSN is required; use a disposable test database")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *duration+10*time.Second)
	defer cancel()
	samples, err := probe(ctx, dsn, *duration, *interval)
	r := summarize(samples, *maxOffset, *maxStep, *maxRTT)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		r.Passed = false
	}
	if err := json.NewEncoder(os.Stdout).Encode(r); err != nil {
		fmt.Fprintln(os.Stderr, "cannot write clock report")
		os.Exit(1)
	}
	if !r.Passed {
		fmt.Fprintln(os.Stderr, "clock/SQL environment check failed; see docs/runbooks/windows-acceptance.md")
		os.Exit(1)
	}
}
