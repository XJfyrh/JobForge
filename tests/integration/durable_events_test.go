// Durable event transport integration tests (PRD v0.3 §10: AT-17/18/20,
// NFR-302/303). These run against a real Redis selected by
// JOBFORGE_TEST_REDIS_URL and are SKIPPED (not failed) when it is unset,
// so environments without the durable-events profile stay green.
//
// Windows bootstrap (PRD v0.3 §10.2):
//
//	docker compose -f deploy/compose.yaml --profile durable-events up -d postgres redis
//	$env:JOBFORGE_TEST_DSN = "postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable"
//	$env:JOBFORGE_TEST_REDIS_URL = "redis://localhost:6379/0"
//
// AT-17 and NFR-303 additionally restart the broker and therefore need the
// docker CLI plus the container name (JOBFORGE_TEST_REDIS_CONTAINER,
// default deploy-redis-1). The restart must preserve the named volume so
// AOF recovery is what gets tested (never `down -v`).
package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/xjfyrh/jobforge/internal/outbox"
	"github.com/xjfyrh/jobforge/internal/store"
)

// redisTestEnv bundles the Redis test prerequisites; nil when unavailable.
type redisTestEnv struct {
	url       string
	container string // empty when broker restarts are unsupported
}

// requireRedis returns the Redis test environment or skips the test with a
// bootstrap hint (skip, not fail: CI without Redis must not misreport).
func requireRedis(t *testing.T) *redisTestEnv {
	t.Helper()
	url := os.Getenv("JOBFORGE_TEST_REDIS_URL")
	if url == "" {
		t.Skip("JOBFORGE_TEST_REDIS_URL not set; durable-event tests skipped " +
			"(start the durable-events compose profile, see PRD v0.3 §10.2)")
	}
	env := &redisTestEnv{url: url, container: os.Getenv("JOBFORGE_TEST_REDIS_CONTAINER")}
	if env.container == "" {
		env.container = "deploy-redis-1"
	}
	if _, err := exec.LookPath("docker"); err != nil {
		env.container = "" // restart-based tests will skip individually
	}
	return env
}

// requireRedisRestart skips unless the broker can be stopped/started while
// preserving its data volume (AT-17/NFR-303 shape, PRD v0.3 §10.2).
func requireRedisRestart(t *testing.T, env *redisTestEnv) {
	t.Helper()
	if env.container == "" {
		t.Skip("docker CLI unavailable; broker restart test skipped")
	}
	// Safety net: whatever happens, leave the broker running so later tests
	// and local development are not broken by a mid-test failure.
	t.Cleanup(func() {
		_ = exec.Command("docker", "start", env.container).Run()
	})
}

func stopRedis(t *testing.T, container string) {
	t.Helper()
	if out, err := exec.Command("docker", "stop", container).CombinedOutput(); err != nil {
		t.Fatalf("docker stop %s: %v\n%s", container, err, out)
	}
}

func startRedis(t *testing.T, env *redisTestEnv) {
	t.Helper()
	if out, err := exec.Command("docker", "start", env.container).CombinedOutput(); err != nil {
		t.Fatalf("docker start %s: %v\n%s", env.container, err, out)
	}
	waitFor(t, 30*time.Second, func() bool {
		client := redis.NewClient(&redis.Options{Addr: redisAddrFromURL(env.url)})
		defer func() { _ = client.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return client.Ping(ctx).Err() == nil
	})
}

// redisAddrFromURL strips the redis:// scheme and any db suffix; the test
// environment only supports plain host:port URLs.
func redisAddrFromURL(url string) string {
	addr := strings.TrimPrefix(url, "redis://")
	if i := strings.Index(addr, "/"); i >= 0 {
		addr = addr[:i]
	}
	return addr
}

// newRedisClient connects a throwaway client for stream assertions.
func newRedisClient(t *testing.T, env *redisTestEnv) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: redisAddrFromURL(env.url)})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// newDurableTransport builds a Redis Streams transport on a per-test stream
// key so parallel/sequential tests never share stream state.
func newDurableTransport(t *testing.T, env *redisTestEnv, streamKey string) *outbox.RedisStreamsTransport {
	t.Helper()
	tr, err := outbox.NewRedisStreamsTransport(env.url, streamKey, 0)
	if err != nil {
		t.Fatalf("create redis transport: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	// Clean the stream before and after: leftover entries from an aborted
	// run must never contaminate entry-count assertions.
	client := newRedisClient(t, env)
	_ = client.Del(context.Background(), streamKey).Err()
	t.Cleanup(func() { _ = client.Del(context.Background(), streamKey).Err() })
	return tr
}

// streamEventIDs collects the event_id field of every stream entry in order.
func streamEventIDs(t *testing.T, client *redis.Client, streamKey string) []string {
	t.Helper()
	ctx := context.Background()
	entries, err := client.XRange(ctx, streamKey, "-", "+").Result()
	if err != nil {
		t.Fatalf("xrange %s: %v", streamKey, err)
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		if v, ok := e.Values["event_id"].(string); ok {
			ids = append(ids, v)
		}
	}
	return ids
}

// insertDurableOutboxEvent writes one outbox row directly (with envelope
// capture columns) for publisher-driven tests that do not need a job.
func insertDurableOutboxEvent(t *testing.T, aggregate, eventType string) int64 {
	t.Helper()
	var id int64
	err := testEnv.pool.QueryRow(context.Background(),
		`insert into outbox_events (aggregate_id, event_type, payload, aggregate_version)
		 values ($1::uuid, $2, jsonb_build_object('job_id', $1::uuid::text, 'event_type', $2::text), 1)
		 returning event_id`, aggregate, eventType).Scan(&id)
	if err != nil {
		t.Fatalf("insert outbox event: %v", err)
	}
	return id
}

// crashWindowStore decorates an OutboxStore so the first N MarkPublishedBatch
// calls fail AFTER the transport already acknowledged the events. This is the
// AT-18 crash window (XADD success -> publisher dies before marking); the
// same events are republished on retry, producing duplicate stream entries.
type crashWindowStore struct {
	store.OutboxStore
	mu         sync.Mutex
	failMarks  int
	failCounts map[int64]int
}

func newCrashWindowStore(inner store.OutboxStore) *crashWindowStore {
	return &crashWindowStore{OutboxStore: inner, failCounts: make(map[int64]int)}
}

func (s *crashWindowStore) armFailMarks(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failMarks = n
}

func (s *crashWindowStore) MarkPublishedBatch(ctx context.Context, eventIDs []int64) (int64, error) {
	s.mu.Lock()
	if s.failMarks > 0 {
		s.failMarks--
		for _, id := range eventIDs {
			s.failCounts[id]++
		}
		s.mu.Unlock()
		return 0, fmt.Errorf("injected crash: publisher died after broker ACK (batch of %d)", len(eventIDs))
	}
	s.mu.Unlock()
	return s.OutboxStore.MarkPublishedBatch(ctx, eventIDs)
}

// runPublisherUntil drains the outbox through transport until pred holds or
// the timeout expires; the publisher is stopped before returning.
func runPublisherUntil(t *testing.T, os store.OutboxStore, tr outbox.Transport, timeout time.Duration, pred func() bool) {
	t.Helper()
	pub := outbox.New(os, tr, fastPublisherConfig(), testLogger(t), nil)
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = pub.Run(runCtx)
		close(done)
	}()
	defer func() { cancel(); <-done }()
	waitFor(t, timeout, pred)
}

// startBackgroundPublisher runs a publisher for the whole test lifetime:
// used by tests whose assertions span broker outage windows (the publisher
// must be live during the outage).
func startBackgroundPublisher(t *testing.T, os store.OutboxStore, tr outbox.Transport) {
	t.Helper()
	pub := outbox.New(os, tr, fastPublisherConfig(), testLogger(t), nil)
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = pub.Run(runCtx)
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
}

// TestDurableAT17RestartRecovery verifies AT-17 / NFR-303 restart shape:
// entries written before a broker stop survive the restart (AOF + named
// volume), events generated during the outage stay unpublished, and after
// recovery the backlog drains into the stream with zero silent loss.
func TestDurableAT17RestartRecovery(t *testing.T) {
	env := requireRedis(t)
	requireRedisRestart(t, env)
	truncateOutboxEvents(t)
	os := setupOutboxStore(t)
	ctx := context.Background()
	client := newRedisClient(t, env)
	streamKey := "test:at17:" + uuid.NewString()[:8]
	tr := newDurableTransport(t, env, streamKey)

	// One publisher spans all phases so the outage assertions observe a live
	// publisher attempting (and failing) delivery.
	startBackgroundPublisher(t, os, tr)

	// Phase 1: publish events with the broker up; they must land in the stream.
	aggregate := uuid.New().String()
	preIDs := []int64{
		insertDurableOutboxEvent(t, aggregate, "job.succeeded"),
		insertDurableOutboxEvent(t, aggregate, "job.succeeded"),
	}
	waitFor(t, 15*time.Second, func() bool {
		return len(streamEventIDs(t, client, streamKey)) >= len(preIDs)
	})

	// Phase 2: stop the broker, generate terminal events during the outage.
	stopRedis(t, env.container)
	outageID := insertDurableOutboxEvent(t, aggregate, "job.cancelled")

	// Give the live publisher time to attempt (and fail) delivery: the event
	// must remain unpublished — no silent loss and no fake success (FR-702
	// forbids marking success without a broker ACK).
	waitFor(t, 5*time.Second, func() bool {
		_, attempts := getOutboxRow(t, outageID)
		return attempts >= 1
	})
	if publishedAt, _ := getOutboxRow(t, outageID); publishedAt != nil {
		t.Fatalf("event %d marked published while broker is down", outageID)
	}

	// Phase 3: restart (preserving the volume) and verify recovery.
	startRedis(t, env)

	waitFor(t, 30*time.Second, func() bool {
		publishedAt, _ := getOutboxRow(t, outageID)
		return publishedAt != nil
	})

	ids := streamEventIDs(t, client, streamKey)
	want := map[string]bool{
		fmt.Sprint(preIDs[0]): false,
		fmt.Sprint(preIDs[1]): false,
		fmt.Sprint(outageID):  false,
	}
	for _, id := range ids {
		if _, ok := want[id]; ok {
			want[id] = true
		}
	}
	for id, seen := range want {
		if !seen {
			t.Fatalf("event_id %s missing from stream after recovery; got %v", id, ids)
		}
	}

	// Backlog drained: nothing unpublished remains for this aggregate.
	var pending int
	if err := testEnv.pool.QueryRow(ctx,
		`select count(*) from outbox_events where published_at is null and aggregate_id = $1`,
		aggregate).Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 0 {
		t.Fatalf("backlog not zero after recovery: %d unpublished", pending)
	}
	t.Logf("AT-17: pre-stop entries survived restart; outage event %d delivered after recovery; stream entries=%d",
		outageID, len(ids))
}

// TestDurableAT18AckCrashWindow verifies AT-18: a publisher crash between
// XADD success and MarkPublished republishes the same event_id (multiple
// stream entries allowed), the outbox is eventually marked, and job state is
// untouched (this test does not depend on the consumer inbox).
func TestDurableAT18AckCrashWindow(t *testing.T) {
	env := requireRedis(t)
	truncateOutboxEvents(t)
	os := newCrashWindowStore(setupOutboxStore(t))
	client := newRedisClient(t, env)
	streamKey := "test:at18:" + uuid.NewString()[:8]
	tr := newDurableTransport(t, env, streamKey)

	aggregate := uuid.New().String()
	eventID := insertDurableOutboxEvent(t, aggregate, "job.succeeded")

	// First publish attempt: XADD succeeds, then the injected crash fails the
	// mark. The claim is released and the retry round republishes.
	os.armFailMarks(1)
	runPublisherUntil(t, os, tr, 15*time.Second, func() bool {
		publishedAt, _ := getOutboxRow(t, eventID)
		return publishedAt != nil
	})

	// Same event_id must now appear at least twice (duplicate entries are the
	// documented at-least-once outcome, not a defect).
	ids := streamEventIDs(t, client, streamKey)
	count := 0
	for _, id := range ids {
		if id == fmt.Sprint(eventID) {
			count++
		}
	}
	if count < 2 {
		t.Fatalf("expected >= 2 stream entries for event %d after ack-crash republish, got %d (%v)",
			eventID, count, ids)
	}
	// The outbox row is eventually marked published (the predicate above
	// already guaranteed it); job state was never touched because publishing
	// only reads outbox rows. Note: publish_attempts stays 0 on this path by
	// design — the broker ACK succeeded, only the mark crashed.
	publishedAt, _ := getOutboxRow(t, eventID)
	if publishedAt == nil {
		t.Fatalf("event %d not marked published after recovery", eventID)
	}
	t.Logf("AT-18: event %d delivered %d entries (duplicate allowed), finally marked published", eventID, count)
}

// TestDurableAT20GroupIsolation verifies AT-20: two consumer groups consume
// the same stream independently and completely; within each group the two
// consumer instances share the load; there is no cross-group cursor
// interference.
func TestDurableAT20GroupIsolation(t *testing.T) {
	env := requireRedis(t)
	truncateOutboxEvents(t)
	os := setupOutboxStore(t)
	client := newRedisClient(t, env)
	streamKey := "test:at20:" + uuid.NewString()[:8]
	tr := newDurableTransport(t, env, streamKey)

	// Groups must exist before the entries are read; ">" starts them at the
	// stream's current tail, so create them first, then publish.
	const total = 40
	groups := []string{"grp-a", "grp-b"}
	readers := make([]*outbox.GroupReader, 0, 4)
	for _, g := range groups {
		for i := 0; i < 2; i++ {
			r, err := outbox.NewGroupReader(env.url, streamKey, g, fmt.Sprintf("inst-%d", i))
			if err != nil {
				t.Fatalf("create group reader %s/%d: %v", g, i, err)
			}
			t.Cleanup(func() { _ = r.Close() })
			if err := r.EnsureGroup(context.Background()); err != nil {
				t.Fatalf("ensure group %s: %v", g, err)
			}
			readers = append(readers, r)
		}
	}

	aggregate := uuid.New().String()
	wantIDs := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		wantIDs[fmt.Sprint(insertDurableOutboxEvent(t, aggregate, "job.succeeded"))] = false
	}
	runPublisherUntil(t, os, tr, 30*time.Second, func() bool {
		return len(streamEventIDs(t, client, streamKey)) >= total
	})

	// Consume with all four readers until every group has the full set.
	type groupState struct {
		mu    sync.Mutex
		seen  map[string]int // event_id -> deliveries
		count map[string]int // consumer instance -> distinct events
	}
	states := map[string]*groupState{
		"grp-a": {seen: make(map[string]int), count: make(map[string]int)},
		"grp-b": {seen: make(map[string]int), count: make(map[string]int)},
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		allDone := true
		for _, st := range states {
			st.mu.Lock()
			if len(st.seen) < total {
				allDone = false
			}
			st.mu.Unlock()
		}
		if allDone {
			break
		}
		for i, r := range readers {
			group := groups[i/2]
			inst := fmt.Sprintf("inst-%d", i%2)
			envs, entryIDs, err := r.ReadNext(context.Background(), 20, 200*time.Millisecond)
			if err != nil {
				t.Fatalf("read group %s: %v", group, err)
			}
			if len(envs) == 0 {
				continue
			}
			st := states[group]
			st.mu.Lock()
			for _, e := range envs {
				if st.seen[e.EventID] == 0 {
					st.count[inst]++
				}
				st.seen[e.EventID]++
			}
			st.mu.Unlock()
			if err := r.Ack(context.Background(), entryIDs...); err != nil {
				t.Fatalf("ack group %s: %v", group, err)
			}
		}
	}

	for _, g := range groups {
		st := states[g]
		st.mu.Lock()
		if len(st.seen) != total {
			st.mu.Unlock()
			t.Fatalf("group %s consumed %d/%d distinct events (cursor interference?)", g, len(st.seen), total)
		}
		for id, n := range st.seen {
			if n != 1 {
				st.mu.Unlock()
				t.Fatalf("group %s: event %s delivered %d times within the group", g, id, n)
			}
		}
		for i := 0; i < 2; i++ {
			inst := fmt.Sprintf("inst-%d", i)
			if st.count[inst] == 0 {
				st.mu.Unlock()
				t.Fatalf("group %s: consumer %s received no events (no in-group distribution)", g, inst)
			}
		}
		st.mu.Unlock()
		t.Logf("AT-20: group %s complete (%d events), distribution %v", g, total, st.count)
	}
}

// TestDurableNFR303PauseRecovery verifies NFR-303: while the broker is
// paused, job state transactions are NOT blocked by the publisher, events
// accumulate unpublished, and after recovery the backlog drains to zero
// without silent loss. The pause follows the PRD value (60s) unless
// JOBFORGE_NFR303_PAUSE overrides it.
func TestDurableNFR303PauseRecovery(t *testing.T) {
	env := requireRedis(t)
	requireRedisRestart(t, env)
	truncateOutboxEvents(t)
	obs := setupOutboxStore(t)
	ctx := context.Background()
	client := newRedisClient(t, env)
	streamKey := "test:nfr303:" + uuid.NewString()[:8]
	tr := newDurableTransport(t, env, streamKey)

	pause := 60 * time.Second
	if v := os.Getenv("JOBFORGE_NFR303_PAUSE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			t.Fatalf("invalid JOBFORGE_NFR303_PAUSE %q", v)
		}
		pause = d
	}

	// Publisher runs for the whole test; it must back off while paused and
	// drain after recovery.
	pub := outbox.New(obs, tr, fastPublisherConfig(), testLogger(t), nil)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		_ = pub.Run(runCtx)
		close(done)
	}()
	defer func() { cancel(); <-done }()

	aggregate := uuid.New().String()

	stopRedis(t, env.container)
	t.Logf("NFR-303: broker paused for %s", pause)

	// Job state transactions must not be blocked by the paused broker:
	// enqueue+claim+complete a real job and require it to finish fast.
	jobStart := time.Now()
	js := setupStore(t)
	job := createTestJob(t, js, "nfr303", "demo.echo")
	if _, err := testEnv.pool.Exec(ctx,
		`update jobs set run_at = now() - interval '10 seconds' where id = $1`, job.ID); err != nil {
		t.Fatalf("re-anchor run_at: %v", err)
	}
	claimed, err := claimJobs(ctx, js, store.ClaimParams{
		Queues:   []string{"nfr303"},
		WorkerID: "nfr303-worker",
		MaxJobs:  1,
		LeaseTTL: 5 * time.Minute,
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim during broker pause: %v (claimed %d)", err, len(claimed))
	}
	if err := js.Complete(ctx, job.ID, "nfr303-worker", claimed[0].FencingToken, "ok", 1); err != nil {
		t.Fatalf("complete during broker pause: %v", err)
	}
	if elapsed := time.Since(jobStart); elapsed > 5*time.Second {
		t.Fatalf("job state transaction blocked by paused broker: took %s", elapsed)
	}

	// Additional events accumulate while paused.
	outageIDs := make([]int64, 0, 5)
	for i := 0; i < 5; i++ {
		outageIDs = append(outageIDs, insertDurableOutboxEvent(t, aggregate, "job.succeeded"))
	}

	time.Sleep(pause)
	startRedis(t, env)

	// Everything drains: the job's succeeded event plus all outage events.
	waitFor(t, 60*time.Second, func() bool {
		var pending int
		if err := testEnv.pool.QueryRow(ctx,
			`select count(*) from outbox_events where published_at is null`).Scan(&pending); err != nil {
			return false
		}
		return pending == 0
	})

	ids := streamEventIDs(t, client, streamKey)
	got := make(map[string]int, len(ids))
	for _, id := range ids {
		got[id]++
	}
	for _, oid := range outageIDs {
		if got[fmt.Sprint(oid)] == 0 {
			t.Fatalf("event %d silently lost after pause recovery", oid)
		}
	}
	t.Logf("NFR-303: pause=%s, job tx completed in <%s during pause, backlog drained, stream entries=%d",
		pause, "5s", len(ids))
}
