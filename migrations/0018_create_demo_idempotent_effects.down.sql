-- Rollback is intentionally destructive for demo business-effect evidence.
-- The table has no dependants or foreign key to jobs, so dropping it does not
-- change task state, attempts, leases, fencing tokens, or outbox history.

drop table demo_idempotent_effects;
