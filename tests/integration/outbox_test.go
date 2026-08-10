package integration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/xjfyrh/jobforge/internal/outbox"
	"github.com/xjfyrh/jobforge/internal/store"
	"github.com/xjfyrh/jobforge/internal/store/postgres"
)

// These tests verify PRD v0.2 AT-15 (outbox duplicate delivery / publisher
// crash) plus FR-610~613 semantics: at-least-once publication, progress
// tracking, crash recovery from table state, and retention boundaries.

// setupOutboxStore creates an OutboxStore bound to the shared test database.
func setupOutboxStore(t *testing.T) *postgres.OutboxStore {
	t.Helper()
	return postgres.NewOutboxStore(testEnv.pool)
}

// truncateOutboxEvents removes all outbox rows so publisher-loop tests are
// isolated from leftovers of earlier tests or previous runs: with batch
// claims ordered by created_at, stale unpublished rows would starve or
// inflate the delivery assertions of the events this test inserts. Outbox
// rows are side-effect hints only (consumers re-query), never job state.
func truncateOutboxEvents(t *testing.T) {
	t.Helper()
	if _, err := testEnv.pool.Exec(context.Background(), `truncate outbox_events`); err != nil {
		t.Fatalf("truncate outbox_events: %v", err)
	}
}

// insertOutboxEvent inserts an outbox event directly (as job state
// transactions do) and returns its event_id.
func insertOutboxEvent(t *testing.T, aggregateID, eventType string) int64 {
	t.Helper()
	var eventID int64
	err := testEnv.pool.QueryRow(context.Background(),
		`insert into outbox_events (aggregate_id, event_type, payload)
		 values ($1, $2, '{}') returning event_id`,
		aggregateID, eventType).Scan(&eventID)
	if err != nil {
		t.Fatalf("insert outbox event: %v", err)
	}
	return eventID
}

// getOutboxRow fetches publication state for one event.
func getOutboxRow(t *testing.T, eventID int64) (publishedAt *time.Time, attempts int) {
	t.Helper()
	err := testEnv.pool.QueryRow(context.Background(),
		`select published_at, publish_attempts from outbox_events where event_id = $1`,
		eventID).Scan(&publishedAt, &attempts)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("outbox event %d missing", eventID)
		}
		t.Fatalf("get outbox row: %v", err)
	}
	return publishedAt, attempts
}

// waitFor polls cond until it returns true or the deadline expires.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

// recordingChannel records published events into a consumer-style dedup set
// keyed by event_id, proving that duplicate deliveries collapse to one
// business effect per event.
type recordingChannel struct {
	mu        sync.Mutex
	delivered []int64        // raw delivery log (may contain duplicates)
	processed map[int64]bool // deduplicated business effects per event_id
	failIDs   map[int64]int  // event_id -> remaining forced failures

	// delegate optionally forwards deliveries to a real channel (e.g.
	// outbox.NotifyChannel) so tests can observe NOTIFY side effects.
	delegate outbox.Channel
}

func newRecordingChannel() *recordingChannel {
	return &recordingChannel{processed: make(map[int64]bool), failIDs: make(map[int64]int)}
}

// forceFailures makes Publish fail n times for the given event IDs.
func (c *recordingChannel) forceFailures(ids []int64, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		c.failIDs[id] = n
	}
}

// Publish implements outbox.Channel.
func (c *recordingChannel) Publish(ctx context.Context, ev *store.OutboxEvent) error {
	c.mu.Lock()
	if n, ok := c.failIDs[ev.EventID]; ok && n > 0 {
		c.failIDs[ev.EventID] = n - 1
		c.mu.Unlock()
		return fmt.Errorf("injected publish failure for event %d", ev.EventID)
	}
	c.delivered = append(c.delivered, ev.EventID)
	// Consumer-side idempotency: the business effect happens once per
	// event_id, regardless of duplicate deliveries (at-least-once).
	c.processed[ev.EventID] = true
	delegate := c.delegate
	c.mu.Unlock()
	if delegate != nil {
		return delegate.Publish(ctx, ev)
	}
	return nil
}

// effectCount returns the deduplicated business effect count for an event
// (0 or 1).
func (c *recordingChannel) effectCount(id int64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.processed[id] {
		return 1
	}
	return 0
}

// deliveryCount returns total raw deliveries for an event.
func (c *recordingChannel) deliveryCount(id int64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, d := range c.delivered {
		if d == id {
			n++
		}
	}
	return n
}

// testLogger returns a discard-ish logger for publisher components.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// fastPublisherConfig returns a publisher config tuned for tests.
func fastPublisherConfig() outbox.Config {
	return outbox.Config{
		PollInterval:    20 * time.Millisecond,
		MaxIdleInterval: 100 * time.Millisecond,
		BatchSize:       50,
		Retention:       0, // cleaner disabled unless the test enables it
		CleanupInterval: time.Hour,
	}
}

// TestOutboxAT15NormalPublish verifies FR-610/612: unpublished events are
// published via NOTIFY, published_at is set, and the NOTIFY payload carries
// the event_id so consumers can re-query the full event.
func TestOutboxAT15NormalPublish(t *testing.T) {
	truncateOutboxEvents(t)
	os := setupOutboxStore(t)
	ctx := context.Background()

	// Listen on the outbox channel before publishing so no notification is missed.
	listenConn, err := pgx.Connect(ctx, testEnv.dsn)
	if err != nil {
		t.Fatalf("connect listener: %v", err)
	}
	defer func() { _ = listenConn.Close(context.Background()) }()
	// outbox.DefaultChannel is a package-level constant, not user input.
	// LISTEN does not support parameterized queries; quote the identifier
	// for safety (same pattern as internal/notify).
	if _, err := listenConn.Exec(ctx, `listen "`+outbox.DefaultChannel+`"`); err != nil {
		t.Fatalf("listen: %v", err)
	}

	aggregate := uuid.New().String()
	ids := []int64{
		insertOutboxEvent(t, aggregate, "job.succeeded"),
		insertOutboxEvent(t, aggregate, "job.cancelled"),
	}

	// Deliver through the real NOTIFY channel so the listener below can
	// observe the hint, while recording deliveries for dedup assertions.
	channel := newRecordingChannel()
	channel.delegate = outbox.NewNotifyChannel(testEnv.pool, outbox.DefaultChannel, testLogger(t))
	pub := outbox.New(os, channel, fastPublisherConfig(), testLogger(t), nil)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		_ = pub.Run(runCtx)
		close(done)
	}()
	// Safety net: if an assertion below fails, the publisher must still be
	// stopped so it cannot leak into later tests and publish their events.
	defer func() { cancel(); <-done }()

	// Wait until both events are marked published.
	waitFor(t, 10*time.Second, func() bool {
		for _, id := range ids {
			publishedAt, _ := getOutboxRow(t, id)
			if publishedAt == nil {
				return false
			}
		}
		return true
	})
	cancel()
	<-done

	// Consumer-side dedup effect: exactly one business effect per event.
	for _, id := range ids {
		if got := channel.effectCount(id); got != 1 {
			t.Fatalf("event %d: expected 1 effect, got %d", id, got)
		}
	}

	// NOTIFY must carry the event_id (hint semantics, ADR-0003).
	notifyCtx, notifyCancel := context.WithTimeout(ctx, 5*time.Second)
	defer notifyCancel()
	notification, err := listenConn.WaitForNotification(notifyCtx)
	if err != nil {
		t.Fatalf("wait for notification: %v", err)
	}
	if notification.Channel != outbox.DefaultChannel {
		t.Fatalf("unexpected channel: %s", notification.Channel)
	}
	if _, err := strconv.ParseInt(notification.Payload, 10, 64); err != nil {
		t.Fatalf("NOTIFY payload is not an event_id: %q", notification.Payload)
	}
}

// TestOutboxAT15PublishFailureRetry verifies FR-610: publish failures
// increment publish_attempts, keep the event unpublished, and are retried
// with backoff until success. Job state must never be affected.
func TestOutboxAT15PublishFailureRetry(t *testing.T) {
	truncateOutboxEvents(t)
	os := setupOutboxStore(t)
	ctx := context.Background()

	aggregate := uuid.New().String()
	id := insertOutboxEvent(t, aggregate, "job.dead")

	channel := newRecordingChannel()
	channel.forceFailures([]int64{id}, 2) // fail the first two attempts

	pub := outbox.New(os, channel, fastPublisherConfig(), testLogger(t), nil)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		_ = pub.Run(runCtx)
		close(done)
	}()
	defer func() { cancel(); <-done }()

	// Wait until the event is eventually published after forced failures.
	waitFor(t, 10*time.Second, func() bool {
		publishedAt, _ := getOutboxRow(t, id)
		return publishedAt != nil
	})
	cancel()
	<-done

	// Attempts must record the two injected failures plus the success path.
	_, attempts := getOutboxRow(t, id)
	if attempts < 2 {
		t.Fatalf("expected publish_attempts >= 2, got %d", attempts)
	}
	// Deliveries include the failed attempts' channel calls were rejected, so
	// successful deliveries must be exactly one; effect count stays one.
	if got := channel.deliveryCount(id); got != 1 {
		t.Fatalf("expected 1 successful delivery, got %d", got)
	}
	if got := channel.effectCount(id); got != 1 {
		t.Fatalf("expected dedup effect 1, got %d", got)
	}
}

// TestOutboxAT15CrashRecovery verifies the crash-recovery contract: the
// publisher holds no in-memory progress; after a crash (context cancel) a new
// publisher resumes from the table and no event is silently lost. Duplicate
// deliveries are tolerated and collapse under event_id dedup.
func TestOutboxAT15CrashRecovery(t *testing.T) {
	truncateOutboxEvents(t)
	os := setupOutboxStore(t)
	ctx := context.Background()

	aggregate := uuid.New().String()
	ids := []int64{
		insertOutboxEvent(t, aggregate, "job.succeeded"),
		insertOutboxEvent(t, aggregate, "job.succeeded"),
		insertOutboxEvent(t, aggregate, "job.succeeded"),
	}

	channel := newRecordingChannel()
	cfg := fastPublisherConfig()

	// First run: crash mid-run by cancelling on the second delivery. Event 1
	// completes publish+mark; event 2 is delivered but its mark races with
	// the crash; event 3 is untouched. This simulates losing progress in
	// memory on crash while the table holds the truth.
	crashCtx, crashCancel := context.WithCancel(ctx)
	crashChannel := &cancelOnDelivery{inner: channel, cancel: crashCancel, after: 2}
	pub1 := outbox.New(os, crashChannel, cfg, testLogger(t), nil)
	done1 := make(chan struct{})
	go func() {
		_ = pub1.Run(crashCtx)
		close(done1)
	}()
	<-done1 // publisher crashed (stopped)

	// At least one event delivered before the crash; some may remain unpublished.
	var publishedBefore int
	for _, id := range ids {
		if publishedAt, _ := getOutboxRow(t, id); publishedAt != nil {
			publishedBefore++
		}
	}
	if publishedBefore == 0 {
		t.Fatalf("expected at least one event published before crash")
	}

	// Second run: a fresh publisher resumes purely from table state.
	pub2 := outbox.New(os, channel, cfg, testLogger(t), nil)
	runCtx, cancel := context.WithCancel(ctx)
	done2 := make(chan struct{})
	go func() {
		_ = pub2.Run(runCtx)
		close(done2)
	}()
	defer func() { cancel(); <-done2 }()

	waitFor(t, 10*time.Second, func() bool {
		for _, id := range ids {
			if publishedAt, _ := getOutboxRow(t, id); publishedAt == nil {
				return false
			}
		}
		return true
	})
	cancel()
	<-done2

	// Zero silent loss: every event eventually published; consumer dedup
	// yields exactly one effect per event even if delivery duplicated.
	for _, id := range ids {
		if got := channel.effectCount(id); got != 1 {
			t.Fatalf("event %d: expected dedup effect 1, got %d", id, got)
		}
	}
}

// cancelOnDelivery cancels the run context after n successful deliveries,
// simulating a publisher crash mid-run.
type cancelOnDelivery struct {
	inner  *recordingChannel
	cancel context.CancelFunc
	after  int

	mu        sync.Mutex
	delivered int
}

// Publish implements outbox.Channel.
func (c *cancelOnDelivery) Publish(ctx context.Context, ev *store.OutboxEvent) error {
	if err := c.inner.Publish(ctx, ev); err != nil {
		return err
	}
	c.mu.Lock()
	c.delivered++
	shouldCrash := c.delivered == c.after
	c.mu.Unlock()
	if shouldCrash {
		c.cancel()
	}
	return nil
}

// TestOutboxAT15DuplicateDeliveryIdempotent verifies that re-publishing an
// already-published event (simulating a crash between NOTIFY and marking
// published) does not change consumer effects and never touches job state.
func TestOutboxAT15DuplicateDeliveryIdempotent(t *testing.T) {
	truncateOutboxEvents(t)
	os := setupOutboxStore(t)
	ctx := context.Background()

	// A real job in terminal state: publisher failures must not alter it.
	js := setupStore(t)
	job := createTestJob(t, js, "outbox-at15", "demo.echo")
	// Re-anchor run_at to the PostgreSQL clock to avoid Docker/WSL2 host
	// clock drift making the job unclaimable (run_at <= now()).
	if _, err := testEnv.pool.Exec(ctx,
		`update jobs set run_at = now() - interval '10 seconds' where id = $1`, job.ID); err != nil {
		t.Fatalf("re-anchor run_at: %v", err)
	}
	claimed, err := claimJobs(ctx, js, store.ClaimParams{
		Queues:   []string{"outbox-at15"},
		WorkerID: "outbox-at15-worker",
		MaxJobs:  1,
		LeaseTTL: time.Minute,
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim: %v", err)
	}
	if err := js.Complete(ctx, job.ID, "outbox-at15-worker", claimed[0].FencingToken, "ok", 1); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// The job's terminal transition wrote a job.succeeded outbox event.
	channel := newRecordingChannel()
	pub := outbox.New(os, channel, fastPublisherConfig(), testLogger(t), nil)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		_ = pub.Run(runCtx)
		close(done)
	}()
	defer func() { cancel(); <-done }()

	// Find the event for this job and wait for publication.
	var eventID int64
	waitFor(t, 10*time.Second, func() bool {
		err := testEnv.pool.QueryRow(ctx,
			`select event_id from outbox_events where aggregate_id = $1 and event_type = 'job.succeeded'`,
			job.ID).Scan(&eventID)
		if err != nil || eventID == 0 {
			return false
		}
		publishedAt, _ := getOutboxRow(t, eventID)
		return publishedAt != nil
	})

	// Simulate crash between NOTIFY and marking: reset the publication
	// state. A real crash also leaves claimed_at stamped; such claims become
	// reclaimable only after the claim TTL (covered by
	// TestOutboxStaleClaimReclaimed), so reset both columns to jump
	// directly to the reclaimable state and let the publisher re-publish
	// (duplicate delivery).
	if _, err := testEnv.pool.Exec(ctx,
		`update outbox_events set published_at = null, claimed_at = null where event_id = $1`, eventID); err != nil {
		t.Fatalf("reset published_at: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool {
		publishedAt, _ := getOutboxRow(t, eventID)
		return publishedAt != nil
	})
	cancel()
	<-done

	// At-least-once: delivery happened twice; consumer dedup effect is one.
	if got := channel.deliveryCount(eventID); got < 2 {
		t.Fatalf("expected >= 2 deliveries (duplicate), got %d", got)
	}
	if got := channel.effectCount(eventID); got != 1 {
		t.Fatalf("expected dedup effect 1, got %d", got)
	}

	// Job state untouched by publish retries.
	got, err := js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != "succeeded" {
		t.Fatalf("expected job state succeeded, got %s", got.State)
	}
}

// TestOutboxRetentionBoundary verifies FR-613: cleanup removes only published
// events older than the retention period; unpublished events are never
// removed regardless of age.
func TestOutboxRetentionBoundary(t *testing.T) {
	truncateOutboxEvents(t)
	os := setupOutboxStore(t)
	ctx := context.Background()

	// Old published event (8 days): must be deleted with 7-day retention.
	oldID := insertOutboxEvent(t, uuid.New().String(), "job.succeeded")
	if _, err := testEnv.pool.Exec(ctx,
		`update outbox_events set published_at = now() - interval '8 days' where event_id = $1`, oldID); err != nil {
		t.Fatalf("backdate published_at: %v", err)
	}

	// Old UNPUBLISHED event (8 days): must survive cleanup.
	unpubID := insertOutboxEvent(t, uuid.New().String(), "job.dead")
	if _, err := testEnv.pool.Exec(ctx,
		`update outbox_events set created_at = now() - interval '8 days' where event_id = $1`, unpubID); err != nil {
		t.Fatalf("backdate created_at: %v", err)
	}

	// Fresh published event: must survive cleanup.
	freshID := insertOutboxEvent(t, uuid.New().String(), "job.cancelled")
	if _, err := testEnv.pool.Exec(ctx,
		`update outbox_events set published_at = now() where event_id = $1`, freshID); err != nil {
		t.Fatalf("mark fresh published: %v", err)
	}

	deleted, err := os.CleanupPublished(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if deleted < 1 {
		t.Fatalf("expected at least 1 deleted row, got %d", deleted)
	}

	// Old published event must be gone.
	var n int
	if err := testEnv.pool.QueryRow(ctx,
		`select count(*) from outbox_events where event_id = $1`, oldID).Scan(&n); err != nil {
		t.Fatalf("count old: %v", err)
	}
	if n != 0 {
		t.Fatalf("old published event was not deleted")
	}

	// Unpublished (even old) and fresh published events must remain.
	for _, id := range []int64{unpubID, freshID} {
		if err := testEnv.pool.QueryRow(ctx,
			`select count(*) from outbox_events where event_id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("count %d: %v", id, err)
		}
		if n != 1 {
			t.Fatalf("event %d was wrongly deleted", id)
		}
	}
}

// TestOutboxCountPending verifies FR-612 progress observability at the store
// level: the pending count reflects unpublished events only.
func TestOutboxCountPending(t *testing.T) {
	truncateOutboxEvents(t)
	os := setupOutboxStore(t)
	ctx := context.Background()

	id := insertOutboxEvent(t, uuid.New().String(), "job.succeeded")
	before, err := os.CountPending(ctx)
	if err != nil {
		t.Fatalf("count pending: %v", err)
	}

	if _, err := testEnv.pool.Exec(ctx,
		`update outbox_events set published_at = now() where event_id = $1`, id); err != nil {
		t.Fatalf("mark published: %v", err)
	}

	after, err := os.CountPending(ctx)
	if err != nil {
		t.Fatalf("count pending after: %v", err)
	}
	if after != before-1 {
		t.Fatalf("expected pending to drop by 1 (%d -> %d)", before, after)
	}
}

// TestOutboxConcurrentPublishersNoDuplicateNotify stress-tests the atomic
// claim: two publishers run concurrently against the same backlog, and every
// event must be delivered exactly once across both channels (zero duplicate
// NOTIFY). Before the atomic claim, FETCH released its SKIP LOCKED rows at
// statement end, letting both publishers pick up the same events.
func TestOutboxConcurrentPublishersNoDuplicateNotify(t *testing.T) {
	truncateOutboxEvents(t)
	os := setupOutboxStore(t)
	ctx := context.Background()

	const n = 100
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, insertOutboxEvent(t, uuid.New().String(), "job.succeeded"))
	}

	chA := newRecordingChannel()
	chB := newRecordingChannel()
	cfg := fastPublisherConfig()
	pubA := outbox.New(os, chA, cfg, testLogger(t), nil)
	pubB := outbox.New(os, chB, cfg, testLogger(t), nil)

	runCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, pub := range []*outbox.Publisher{pubA, pubB} {
		wg.Add(1)
		go func(p *outbox.Publisher) {
			defer wg.Done()
			_ = p.Run(runCtx)
		}(pub)
	}
	// Safety net: stop both publishers even if an assertion below fails.
	defer func() { cancel(); wg.Wait() }()

	waitFor(t, 30*time.Second, func() bool {
		for _, id := range ids {
			if publishedAt, _ := getOutboxRow(t, id); publishedAt == nil {
				return false
			}
		}
		return true
	})
	cancel()
	wg.Wait()

	// Zero duplicate NOTIFY: each test event was delivered exactly once
	// across both publishers combined.
	for _, id := range ids {
		got := chA.deliveryCount(id) + chB.deliveryCount(id)
		if got != 1 {
			t.Fatalf("event %d: expected exactly 1 delivery across both publishers, got %d", id, got)
		}
		if chA.effectCount(id)+chB.effectCount(id) != 1 {
			t.Fatalf("event %d: expected dedup effect 1", id)
		}
	}
}

// TestOutboxStaleClaimReclaimed verifies crashed-publisher recovery: an event
// claimed but never published (claimed_at older than the claim TTL) is
// reclaimable, both at the store level and end-to-end through a publisher.
func TestOutboxStaleClaimReclaimed(t *testing.T) {
	truncateOutboxEvents(t)
	os := setupOutboxStore(t)
	ctx := context.Background()

	// Store level: a claim older than the TTL must be reclaimable.
	staleID := insertOutboxEvent(t, uuid.New().String(), "job.succeeded")
	if _, err := testEnv.pool.Exec(ctx,
		`update outbox_events set claimed_at = now() - interval '6 minutes' where event_id = $1`, staleID); err != nil {
		t.Fatalf("backdate claimed_at: %v", err)
	}
	events, err := os.FetchUnpublished(ctx, 200)
	if err != nil {
		t.Fatalf("fetch unpublished: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.EventID == staleID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("stale-claimed event %d was not reclaimed by FetchUnpublished", staleID)
	}
	// Mark it published so it does not interfere with the publisher below.
	if _, err := os.MarkPublished(ctx, staleID); err != nil {
		t.Fatalf("mark stale event published: %v", err)
	}

	// A fresh claim (within the TTL) must NOT be reclaimable yet.
	freshID := insertOutboxEvent(t, uuid.New().String(), "job.succeeded")
	if _, err := testEnv.pool.Exec(ctx,
		`update outbox_events set claimed_at = now() where event_id = $1`, freshID); err != nil {
		t.Fatalf("set fresh claimed_at: %v", err)
	}
	events, err = os.FetchUnpublished(ctx, 200)
	if err != nil {
		t.Fatalf("fetch unpublished: %v", err)
	}
	for _, ev := range events {
		if ev.EventID == freshID {
			t.Fatalf("freshly claimed event %d must not be reclaimable within the TTL", freshID)
		}
	}
	// Release the fresh claim, then verify end-to-end publication.
	if _, err := testEnv.pool.Exec(ctx,
		`update outbox_events set claimed_at = now() - interval '6 minutes' where event_id = $1`, freshID); err != nil {
		t.Fatalf("expire fresh claim: %v", err)
	}

	channel := newRecordingChannel()
	pub := outbox.New(os, channel, fastPublisherConfig(), testLogger(t), nil)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		_ = pub.Run(runCtx)
		close(done)
	}()
	defer func() { cancel(); <-done }()
	waitFor(t, 10*time.Second, func() bool {
		publishedAt, _ := getOutboxRow(t, freshID)
		return publishedAt != nil
	})
	cancel()
	<-done

	if got := channel.deliveryCount(freshID); got != 1 {
		t.Fatalf("expected 1 delivery of the reclaimed event, got %d", got)
	}
}
