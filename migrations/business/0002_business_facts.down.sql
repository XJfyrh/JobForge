-- Explicit rollback of all S1 business facts, snapshots and vectors.
-- Destructive for this dedicated database: only run in a disposable test environment.
-- Access-exclusive/catalog locks are expected; no control tables are touched.
-- Reapply version 2 then reseed/reprepare. Verify marker and restricted roles survive.
drop schema business cascade;
drop extension vector;
drop schema extensions;
