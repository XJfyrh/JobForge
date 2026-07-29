-- Purpose: Rollback workers table.
-- Data risk: All worker registration data will be lost.

drop table if exists workers;
