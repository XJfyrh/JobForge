-- Purpose: Revert migration 0008 (request_hash column).
-- Data risk: Drops the request_hash column and its data; conflict detection
-- falls back to legacy deduplicate-for-all semantics.
-- Rollback: Re-apply the up migration.
-- Verification: \d jobs

alter table jobs drop column request_hash;
