-- Rollback: Remove session tracking fields from workers table.

alter table workers drop column if exists session_id;
alter table workers drop column if exists registered_at;
