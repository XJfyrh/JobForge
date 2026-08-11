package eventconsumer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/xjfyrh/jobforge/internal/outbox"
)

type fakeSource struct {
	mu              sync.Mutex
	readStarted     chan struct{}
	readStartedOnce sync.Once
	acks            int
	quarantines     int
	ackErr          error
	quarantineErr   error
	closed          bool
}

func (s *fakeSource) Transport() string { return redisStreamsTransport }
func (s *fakeSource) EnsureGroup(context.Context) error {
	return nil
}
func (s *fakeSource) ReadNew(ctx context.Context, _ time.Duration) (*Message, error) {
	s.readStartedOnce.Do(func() {
		if s.readStarted != nil {
			close(s.readStarted)
		}
	})
	<-ctx.Done()
	return nil, ctx.Err()
}
func (s *fakeSource) ClaimStale(context.Context, time.Duration, string) (*Message, string, error) {
	return nil, "0-0", nil
}
func (s *fakeSource) DeliveryCount(context.Context, string) (int64, error) { return 1, nil }
func (s *fakeSource) Ack(context.Context, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acks++
	return s.ackErr
}
func (s *fakeSource) Quarantine(context.Context, *Message, string, int64, time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quarantines++
	return s.quarantineErr
}
func (s *fakeSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

type fakeProcessor struct {
	duplicate bool
	err       error
}

func (p fakeProcessor) Process(context.Context, *outbox.Envelope) (bool, error) {
	return p.duplicate, p.err
}

func testConfig() Config {
	return Config{
		Group:               "test-group",
		BlockTimeout:        10 * time.Millisecond,
		PendingScanInterval: 10 * time.Millisecond,
		PendingMinIdle:      50 * time.Millisecond,
		ProcessTimeout:      20 * time.Millisecond,
		MaxDeliveries:       2,
		RetryBase:           time.Millisecond,
		RetryMax:            5 * time.Millisecond,
	}
}

func testMessage() *Message {
	return &Message{
		EntryID: "1-0",
		Envelope: &outbox.Envelope{
			SchemaVersion:    outbox.EnvelopeSchemaVersion,
			EventID:          "1",
			AggregateID:      "job-1",
			AggregateVersion: 1,
			EventType:        "job.succeeded",
			OccurredAt:       time.Now(),
			Payload:          []byte(`{"ok":true}`),
		},
		DeliveryCount: 1,
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewRedisSourceRejectsPoisonSourceLoop(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = client.Close() })
	if _, err := NewRedisSource(
		client, "jobforge:events", "test-group", "test-consumer", "jobforge:events",
	); err == nil {
		t.Fatal("expected source/poison stream collision rejection")
	}
}

func TestConsumerTransientProcessingErrorStaysPending(t *testing.T) {
	source := &fakeSource{}
	consumer, err := New(
		source, fakeProcessor{err: errors.New("database unavailable")},
		testConfig(), nil, testLogger(),
	)
	if err != nil {
		t.Fatalf("new consumer: %v", err)
	}
	message := testMessage()
	message.DeliveryCount = 100 // infrastructure retries never trigger poison
	message.Redelivered = true
	if err := consumer.handle(context.Background(), message); err == nil {
		t.Fatal("expected transient processing error")
	}
	if source.acks != 0 || source.quarantines != 0 {
		t.Fatalf("transient failure acked or quarantined: ack=%d poison=%d",
			source.acks, source.quarantines)
	}
}

func TestConsumerPermanentFailureQuarantinesOnlyAtLimit(t *testing.T) {
	source := &fakeSource{}
	consumer, err := New(
		source,
		fakeProcessor{err: Permanent("invalid_business_event", errors.New("rejected"))},
		testConfig(), nil, testLogger(),
	)
	if err != nil {
		t.Fatalf("new consumer: %v", err)
	}
	message := testMessage()
	if err := consumer.handle(context.Background(), message); err != nil {
		t.Fatalf("first permanent delivery: %v", err)
	}
	if source.acks != 0 || source.quarantines != 0 {
		t.Fatal("permanent event isolated before delivery limit")
	}
	message.DeliveryCount = 2
	message.Redelivered = true
	if err := consumer.handle(context.Background(), message); err != nil {
		t.Fatalf("second permanent delivery: %v", err)
	}
	if source.acks != 1 || source.quarantines != 1 {
		t.Fatalf("limit action: ack=%d poison=%d, want 1/1", source.acks, source.quarantines)
	}
}

func TestConsumerAfterCommitFailpointLeavesEntryUnacked(t *testing.T) {
	source := &fakeSource{}
	config := testConfig()
	config.AfterCommitBeforeAck = func(*Message) error { return errors.New("injected crash") }
	consumer, err := New(source, fakeProcessor{}, config, nil, testLogger())
	if err != nil {
		t.Fatalf("new consumer: %v", err)
	}
	err = consumer.handle(context.Background(), testMessage())
	if !errors.Is(err, ErrAfterCommitHook) {
		t.Fatalf("failpoint error = %v, want ErrAfterCommitHook", err)
	}
	if source.acks != 0 {
		t.Fatalf("entry ACKed across crash failpoint: %d", source.acks)
	}
}

func TestConsumerAckFailureRemainsPendingWithoutPoison(t *testing.T) {
	source := &fakeSource{ackErr: errors.New("Redis unavailable")}
	consumer, err := New(source, fakeProcessor{}, testConfig(), nil, testLogger())
	if err != nil {
		t.Fatalf("new consumer: %v", err)
	}
	if err := consumer.handle(context.Background(), testMessage()); err == nil {
		t.Fatal("expected ACK failure")
	}
	if source.acks != 1 || source.quarantines != 0 {
		t.Fatalf("ACK failure actions: ack=%d poison=%d", source.acks, source.quarantines)
	}
}

func TestConsumerFatalProcessorFailureIsNeverAckedOrPoisoned(t *testing.T) {
	source := &fakeSource{}
	consumer, err := New(
		source,
		fakeProcessor{err: fatal("inbox_metadata_mismatch", ErrInboxMetadataMismatch)},
		testConfig(), nil, testLogger(),
	)
	if err != nil {
		t.Fatalf("new consumer: %v", err)
	}
	err = consumer.handle(context.Background(), testMessage())
	if !errors.Is(err, ErrInboxMetadataMismatch) {
		t.Fatalf("fatal processor error = %v, want ErrInboxMetadataMismatch", err)
	}
	if source.acks != 0 || source.quarantines != 0 {
		t.Fatalf("fatal processor error acked or poisoned: ack=%d poison=%d",
			source.acks, source.quarantines)
	}
}

func TestConsumerPoisonWriteMustSucceedBeforeAck(t *testing.T) {
	source := &fakeSource{quarantineErr: errors.New("Redis unavailable")}
	consumer, err := New(
		source,
		fakeProcessor{err: Permanent("invalid_business_event", errors.New("rejected"))},
		testConfig(), nil, testLogger(),
	)
	if err != nil {
		t.Fatalf("new consumer: %v", err)
	}
	message := testMessage()
	message.DeliveryCount = 2
	if err := consumer.handle(context.Background(), message); err == nil {
		t.Fatal("expected poison XADD failure")
	}
	if source.quarantines != 1 || source.acks != 0 {
		t.Fatalf("poison failure actions: poison=%d ack=%d", source.quarantines, source.acks)
	}
}

func TestConsumerCloseWaitLifecycle(t *testing.T) {
	source := &fakeSource{readStarted: make(chan struct{})}
	consumer, err := New(source, fakeProcessor{}, testConfig(), nil, testLogger())
	if err != nil {
		t.Fatalf("new consumer: %v", err)
	}
	if err := consumer.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-source.readStarted:
	case <-time.After(time.Second):
		t.Fatal("consumer did not enter read loop")
	}
	if err := consumer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := consumer.Wait(); err != nil {
		t.Fatalf("wait after cancellation: %v", err)
	}
	if !source.closed {
		t.Fatal("source not closed")
	}
}
