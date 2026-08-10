-- Purpose: Derived per-tenant inflight quota counter table (PRD v0.3 FR-720,
-- ADR-0007). The Claim transaction reserves slots with an atomic conditional
-- upsert on this table instead of re-counting jobs rows inside the candidate
-- row-lock window; every state transition that leaves inflight decrements the
-- counter in the same transaction. jobs remains the sole source of truth:
-- this table is always rebuildable from
--   select tenant_id, count(*) from jobs
--   where state in ('running','cancelling') group by tenant_id
-- and must never be used for job queries, recovery or audit.
-- Lock behavior: CREATE TABLE takes an ACCESS EXCLUSIVE lock on the new
-- (empty) table only. The backfill aggregate is a one-shot scan of jobs; at
-- MVP scale this is acceptable, very large production tables should run the
-- backfill out-of-band and deploy the table without it.
-- Data risk: None (new table only; backfill is idempotent via ON CONFLICT).
-- Rollback: Drop the table (see down migration). Quota enforcement falls back
-- to the code path selected at build time; see ADR-0007 §7.
-- Verification: select * from tenant_quota_counters;

create table if not exists tenant_quota_counters (
    tenant_id  text primary key,
    inflight   integer not null check (inflight >= 0),
    updated_at timestamptz not null default now()
);

-- Backfill from the current jobs state (source of truth) so counters are
-- correct immediately after migration, including pre-existing running jobs.
insert into tenant_quota_counters (tenant_id, inflight, updated_at)
select tenant_id, count(*), now()
from jobs
where state in ('running', 'cancelling')
group by tenant_id
on conflict (tenant_id) do update
set inflight = excluded.inflight,
    updated_at = now();
