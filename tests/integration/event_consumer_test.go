package integration

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/xjfyrh/jobforge/internal/eventconsumer"
	"github.com/xjfyrh/jobforge/internal/migrate"
	"github.com/xjfyrh/jobforge/internal/observability"
	"github.com/xjfyrh/jobforge/internal/outbox"
	"github.com/xjfyrh/jobforge/migrations"
)

type recordingConsumerProcessor struct {
	calls     atomic.Int64
	processed chan string
}

func (p *recordingConsumerProcessor) Process(
	_ context.Context,
	envelope *outbox.Envelope,
) (bool, error) {
	p.calls.Add(1)
	if p.processed != nil {
		select {
		case p.processed <- envelope.EventID:
		default:
		}
	}
	return false, nil
}

type transientConsumerSource struct {
	eventconsumer.Source
	mu           sync.Mutex
	readFailures int
	ackFailures  int
}

func (s *transientConsumerSource) ReadNew(
	ctx context.Context,
	block time.Duration,
) (*eventconsumer.Message, error) {
	s.mu.Lock()
	if s.readFailures > 0 {
		s.readFailures--
		s.mu.Unlock()
		return nil, errors.New("injected Redis read failure")
	}
	s.mu.Unlock()
	return s.Source.ReadNew(ctx, block)
}

func (s *transientConsumerSource) Ack(ctx context.Context, entryID string) error {
	s.mu.Lock()
	if s.ackFailures > 0 {
		s.ackFailures--
		s.mu.Unlock()
		return errors.New("injected Redis ACK failure")
	}
	s.mu.Unlock()
	return s.Source.Ack(ctx, entryID)
}

type transientConsumerProcessor struct {
	delegate eventconsumer.Processor
	mu       sync.Mutex
	failures int
}

func (p *transientConsumerProcessor) Process(
	ctx context.Context,
	envelope *outbox.Envelope,
) (bool, error) {
	p.mu.Lock()
	if p.failures > 0 {
		p.failures--
		p.mu.Unlock()
		return false, errors.New("injected database failure")
	}
	p.mu.Unlock()
	return p.delegate.Process(ctx, envelope)
}

type blockingConsumerHandler struct {
	started chan struct{}
}

func (h blockingConsumerHandler) Handle(
	ctx context.Context,
	tx pgx.Tx,
	envelope *outbox.Envelope,
) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO consumer_demo_effects (
			event_id, aggregate_id, aggregate_version, event_type
		) VALUES ($1, $2, $3, $4)
	`, envelope.EventID, envelope.AggregateID, envelope.AggregateVersion, envelope.EventType); err != nil {
		return fmt.Errorf("insert blocking effect: %w", err)
	}
	close(h.started)
	<-ctx.Done()
	return ctx.Err()
}

func resetConsumerTables(t *testing.T) {
	t.Helper()
	if _, err := testEnv.pool.Exec(context.Background(),
		`TRUNCATE consumer_demo_effects, consumer_inbox, consumer_inbox_binding RESTART IDENTITY`); err != nil {
		t.Fatalf("truncate consumer tables: %v", err)
	}
}

func newConsumerSource(
	t *testing.T,
	env *redisTestEnv,
	stream string,
	group string,
	name string,
) *eventconsumer.RedisSource {
	t.Helper()
	options, err := redis.ParseURL(env.url)
	if err != nil {
		t.Fatalf("parse test Redis URL: %v", err)
	}
	source, err := eventconsumer.NewRedisSource(
		redis.NewClient(options), stream, group, name, stream+":poison",
	)
	if err != nil {
		t.Fatalf("new Redis source: %v", err)
	}
	t.Cleanup(func() { _ = source.Close() })
	return source
}

func fastConsumerConfig(group string) eventconsumer.Config {
	return eventconsumer.Config{
		Group:               group,
		BlockTimeout:        20 * time.Millisecond,
		PendingScanInterval: 20 * time.Millisecond,
		PendingMinIdle:      750 * time.Millisecond,
		ProcessTimeout:      500 * time.Millisecond,
		MaxDeliveries:       5,
		RetryBase:           5 * time.Millisecond,
		RetryMax:            50 * time.Millisecond,
	}
}

func testConsumerEnvelope(eventID string) *outbox.Envelope {
	return &outbox.Envelope{
		SchemaVersion:    outbox.EnvelopeSchemaVersion,
		EventID:          eventID,
		AggregateID:      uuid.NewString(),
		AggregateVersion: 7,
		EventType:        "job.succeeded",
		OccurredAt:       time.Now().UTC(),
		Payload:          []byte(`{"value":"secret-payload-must-not-appear"}`),
		Traceparent:      "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	}
}

func consumerRowCounts(t *testing.T) (int, int) {
	t.Helper()
	var inboxCount int
	var effectCount int
	if err := testEnv.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM consumer_inbox`).Scan(&inboxCount); err != nil {
		t.Fatalf("count inbox: %v", err)
	}
	if err := testEnv.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM consumer_demo_effects`).Scan(&effectCount); err != nil {
		t.Fatalf("count effects: %v", err)
	}
	return inboxCount, effectCount
}

func groupDrained(ctx context.Context, client *redis.Client, stream string, group string) bool {
	pending, err := client.XPending(ctx, stream, group).Result()
	if err != nil || pending.Count != 0 {
		return false
	}
	groups, err := client.XInfoGroups(ctx, stream).Result()
	if err != nil {
		return false
	}
	for _, info := range groups {
		if info.Name == group {
			return info.Pending == 0 && info.Lag == 0
		}
	}
	return false
}

func prometheusCounterValue(t *testing.T, registry *promclient.Registry, name string) float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		var total float64
		for _, metric := range family.Metric {
			total += metric.GetCounter().GetValue()
		}
		return total
	}
	return 0
}

// TestDurableAT19TransactionalConsumer verifies the commit-before-ACK crash
// window independently of publisher duplication. Two stream entries carry the
// same event_id; Consumer A exits after commit, Consumer B first handles the
// second entry and then recovers A's PEL entry with XAUTOCLAIM.
func TestDurableAT19TransactionalConsumer(t *testing.T) {
	env := requireRedis(t)
	resetConsumerTables(t)
	ctx := context.Background()
	stream := "test:at19:" + uuid.NewString()[:8]
	group := "at19-" + uuid.NewString()[:8]
	transport := newDurableTransport(t, env, stream)
	client := newRedisClient(t, env)

	registry := promclient.NewRegistry()
	metrics, metricsShutdown, err := observability.SetupMetrics(ctx, registry)
	if err != nil {
		t.Fatalf("setup consumer metrics: %v", err)
	}
	t.Cleanup(func() { _ = metricsShutdown(context.Background()) })

	recorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(recorder),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tracerProvider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { _ = tracerProvider.Shutdown(context.Background()) })

	envelope := testConsumerEnvelope(fmt.Sprint(time.Now().UnixNano()))
	if err := transport.Publish(ctx, envelope); err != nil {
		t.Fatalf("publish first duplicate: %v", err)
	}
	if err := transport.Publish(ctx, envelope); err != nil {
		t.Fatalf("publish second duplicate: %v", err)
	}

	sourceA := newConsumerSource(t, env, stream, group, "consumer-a")
	processorA, err := eventconsumer.NewInboxProcessor(
		testEnv.pool, group, eventconsumer.DemoEffectHandler{}, metrics,
	)
	if err != nil {
		t.Fatalf("new processor A: %v", err)
	}
	configA := fastConsumerConfig(group)
	configA.AfterCommitBeforeAck = func(*eventconsumer.Message) error {
		return errors.New("injected process exit")
	}
	consumerA, err := eventconsumer.New(sourceA, processorA, configA, metrics, testLogger(t))
	if err != nil {
		t.Fatalf("new consumer A: %v", err)
	}
	if err := consumerA.Start(ctx); err != nil {
		t.Fatalf("start consumer A: %v", err)
	}
	if err := consumerA.Wait(); !errors.Is(err, eventconsumer.ErrAfterCommitHook) {
		t.Fatalf("consumer A result = %v, want crash failpoint", err)
	}
	if err := consumerA.Close(); err != nil {
		t.Fatalf("close consumer A: %v", err)
	}

	inboxCount, effectCount := consumerRowCounts(t)
	if inboxCount != 1 || effectCount != 1 {
		t.Fatalf("after commit-before-ACK crash: inbox=%d effects=%d, want 1/1",
			inboxCount, effectCount)
	}
	pending, err := client.XPending(ctx, stream, group).Result()
	if err != nil || pending.Count != 1 {
		t.Fatalf("crashed entry pending = %+v, err=%v; want count 1", pending, err)
	}

	sourceB := newConsumerSource(t, env, stream, group, "consumer-b")
	processorB, err := eventconsumer.NewInboxProcessor(
		testEnv.pool, group, eventconsumer.DemoEffectHandler{}, metrics,
	)
	if err != nil {
		t.Fatalf("new processor B: %v", err)
	}
	consumerB, err := eventconsumer.New(
		sourceB, processorB, fastConsumerConfig(group), metrics, testLogger(t),
	)
	if err != nil {
		t.Fatalf("new consumer B: %v", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	if err := consumerB.Start(runCtx); err != nil {
		t.Fatalf("start consumer B: %v", err)
	}
	waitFor(t, 15*time.Second, func() bool {
		return groupDrained(ctx, client, stream, group)
	})
	cancel()
	if err := consumerB.Wait(); err != nil {
		t.Fatalf("wait consumer B: %v", err)
	}
	if err := consumerB.Close(); err != nil {
		t.Fatalf("close consumer B: %v", err)
	}

	inboxCount, effectCount = consumerRowCounts(t)
	if inboxCount != 1 || effectCount != 1 {
		t.Fatalf("after duplicate and redelivery: inbox=%d effects=%d, want 1/1",
			inboxCount, effectCount)
	}
	if got := prometheusCounterValue(t, registry, "jobforge_event_redeliveries_total"); got < 1 {
		t.Fatalf("redelivery metric = %v, want >= 1", got)
	}
	if got := prometheusCounterValue(t, registry, "jobforge_consumer_inbox_duplicates_total"); got < 1 {
		t.Fatalf("inbox duplicate metric = %v, want >= 1", got)
	}

	spanNames := make(map[string]bool)
	for _, span := range recorder.Ended() {
		spanNames[span.Name()] = true
		if span.Name() == "event.consume" || span.Name() == "event.process" {
			if span.SpanContext().TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
				t.Fatalf("%s trace id = %s, envelope parent not continued",
					span.Name(), span.SpanContext().TraceID())
			}
			for _, attr := range span.Attributes() {
				if strings.Contains(fmt.Sprint(attr.Value.AsInterface()), "secret-payload") {
					t.Fatalf("%s span leaked full payload", span.Name())
				}
			}
		}
	}
	if !spanNames["event.consume"] || !spanNames["event.process"] {
		t.Fatalf("consumer spans missing: %v", spanNames)
	}

	var uniqueEffectEventIDIndexes int
	if err := testEnv.pool.QueryRow(ctx, `
		SELECT count(*)
		FROM pg_indexes
		WHERE tablename = 'consumer_demo_effects'
		  AND indexdef ILIKE '%UNIQUE%'
		  AND indexdef ILIKE '%event_id%'
	`).Scan(&uniqueEffectEventIDIndexes); err != nil {
		t.Fatalf("inspect demo effect indexes: %v", err)
	}
	if uniqueEffectEventIDIndexes != 0 {
		t.Fatal("demo effect event_id unexpectedly has a unique index")
	}
}

func TestEventConsumerPendingCursorSkipsFreshPrefix(t *testing.T) {
	env := requireRedis(t)
	ctx := context.Background()
	stream := "test:pending-cursor:" + uuid.NewString()[:8]
	group := "cursor-" + uuid.NewString()[:8]
	transport := newDurableTransport(t, env, stream)
	client := newRedisClient(t, env)

	const pendingCount = 25
	for i := 0; i < pendingCount; i++ {
		envelope := testConsumerEnvelope(strconv.FormatInt(time.Now().UnixNano()+int64(i), 10))
		if err := transport.Publish(ctx, envelope); err != nil {
			t.Fatalf("publish pending entry %d: %v", i, err)
		}
	}
	if err := client.XGroupCreate(ctx, stream, group, "0-0").Err(); err != nil {
		t.Fatalf("create cursor group: %v", err)
	}
	streams, err := client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: group, Consumer: "original-owner",
		Streams: []string{stream, ">"}, Count: pendingCount,
	}).Result()
	if err != nil || len(streams) != 1 || len(streams[0].Messages) != pendingCount {
		t.Fatalf("seed pending list: streams=%v err=%v", streams, err)
	}
	stale := streams[0].Messages[pendingCount-1]
	if err := client.Do(
		ctx, "XCLAIM", stream, group, "original-owner", "0", stale.ID, "IDLE", "30000",
	).Err(); err != nil {
		t.Fatalf("age tail pending entry: %v", err)
	}

	source := newConsumerSource(t, env, stream, group, "recovery-consumer")
	processor := &recordingConsumerProcessor{processed: make(chan string, 1)}
	config := fastConsumerConfig(group)
	config.ProcessTimeout = 100 * time.Millisecond
	config.PendingMinIdle = 10 * time.Second
	config.PendingScanInterval = 10 * time.Millisecond
	consumer, err := eventconsumer.New(source, processor, config, nil, testLogger(t))
	if err != nil {
		t.Fatalf("new cursor consumer: %v", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	if err := consumer.Start(runCtx); err != nil {
		t.Fatalf("start cursor consumer: %v", err)
	}
	select {
	case eventID := <-processor.processed:
		if eventID != stale.Values["event_id"] {
			t.Fatalf("processed event %q, want stale tail %q", eventID, stale.Values["event_id"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stale tail entry was starved behind fresh PEL prefix")
	}
	cancel()
	if err := consumer.Wait(); err != nil {
		t.Fatalf("wait cursor consumer: %v", err)
	}
	if err := consumer.Close(); err != nil {
		t.Fatalf("close cursor consumer: %v", err)
	}
}

func TestConsumerInboxBindingAndDuplicateMetadata(t *testing.T) {
	resetConsumerTables(t)
	ctx := context.Background()
	group := "binding-" + uuid.NewString()[:8]
	processorA, err := eventconsumer.NewInboxProcessor(
		testEnv.pool, group, eventconsumer.DemoEffectHandler{}, nil,
	)
	if err != nil {
		t.Fatalf("new group A processor: %v", err)
	}
	if err := processorA.EnsureBinding(ctx); err != nil {
		t.Fatalf("bind group A: %v", err)
	}
	processorA2, err := eventconsumer.NewInboxProcessor(
		testEnv.pool, group, eventconsumer.DemoEffectHandler{}, nil,
	)
	if err != nil {
		t.Fatalf("new second group A processor: %v", err)
	}
	if err := processorA2.EnsureBinding(ctx); err != nil {
		t.Fatalf("bind second group A processor: %v", err)
	}
	processorB, err := eventconsumer.NewInboxProcessor(
		testEnv.pool, "other-"+uuid.NewString()[:8], eventconsumer.DemoEffectHandler{}, nil,
	)
	if err != nil {
		t.Fatalf("new group B processor: %v", err)
	}
	if err := processorB.EnsureBinding(ctx); !errors.Is(err, eventconsumer.ErrConsumerGroupMismatch) {
		t.Fatalf("group B binding error = %v, want ErrConsumerGroupMismatch", err)
	}

	envelope := testConsumerEnvelope(strconv.FormatInt(time.Now().UnixNano(), 10))
	if duplicate, err := processorA.Process(ctx, envelope); err != nil || duplicate {
		t.Fatalf("process first event: duplicate=%v err=%v", duplicate, err)
	}
	mismatched := *envelope
	mismatched.AggregateID = uuid.NewString()
	if _, err := processorA2.Process(ctx, &mismatched); !errors.Is(err, eventconsumer.ErrInboxMetadataMismatch) {
		t.Fatalf("metadata mismatch error = %v, want ErrInboxMetadataMismatch", err)
	}
	inbox, effects := consumerRowCounts(t)
	if inbox != 1 || effects != 1 {
		t.Fatalf("binding mismatch changed effects: inbox=%d effects=%d", inbox, effects)
	}
}

func TestEventConsumerTransientFailuresRecoverWithoutPoison(t *testing.T) {
	env := requireRedis(t)
	resetConsumerTables(t)
	ctx := context.Background()
	stream := "test:transient-consumer:" + uuid.NewString()[:8]
	group := "transient-" + uuid.NewString()[:8]
	transport := newDurableTransport(t, env, stream)
	client := newRedisClient(t, env)
	if err := transport.Publish(
		ctx, testConsumerEnvelope(strconv.FormatInt(time.Now().UnixNano(), 10)),
	); err != nil {
		t.Fatalf("publish transient event: %v", err)
	}

	realSource := newConsumerSource(t, env, stream, group, "transient-consumer")
	source := &transientConsumerSource{
		Source: realSource, readFailures: 1, ackFailures: 1,
	}
	realProcessor, err := eventconsumer.NewInboxProcessor(
		testEnv.pool, group, eventconsumer.DemoEffectHandler{}, nil,
	)
	if err != nil {
		t.Fatalf("new transient processor: %v", err)
	}
	processor := &transientConsumerProcessor{delegate: realProcessor, failures: 1}
	config := fastConsumerConfig(group)
	config.ProcessTimeout = 100 * time.Millisecond
	config.PendingMinIdle = 250 * time.Millisecond
	config.PendingScanInterval = 10 * time.Millisecond
	consumer, err := eventconsumer.New(source, processor, config, nil, testLogger(t))
	if err != nil {
		t.Fatalf("new transient consumer: %v", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	if err := consumer.Start(runCtx); err != nil {
		t.Fatalf("start transient consumer: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool {
		if !groupDrained(ctx, client, stream, group) {
			return false
		}
		inbox, effects := consumerRowCounts(t)
		return inbox == 1 && effects == 1
	})
	cancel()
	if err := consumer.Wait(); err != nil {
		t.Fatalf("wait transient consumer: %v", err)
	}
	if err := consumer.Close(); err != nil {
		t.Fatalf("close transient consumer: %v", err)
	}
	if poisonLength, err := client.XLen(ctx, stream+":poison").Result(); err != nil || poisonLength != 0 {
		t.Fatalf("transient event poison length=%d err=%v", poisonLength, err)
	}
}

func TestEventConsumerDeletedPendingFailsClosed(t *testing.T) {
	env := requireRedis(t)
	ctx := context.Background()
	stream := "test:deleted-pending:" + uuid.NewString()[:8]
	group := "deleted-" + uuid.NewString()[:8]
	transport := newDurableTransport(t, env, stream)
	client := newRedisClient(t, env)
	if err := transport.Publish(
		ctx, testConsumerEnvelope(strconv.FormatInt(time.Now().UnixNano(), 10)),
	); err != nil {
		t.Fatalf("publish deleted-pending event: %v", err)
	}
	if err := client.XGroupCreate(ctx, stream, group, "0-0").Err(); err != nil {
		t.Fatalf("create deleted-pending group: %v", err)
	}
	streams, err := client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: group, Consumer: "original-owner", Streams: []string{stream, ">"}, Count: 1,
	}).Result()
	if err != nil || len(streams) != 1 || len(streams[0].Messages) != 1 {
		t.Fatalf("seed deleted pending entry: streams=%v err=%v", streams, err)
	}
	entryID := streams[0].Messages[0].ID
	if err := client.Do(
		ctx, "XCLAIM", stream, group, "original-owner", "0", entryID, "IDLE", "30000",
	).Err(); err != nil {
		t.Fatalf("age deleted pending entry: %v", err)
	}
	if deleted, err := client.XDel(ctx, stream, entryID).Result(); err != nil || deleted != 1 {
		t.Fatalf("delete pending payload: deleted=%d err=%v", deleted, err)
	}

	registry := promclient.NewRegistry()
	metrics, shutdown, err := observability.SetupMetrics(ctx, registry)
	if err != nil {
		t.Fatalf("setup integrity metrics: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	source := newConsumerSource(t, env, stream, group, "integrity-consumer")
	processor := &recordingConsumerProcessor{}
	config := fastConsumerConfig(group)
	config.ProcessTimeout = 100 * time.Millisecond
	config.PendingMinIdle = 10 * time.Second
	consumer, err := eventconsumer.New(source, processor, config, metrics, testLogger(t))
	if err != nil {
		t.Fatalf("new integrity consumer: %v", err)
	}
	runCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := consumer.Start(runCtx); err != nil {
		t.Fatalf("start integrity consumer: %v", err)
	}
	err = consumer.Wait()
	if !errors.Is(err, eventconsumer.ErrPendingPayloadDeleted) {
		t.Fatalf("consumer result = %v, want ErrPendingPayloadDeleted", err)
	}
	if processor.calls.Load() != 0 {
		t.Fatalf("deleted payload reached processor %d times", processor.calls.Load())
	}
	if got := prometheusCounterValue(t, registry, "jobforge_event_transport_failures_total"); got != 1 {
		t.Fatalf("integrity failure metric = %v, want 1", got)
	}
	if err := consumer.Close(); err != nil {
		t.Fatalf("close integrity consumer: %v", err)
	}
}

func TestEventConsumerPoisonIsolationAndForwardProgress(t *testing.T) {
	env := requireRedis(t)
	resetConsumerTables(t)
	ctx := context.Background()
	stream := "test:poison:" + uuid.NewString()[:8]
	group := "poison-" + uuid.NewString()[:8]
	transport := newDurableTransport(t, env, stream)
	client := newRedisClient(t, env)

	if _, err := client.XAdd(ctx, &redis.XAddArgs{
		Stream: stream,
		Values: map[string]any{
			"schema_version": "99",
			"event_id":       "malformed-" + uuid.NewString(),
			"payload":        `{"secret":"must-not-be-copied"}`,
		},
	}).Result(); err != nil {
		t.Fatalf("add malformed event: %v", err)
	}
	if err := transport.Publish(ctx, testConsumerEnvelope(fmt.Sprint(time.Now().UnixNano()))); err != nil {
		t.Fatalf("publish valid event after malformed one: %v", err)
	}

	source := newConsumerSource(t, env, stream, group, "poison-consumer")
	processor, err := eventconsumer.NewInboxProcessor(
		testEnv.pool, group, eventconsumer.DemoEffectHandler{}, nil,
	)
	if err != nil {
		t.Fatalf("new poison processor: %v", err)
	}
	consumer, err := eventconsumer.New(
		source, processor, fastConsumerConfig(group), nil, testLogger(t),
	)
	if err != nil {
		t.Fatalf("new poison consumer: %v", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	if err := consumer.Start(runCtx); err != nil {
		t.Fatalf("start poison consumer: %v", err)
	}
	waitFor(t, 15*time.Second, func() bool {
		length, err := client.XLen(ctx, stream+":poison").Result()
		if err != nil || length < 1 || !groupDrained(ctx, client, stream, group) {
			return false
		}
		inbox, effects := consumerRowCounts(t)
		return inbox == 1 && effects == 1
	})
	cancel()
	if err := consumer.Wait(); err != nil {
		t.Fatalf("wait poison consumer: %v", err)
	}
	if err := consumer.Close(); err != nil {
		t.Fatalf("close poison consumer: %v", err)
	}

	entries, err := client.XRange(ctx, stream+":poison", "-", "+").Result()
	if err != nil || len(entries) == 0 {
		t.Fatalf("read poison stream: entries=%d err=%v", len(entries), err)
	}
	poison := entries[0].Values
	if _, exists := poison["payload"]; exists {
		t.Fatal("poison record copied payload")
	}
	if strings.Contains(fmt.Sprint(poison), "must-not-be-copied") {
		t.Fatal("poison record leaked payload content")
	}
	if poison["reason"] != "invalid_envelope" || poison["delivery_count"] != "5" {
		t.Fatalf("poison metadata = %v", poison)
	}
}

// TestMigration0017FromClean0016 uses a dedicated temporary database so the
// production Migrator proves the real 0016 -> 0017 forward path. It then runs
// the destructive down migration and verifies dependency order.
func TestMigration0017FromClean0016(t *testing.T) {
	ctx := context.Background()
	adminConfig, err := pgxpool.ParseConfig(testEnv.dsn)
	if err != nil {
		t.Fatalf("parse admin DSN: %v", err)
	}
	adminConfig.ConnConfig.Database = "postgres"
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()
	databaseName := "jobforge_migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	databaseIdentifier := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+databaseIdentifier); err != nil {
		t.Fatalf("create temporary migration database: %v", err)
	}

	tempConfig, err := pgxpool.ParseConfig(testEnv.dsn)
	if err != nil {
		t.Fatalf("parse temporary database DSN: %v", err)
	}
	tempConfig.ConnConfig.Database = databaseName
	tempPool, err := pgxpool.NewWithConfig(ctx, tempConfig)
	if err != nil {
		t.Fatalf("open temporary migration database: %v", err)
	}
	defer func() {
		tempPool.Close()
		dropCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, dropErr := adminPool.Exec(
			dropCtx, "DROP DATABASE IF EXISTS "+databaseIdentifier+" WITH (FORCE)",
		); dropErr != nil {
			t.Logf("drop temporary migration database: %v", dropErr)
		}
	}()

	if _, err := tempPool.Exec(ctx, `
		CREATE TABLE schema_migrations (
			version integer primary key,
			name text not null,
			applied_at timestamptz not null default now()
		)
	`); err != nil {
		t.Fatalf("create migration ledger: %v", err)
	}
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		version, parseErr := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if parseErr != nil {
			t.Fatalf("parse migration version %s: %v", name, parseErr)
		}
		if version > 16 {
			continue
		}
		content, readErr := migrations.FS.ReadFile(name)
		if readErr != nil {
			t.Fatalf("read migration %s: %v", name, readErr)
		}
		tx, beginErr := tempPool.Begin(ctx)
		if beginErr != nil {
			t.Fatalf("begin migration %s: %v", name, beginErr)
		}
		if _, execErr := tx.Exec(ctx, string(content)); execErr != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("execute migration %s: %v", name, execErr)
		}
		if _, recordErr := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, version, name,
		); recordErr != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("record migration %s: %v", name, recordErr)
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			t.Fatalf("commit migration %s: %v", name, commitErr)
		}
	}
	if err := migrate.New(tempPool, testLogger(t)).Up(ctx); err != nil {
		t.Fatalf("migrate clean 0016 database to 0017: %v", err)
	}
	var forwardApplied bool
	if err := tempPool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = 17)
		   AND to_regclass('public.consumer_inbox_binding') IS NOT NULL
		   AND to_regclass('public.consumer_inbox') IS NOT NULL
		   AND to_regclass('public.consumer_demo_effects') IS NOT NULL
	`).Scan(&forwardApplied); err != nil {
		t.Fatalf("verify 0017 forward migration: %v", err)
	}
	if !forwardApplied {
		t.Fatal("Migrator did not apply complete 0017 schema")
	}
	downContent, err := migrations.FS.ReadFile("0017_create_consumer_inbox.down.sql")
	if err != nil {
		t.Fatalf("read migration 0017 down: %v", err)
	}
	if _, err := tempPool.Exec(ctx, string(downContent)); err != nil {
		t.Fatalf("execute migration 0017 down: %v", err)
	}
	var allDropped bool
	if err := tempPool.QueryRow(ctx, `
		SELECT to_regclass('public.consumer_demo_effects') IS NULL
		   AND to_regclass('public.consumer_inbox') IS NULL
		   AND to_regclass('public.consumer_inbox_binding') IS NULL
	`).Scan(&allDropped); err != nil {
		t.Fatalf("verify 0017 down migration: %v", err)
	}
	if !allDropped {
		t.Fatal("migration 0017 down did not drop all consumer tables")
	}
}

func TestEventConsumerCancellationRollsBackInFlight(t *testing.T) {
	env := requireRedis(t)
	resetConsumerTables(t)
	ctx := context.Background()
	stream := "test:cancel-consumer:" + uuid.NewString()[:8]
	group := "cancel-" + uuid.NewString()[:8]
	transport := newDurableTransport(t, env, stream)
	client := newRedisClient(t, env)
	if err := transport.Publish(ctx, testConsumerEnvelope(fmt.Sprint(time.Now().UnixNano()))); err != nil {
		t.Fatalf("publish cancellation event: %v", err)
	}

	started := make(chan struct{})
	source := newConsumerSource(t, env, stream, group, "cancel-consumer")
	processor, err := eventconsumer.NewInboxProcessor(
		testEnv.pool, group, blockingConsumerHandler{started: started}, nil,
	)
	if err != nil {
		t.Fatalf("new cancellation processor: %v", err)
	}
	config := fastConsumerConfig(group)
	config.ProcessTimeout = 5 * time.Second
	config.PendingMinIdle = 6 * time.Second
	consumer, err := eventconsumer.New(source, processor, config, nil, testLogger(t))
	if err != nil {
		t.Fatalf("new cancellation consumer: %v", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	if err := consumer.Start(runCtx); err != nil {
		t.Fatalf("start cancellation consumer: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not start")
	}
	cancel()
	if err := consumer.Wait(); err != nil {
		t.Fatalf("wait cancellation consumer: %v", err)
	}
	if err := consumer.Close(); err != nil {
		t.Fatalf("close cancellation consumer: %v", err)
	}

	inbox, effects := consumerRowCounts(t)
	if inbox != 0 || effects != 0 {
		t.Fatalf("cancelled transaction committed: inbox=%d effects=%d", inbox, effects)
	}
	pending, err := client.XPending(ctx, stream, group).Result()
	if err != nil || pending.Count != 1 {
		t.Fatalf("cancelled entry pending = %+v, err=%v; want 1", pending, err)
	}
}
