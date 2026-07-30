-- Purpose: Add session tracking fields to workers table for Gateway registration.
-- Lock behavior: ALTER TABLE ADD COLUMN acquires brief exclusive lock.
-- Data risk: None (nullable/new columns with defaults).
-- Rollback: Drop the columns (see down migration).
-- Verification: \d workers

alter table workers add column session_id uuid;
alter table workers add column registered_at timestamptz not null default now();
