-- Run psql -X -q -A -t -v ON_ERROR_STOP=1 as the deployed reader before
-- starting the batch and after the container exits. Snapshots are excluded:
-- trusted admission is allowed to insert them.
begin transaction isolation level repeatable read read only;
with records as (
    select 'dataset_imports' as name, to_jsonb(t) as body from business.dataset_imports as t
    union all select 'policy_versions', to_jsonb(t) from business.policy_versions as t
    union all select 'tickets', to_jsonb(t) from business.tickets as t
    union all select 'orders', to_jsonb(t) from business.orders as t
    union all select 'deliveries', to_jsonb(t) from business.deliveries as t
    union all select 'policy_indexes', to_jsonb(t) from business.policy_indexes as t
    union all select 'policy_chunks', to_jsonb(t) from business.policy_chunks as t
), names as (
    select unnest(array[
        'dataset_imports', 'policy_versions', 'tickets', 'orders',
        'deliveries', 'policy_indexes', 'policy_chunks'
    ]) as name
), facts as (
    select n.name, count(r.body) as rows,
        encode(sha256(convert_to(coalesce(
            jsonb_agg(r.body order by r.body::text) filter (where r.body is not null),
            '[]'::jsonb
        )::text, 'UTF8')), 'hex') as sha256,
        has_table_privilege(current_user, 'business.' || n.name, 'INSERT,UPDATE,DELETE,TRUNCATE')
        or has_any_column_privilege(current_user, 'business.' || n.name, 'INSERT,UPDATE') as can_write
    from names as n left join records as r on n.name = r.name
    group by n.name
)
select jsonb_build_object(
    'schema_version', 1,
    'observed_at', clock_timestamp(),
    'database', current_database(),
    'role', current_user,
    'facts', jsonb_object_agg(name, jsonb_build_object('rows', rows, 'sha256', sha256)),
    'write_privileges', jsonb_object_agg(name, can_write)
) from facts;
commit;
