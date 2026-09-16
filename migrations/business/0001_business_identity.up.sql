-- Initialize a verified empty, dedicated business database (ADR-0016).
-- DDL takes catalog locks; only the migration identity runs this file.
-- Role names are global: never mutate an unrelated existing role or its password.
-- Forward recovery reruns initialization; teardown preserves marked global roles.
-- Verify purpose rejection, role membership and low-privilege startup in real PG.
do $$
declare
    role_name text;
    marker text := 'jobforge-business-v1';
    existing record;
begin
    foreach role_name in array array[
        'jobforge_business_owner', 'jobforge_business_runtime', 'jobforge_business_loader',
        'jobforge_business_reader', 'jobforge_business_loader_login'
    ] loop
        select r.*, shobj_description(r.oid, 'pg_authid') as marker
        into existing from pg_roles r where r.rolname = role_name;
        if found then
            if existing.marker is distinct from marker
                or existing.rolsuper or existing.rolcreatedb or existing.rolcreaterole
                or existing.rolreplication or existing.rolbypassrls
                or existing.rolcanlogin <> (role_name in (
                    'jobforge_business_reader', 'jobforge_business_loader_login'
                )) then
                raise exception 'business role identity conflict';
            end if;
            if role_name in ('jobforge_business_reader', 'jobforge_business_loader_login')
                and existing.rolconfig is distinct from array['search_path=pg_catalog'] then
                raise exception 'business role configuration conflict';
            end if;
            if exists (
                select 1 from pg_auth_members m join pg_roles p on p.oid = m.roleid
                where m.member = existing.oid and not (
                    (role_name = 'jobforge_business_reader'
                     and p.rolname = 'jobforge_business_runtime')
                    or (role_name = 'jobforge_business_loader_login'
                        and p.rolname = 'jobforge_business_loader')
                )
            ) then
                raise exception 'business role membership conflict';
            end if;
        else
            execute format('create role %I nologin nosuperuser nocreatedb nocreaterole', role_name);
            execute format('comment on role %I is %L', role_name, marker);
            if role_name in ('jobforge_business_reader', 'jobforge_business_loader_login') then
                -- Fixed local demo credentials only, created on the isolated dev instance.
                execute format('alter role %I login password %L', role_name, role_name);
                execute format('alter role %I set search_path = pg_catalog', role_name);
            end if;
        end if;
    end loop;
    execute format('revoke all on database %I from public', current_database());
    execute format(
        'grant connect on database %I to jobforge_business_runtime, jobforge_business_loader',
        current_database()
    );
end
$$;

grant jobforge_business_runtime to jobforge_business_reader;
grant jobforge_business_loader to jobforge_business_loader_login;
revoke all on schema public from public;

create schema business_meta authorization jobforge_business_owner;
set local role jobforge_business_owner;
create table business_meta.database_identity (
    singleton boolean primary key default true check (singleton),
    purpose text not null check (purpose = 'jobforge-business-v1'),
    database_name text not null,
    created_at timestamptz not null default now()
);
insert into business_meta.database_identity (purpose, database_name)
values ('jobforge-business-v1', current_database());
create table business_meta.business_schema_migrations (
    version integer primary key,
    applied_at timestamptz not null default now()
);
reset role;
grant usage on schema business_meta to jobforge_business_runtime, jobforge_business_loader;
grant select on all tables in schema business_meta
to jobforge_business_runtime, jobforge_business_loader;
