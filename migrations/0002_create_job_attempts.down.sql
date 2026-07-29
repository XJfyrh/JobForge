-- Purpose: Rollback job_attempts table.
-- Data risk: All attempt audit data will be lost.

drop table if exists job_attempts;
