-- Rollback is intentionally destructive for reference-consumer data. Drop the
-- dependent effect table before the inbox and binding tables so foreign keys
-- are removed in dependency order. This does not modify jobs or outbox_events.

drop table consumer_demo_effects;
drop table consumer_inbox;
drop table consumer_inbox_binding;
