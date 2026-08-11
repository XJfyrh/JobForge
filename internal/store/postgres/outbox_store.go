package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xjfyrh/jobforge/internal/store"
)

// OutboxStore implements store.OutboxStore using PostgreSQL. All operations
// are independent of the job state transaction path: the publisher marks
// events published in short standalone statements, never inside job
// transactions (PRD v0.2 NFR-203).
type OutboxStore struct {
	pool *pgxpool.Pool
}

// NewOutboxStore creates a new PostgreSQL-backed outbox store.
func NewOutboxStore(pool *pgxpool.Pool) *OutboxStore {
	return &OutboxStore{pool: pool}
}

// Ensure interface compliance at compile time.
var _ store.OutboxStore = (*OutboxStore)(nil)

// OutboxClaimTTL bounds how long a publisher claim stays exclusive. Events
// claimed but still unpublished past this TTL (the claiming publisher
// crashed between claim and mark) become eligible for reclaim by any
// publisher. Hardcoded like the publisher's backoff parameters.
const OutboxClaimTTL = 5 * time.Minute

// fetchUnpublished atomically claims a batch of unpublished events in one
// statement: the UPDATE sets claimed_at for the rows selected by the inner
// FOR UPDATE SKIP LOCKED query, so concurrent publishers can never claim the
// same event. Rows claimed but not published within OutboxClaimTTL are
// reclaimable, which recovers events left behind by a crashed publisher.
// Delivery remains at-least-once: a crash between NOTIFY and marking still
// duplicates the event, and consumers deduplicate by event_id.
const fetchUnpublished = `
update outbox_events
set claimed_at = now()
where event_id in (
    select event_id
    from outbox_events
    where published_at is null
      and (claimed_at is null or claimed_at < now() - $1::interval)
    order by created_at
    limit $2
    for update skip locked
)
returning event_id, aggregate_id, event_type, payload, created_at, published_at, publish_attempts, aggregate_version, traceparent`

// markPublished records successful publication. Guarded by published_at IS
// NULL so concurrent publishers cannot double-mark.
const markPublished = `
update outbox_events
set published_at = now()
where event_id = $1 and published_at is null`

// markPublishedBatch is the batch form of markPublished: one statement for a
// whole publish round keeps the mark cost O(1) round trips regardless of
// batch size (NFR-302 smoke floor).
const markPublishedBatch = `
update outbox_events
set published_at = now()
where event_id = any($1) and published_at is null`

// markPublishFailed increments the attempt counter without changing
// publication state; the event remains eligible for retry.
const markPublishFailed = `
update outbox_events
set publish_attempts = publish_attempts + 1
where event_id = $1 and published_at is null`

// resetClaim releases an atomic claim (claimed_at back to NULL) without
// touching publication state. Used when a claimed event failed to publish or
// was left unprocessed (graceful shutdown), so it becomes immediately
// reclaimable instead of waiting out the claim TTL. Guarded by
// published_at IS NULL so a stale reset can never revive a published row.
const resetClaim = `
update outbox_events
set claimed_at = null
where event_id = $1 and published_at is null`

// countPending reports the unpublished backlog for the jobforge_outbox_pending
// gauge (PRD v0.2 §8).
const countPending = `
select count(*) from outbox_events where published_at is null`

// cleanupPublished implements retention: only published events older than the
// retention period are removed. Unpublished events are never deleted
// (PRD v0.2 FR-613).
const cleanupPublished = `
delete from outbox_events
where published_at is not null and published_at < now() - $1::interval`

// FetchUnpublished atomically claims up to batch unpublished events ordered
// by created_at (see fetchUnpublished). Events claimed but not published
// within OutboxClaimTTL are reclaimable.
func (s *OutboxStore) FetchUnpublished(ctx context.Context, batch int) ([]*store.OutboxEvent, error) {
	rows, err := s.pool.Query(ctx, fetchUnpublished, OutboxClaimTTL, batch)
	if err != nil {
		return nil, fmt.Errorf("fetch unpublished: %w", err)
	}
	defer rows.Close()

	events := make([]*store.OutboxEvent, 0, batch)
	for rows.Next() {
		var ev store.OutboxEvent
		if err := rows.Scan(
			&ev.EventID, &ev.AggregateID, &ev.EventType, &ev.Payload,
			&ev.CreatedAt, &ev.PublishedAt, &ev.PublishAttempts,
			&ev.AggregateVersion, &ev.Traceparent,
		); err != nil {
			return nil, fmt.Errorf("scan outbox event: %w", err)
		}
		events = append(events, &ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate unpublished rows: %w", err)
	}
	return events, nil
}

// MarkPublished sets published_at to the current time for the event.
// Returns true if the row was updated (i.e. this publisher was the first to
// publish it); false if another instance already marked it.
func (s *OutboxStore) MarkPublished(ctx context.Context, eventID int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, markPublished, eventID)
	if err != nil {
		return false, fmt.Errorf("mark published: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// MarkPublishedBatch sets published_at for every event in one statement
// (see markPublishedBatch). Returns the number of rows transitioned.
func (s *OutboxStore) MarkPublishedBatch(ctx context.Context, eventIDs []int64) (int64, error) {
	if len(eventIDs) == 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, markPublishedBatch, eventIDs)
	if err != nil {
		return 0, fmt.Errorf("mark published batch: %w", err)
	}
	return tag.RowsAffected(), nil
}

// MarkPublishFailed increments publish_attempts for the event. The event
// stays unpublished and will be retried on a later polling round.
func (s *OutboxStore) MarkPublishFailed(ctx context.Context, eventID int64) error {
	if _, err := s.pool.Exec(ctx, markPublishFailed, eventID); err != nil {
		return fmt.Errorf("mark publish failed: %w", err)
	}
	return nil
}

// ResetClaim releases the atomic claim on the event (see resetClaim), making
// it immediately reclaimable again.
func (s *OutboxStore) ResetClaim(ctx context.Context, eventID int64) error {
	if _, err := s.pool.Exec(ctx, resetClaim, eventID); err != nil {
		return fmt.Errorf("reset claim: %w", err)
	}
	return nil
}

// CountPending returns the number of unpublished events.
func (s *OutboxStore) CountPending(ctx context.Context) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, countPending).Scan(&n); err != nil {
		return 0, fmt.Errorf("count pending: %w", err)
	}
	return n, nil
}

// CleanupPublished deletes events that were published before the retention
// period. Unpublished events are never removed. Returns the number of rows
// deleted.
func (s *OutboxStore) CleanupPublished(ctx context.Context, retention time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, cleanupPublished, retention)
	if err != nil {
		return 0, fmt.Errorf("cleanup published: %w", err)
	}
	return tag.RowsAffected(), nil
}
