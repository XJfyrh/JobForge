-- Rollback for migration 0013: drop the derived quota counter table.
-- Data risk: loses derived counters only; jobs is the source of truth and
-- counters are rebuildable (see up migration header).
drop table if exists tenant_quota_counters;
