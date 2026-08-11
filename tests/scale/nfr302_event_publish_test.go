//go:build scale

// NFR-302 smoke floor for the durable event transport (PRD v0.3 §6.1):
// Redis Streams must sustain at least 5x the W11 Process throughput target
// (~1,800 events/sec) with publish lag p95 <= 2s while healthy.
//
// This is a smoke floor, not a capacity report: it runs on dev hardware
// (Docker/WSL2) and only screens out clearly unsuitable configurations. A
// drain rate below the floor is logged as a capacity risk (triggering the
// Kafka gate evaluation per PRD §6.1) instead of failing the test, while the
// healthy steady-state lag p95 <= 2s requirement is enforced.
//
// Requires JOBFORGE_TEST_REDIS_URL (durable-events compose profile); skipped
// otherwise.
package scale

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/xjfyrh/jobforge/internal/outbox"
	"github.com/xjfyrh/jobforge/internal/store"
	"github.com/xjfyrh/jobforge/internal/store/postgres"
)

const nfr302SmokeFloor = 1800 // events/sec, PRD v0.3 §6.1

// TestScaleNFR302EventPublishSmoke inserts a bulk backlog into the outbox,
// drains it through the Redis Streams transport, then measures the healthy
// steady-state publish lag on a fresh wave (created_at to published_at,
// both PostgreSQL clocks — immune to host/container clock skew).
func TestScaleNFR302EventPublishSmoke(t *testing.T) {
	redisURL := os.Getenv("JOBFORGE_TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("JOBFORGE_TEST_REDIS_URL not set; NFR-302 smoke skipped " +
			"(start the durable-events compose profile, PRD v0.3 §10.2)")
	}
	ctx := context.Background()

	n := envInt("JOBFORGE_SCALE_NFR302_EVENTS", 10000)
	wave := envInt("JOBFORGE_SCALE_NFR302_WAVE", 1000)

	// Isolated stream + clean outbox leftovers from earlier scale tests would
	// distort the drain measurement: scope everything to a fresh aggregate.
	streamKey := "test:nfr302:" + uuid.NewString()[:8]
	aggregate := uuid.New().String()
	defer func() {
		client := redis.NewClient(&redis.Options{Addr: redisAddrFromURL(redisURL)})
		_ = client.Del(context.Background(), streamKey).Err()
		_ = client.Close()
	}()
	if _, err := testEnv.pool.Exec(ctx, `delete from outbox_events where aggregate_id = $1::uuid`, aggregate); err != nil {
		t.Fatalf("clean aggregate: %v", err)
	}

	tr, err := outbox.NewRedisStreamsTransport(redisURL, streamKey, 0)
	if err != nil {
		t.Fatalf("create redis transport: %v", err)
	}
	defer func() { _ = tr.Close() }()

	// Bulk backlog: server-side insert so the injection rate itself is far
	// above the smoke floor.
	insertStart := time.Now()
	if _, err := testEnv.pool.Exec(ctx, `
		insert into outbox_events (aggregate_id, event_type, payload, aggregate_version)
		select $1::uuid, 'job.succeeded',
		       jsonb_build_object('job_id', $1::uuid::text, 'seq', g), 1
		from generate_series(1, $2) g`, aggregate, n); err != nil {
		t.Fatalf("insert backlog: %v", err)
	}
	t.Logf("NFR-302: inserted %d events in %v", n, time.Since(insertStart).Round(time.Millisecond))

	// Publisher tuned for throughput: large batches, tight polling.
	os := newScaleOutboxStore(t)
	pub := outbox.New(os, tr, outbox.Config{
		PollInterval:    10 * time.Millisecond,
		MaxIdleInterval: 50 * time.Millisecond,
		BatchSize:       500,
		Retention:       0,
		CleanupInterval: time.Hour,
	}, scaleLogger(t), nil)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		_ = pub.Run(runCtx)
		close(done)
	}()
	defer func() { cancel(); <-done }()

	// Drain the backlog and measure the achieved rate.
	drainStart := time.Now()
	waitForCondition(t, 120*time.Second, func() bool {
		var pending int
		if err := testEnv.pool.QueryRow(ctx,
			`select count(*) from outbox_events where published_at is null and aggregate_id = $1::uuid`,
			aggregate).Scan(&pending); err != nil {
			return false
		}
		return pending == 0
	})
	drainElapsed := time.Since(drainStart)
	rate := float64(n) / drainElapsed.Seconds()
	t.Logf("NFR-302: drained %d events in %v (%.0f events/sec; smoke floor %d)",
		n, drainElapsed.Round(time.Millisecond), rate, nfr302SmokeFloor)
	if rate < nfr302SmokeFloor {
		// Capacity risk, not a functional failure (PRD §6.1 gate): record it
		// in the benchmark archive instead of failing dev hardware.
		t.Logf("NFR-302 WARNING: achieved %.0f events/sec is below the %d events/sec smoke floor; "+
			"archive as capacity risk and evaluate the Kafka gate (PRD v0.3 §6.1)", rate, nfr302SmokeFloor)
	}

	// Healthy steady-state wave: backlog is zero, so these lags measure the
	// publish pipeline itself. Lag = published_at - created_at, both
	// PostgreSQL clocks.
	if _, err := testEnv.pool.Exec(ctx, `
		insert into outbox_events (aggregate_id, event_type, payload, aggregate_version)
		select $1::uuid, 'job.cancelled',
		       jsonb_build_object('job_id', $1::uuid::text, 'wave', g), 1
		from generate_series(1, $2) g`, aggregate, wave); err != nil {
		t.Fatalf("insert wave: %v", err)
	}
	waitForCondition(t, 60*time.Second, func() bool {
		var pending int
		if err := testEnv.pool.QueryRow(ctx,
			`select count(*) from outbox_events where published_at is null and aggregate_id = $1::uuid`,
			aggregate).Scan(&pending); err != nil {
			return false
		}
		return pending == 0
	})

	rows, err := testEnv.pool.Query(ctx, `
		select extract(epoch from (published_at - created_at))
		from outbox_events
		where aggregate_id = $1::uuid and event_type = 'job.cancelled' and published_at is not null`,
		aggregate)
	if err != nil {
		t.Fatalf("query wave lags: %v", err)
	}
	defer rows.Close()
	var lags []time.Duration
	for rows.Next() {
		var seconds float64
		if err := rows.Scan(&seconds); err != nil {
			t.Fatalf("scan lag: %v", err)
		}
		lags = append(lags, time.Duration(seconds*float64(time.Second)))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("lag rows: %v", err)
	}
	if len(lags) != wave {
		t.Fatalf("wave lag samples = %d, want %d", len(lags), wave)
	}
	p50, p95 := latencyPercentiles(lags)
	var maxLag time.Duration
	for _, l := range lags {
		if l > maxLag {
			maxLag = l
		}
	}
	t.Logf("NFR-302: healthy wave=%d publish lag p50=%v p95=%v max=%v", wave, p50, p95, maxLag)
	// Failure-caliber split (PRD NFR-302 wording): the healthy publish-lag
	// p95 <= 2s is a functional gate and fails the test, while a drain rate
	// below the throughput floor above is only archived as a capacity risk.
	if p95 > 2*time.Second {
		t.Fatalf("NFR-302: healthy publish lag p95 %v exceeds 2s", p95)
	}
}

// redisAddrFromURL strips the redis:// scheme and db suffix (test env only
// supports plain host:port URLs).
func redisAddrFromURL(url string) string {
	addr := url
	if len(addr) > len("redis://") && addr[:8] == "redis://" {
		addr = addr[8:]
	}
	for i := 0; i < len(addr); i++ {
		if addr[i] == '/' {
			return addr[:i]
		}
	}
	return addr
}

// newScaleOutboxStore exposes the outbox store for scale tests.
func newScaleOutboxStore(t *testing.T) store.OutboxStore {
	t.Helper()
	return postgres.NewOutboxStore(testEnv.pool)
}

// waitForCondition polls cond until true or the timeout expires.
func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

// scaleLogger returns a warn-level logger for the scale publisher.
func scaleLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}
