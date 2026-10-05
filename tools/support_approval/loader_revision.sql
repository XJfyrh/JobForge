-- Sole predeclared S4 source-conflict mutation, executed as the actual loader.
-- Admission snapshot/cursor/lease/deadline and action authorization are untouched.
begin;
with changed as (
    update business.tickets set revision = revision + 1,
        body = jsonb_set(body, '{revision}', to_jsonb(revision + 1))
    where tenant_id = :'tenant_id' and ticket_id = :'ticket_id'
        and current_user = 'jobforge_business_loader_login'
    returning tenant_id, ticket_id, revision
)
select jsonb_build_object('schema_version', 1, 'role', current_user,
    'changed', coalesce(jsonb_agg(to_jsonb(changed)), '[]'::jsonb)) from changed;
commit;
