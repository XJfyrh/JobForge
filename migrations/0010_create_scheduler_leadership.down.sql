-- Purpose: Revert migration 0010 (scheduler_leadership table).
-- Data risk: Drops the leadership lease row; the Scheduler falls back to
-- advisory-lock-only election.
-- Rollback: Re-apply the up migration.
-- Verification: \d scheduler_leadership

drop table scheduler_leadership;
