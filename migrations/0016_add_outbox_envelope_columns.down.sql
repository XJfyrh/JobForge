-- Rollback for 0016: drop the envelope-support columns. Events written while
-- the columns existed lose their aggregate_version/traceparent capture; the
-- publisher falls back to the legacy NOTIFY-only payload shape.
alter table outbox_events drop column if exists aggregate_version;
alter table outbox_events drop column if exists traceparent;
