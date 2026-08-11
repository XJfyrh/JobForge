-- Reference consumer inbox for the v0.3 transactional consumption protocol.
--
-- Lock behavior: this migration only creates new tables and does not lock or
-- rewrite jobs, outbox_events, or other existing application tables.
-- Data risk: forward migration is additive. A rollback deletes all reference
-- consumer inbox and demo-effect data created after this migration.
-- Verification:
--   SELECT to_regclass('public.consumer_inbox_binding');
--   SELECT to_regclass('public.consumer_inbox');
--   SELECT to_regclass('public.consumer_demo_effects');
--   SELECT conname FROM pg_constraint
--     WHERE conrelid = 'consumer_demo_effects'::regclass;
-- The last query must show the primary key and inbox foreign key, but no unique
-- constraint on consumer_demo_effects.event_id. The binding table deliberately
-- permits exactly one row so one inbox schema cannot silently serve two groups.

create table consumer_inbox_binding (
    binding_id smallint primary key default 1 check (binding_id = 1),
    consumer_group text not null unique,
    bound_at timestamptz not null default clock_timestamp()
);

create table consumer_inbox (
    event_id text primary key,
    consumer_group text not null references consumer_inbox_binding (consumer_group),
    aggregate_id text not null,
    aggregate_version bigint not null,
    processed_at timestamptz not null default clock_timestamp()
);

create table consumer_demo_effects (
    effect_id bigint generated always as identity primary key,
    event_id text not null references consumer_inbox (event_id),
    aggregate_id text not null,
    aggregate_version bigint not null,
    event_type text not null,
    applied_at timestamptz not null default clock_timestamp()
);
