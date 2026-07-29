-- Purpose: Rollback outbox_events table.
-- Data risk: All outbox events will be lost.

drop table if exists outbox_events;
