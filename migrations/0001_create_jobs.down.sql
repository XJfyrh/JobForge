-- Purpose: Rollback jobs table creation.
-- Lock behavior: DROP TABLE acquires exclusive lock.
-- Data risk: All job data will be lost.
-- Verification: \d jobs should return "did not find any relation".

drop table if exists jobs;
