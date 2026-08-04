-- Purpose: Support retention cleanup for published outbox events (PRD v0.2
-- FR-613). The publisher deletes rows where published_at IS NOT NULL and
-- published_at < now() - retention; this partial index accelerates that scan.
-- Lock behavior: CREATE INDEX CONCURRENTLY is not usable inside a migration
-- transaction; the table is small and append-mostly, so a plain CREATE INDEX
-- is acceptable here.
-- Data risk: None (new index only; no data change).
-- Rollback: Drop the index (see down migration).
-- Verification: \d outbox_events

create index if not exists idx_outbox_published_at on outbox_events (published_at)
where published_at is not null;
