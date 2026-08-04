-- Rollback: remove the published_at retention index added in 0007.
-- Lock risk: DROP INDEX takes a brief exclusive lock on the index only.
-- Data risk: None (index only; outbox rows are untouched).

drop index if exists idx_outbox_published_at;
