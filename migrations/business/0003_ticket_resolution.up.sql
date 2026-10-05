-- Add signed ticket resolutions and immutable receipts (ADR-0026).
-- Short catalog/table locks only; source statement guards serialize bounded
-- writes before row locks, including insertion of previously missing facts.
-- No source backfill. Down refuses existing effects; prefer forward repair.
-- Verify real PG role boundaries, loader/Apply races, up/down and lock ordering.
set local lock_timeout = '5s';

do $$
declare
    role_name text;
    existing record;
    is_login boolean;
begin
    foreach role_name in array array[
        'jobforge_business_writer', 'jobforge_business_writer_login',
        'jobforge_business_action_reader', 'jobforge_business_receipt_reader'
    ] loop
        is_login := role_name in (
            'jobforge_business_writer_login', 'jobforge_business_receipt_reader'
        );
        select r.*, shobj_description(r.oid, 'pg_authid') as marker
        into existing from pg_roles r where r.rolname = role_name;
        if found then
            if existing.marker is distinct from 'jobforge-business-v1'
                or existing.rolsuper or existing.rolcreatedb or existing.rolcreaterole
                or existing.rolreplication or existing.rolbypassrls
                or existing.rolcanlogin <> is_login then
                raise exception 'business action role identity conflict';
            end if;
            if is_login and existing.rolconfig is distinct from
                array['search_path=pg_catalog'] then
                raise exception 'business action role configuration conflict';
            end if;
            if exists (
                select 1 from pg_auth_members m join pg_roles p on p.oid = m.roleid
                where m.member = existing.oid and not (
                    (role_name = 'jobforge_business_writer_login'
                     and p.rolname = 'jobforge_business_writer')
                    or (role_name = 'jobforge_business_receipt_reader'
                        and p.rolname = 'jobforge_business_action_reader')
                )
            ) then
                raise exception 'business action role membership conflict';
            end if;
        else
            execute format('create role %I nologin nosuperuser nocreatedb nocreaterole', role_name);
            execute format('comment on role %I is %L', role_name, 'jobforge-business-v1');
            if is_login then
                -- Isolated development credentials; deployment supplies its own secrets.
                execute format('alter role %I login password %L', role_name, role_name);
                execute format('alter role %I set search_path = pg_catalog', role_name);
            end if;
        end if;
    end loop;
    execute format(
        'grant connect on database %I to jobforge_business_writer, jobforge_business_action_reader',
        current_database()
    );
end
$$;

grant jobforge_business_writer to jobforge_business_writer_login;
grant jobforge_business_action_reader to jobforge_business_receipt_reader;
grant usage on schema business, business_meta
to jobforge_business_writer, jobforge_business_action_reader;
grant select on all tables in schema business_meta
to jobforge_business_writer, jobforge_business_action_reader;

set local role jobforge_business_owner;
create table business.ticket_resolutions (
    tenant_id text not null,
    operation_id uuid not null,
    business_request_id uuid not null,
    ticket_id text not null,
    authorization_hash text not null check (authorization_hash ~ '^[a-f0-9]{64}$'),
    parameters jsonb not null check (octet_length(parameters::text) <= 16384),
    receipt jsonb not null check (octet_length(receipt::text) <= 4096),
    applied_at timestamptz not null,
    retain_until timestamptz not null,
    primary key (tenant_id, operation_id),
    unique (tenant_id, business_request_id),
    foreign key (tenant_id, ticket_id) references business.tickets (tenant_id, ticket_id),
    check (retain_until >= applied_at + interval '30 days'),
    check (parameters ->> 'ticket_id' = ticket_id),
    check (receipt ->> 'tenant_id' = tenant_id),
    check (receipt ->> 'operation_id' = operation_id::text),
    check (receipt ->> 'authorization_hash' = authorization_hash)
);

-- BEFORE STATEMENT runs before tuple locks. The same transaction guard is
-- explicitly acquired at the start of loader and first-action transactions.
create function business.guard_action_sources() returns trigger
language plpgsql set search_path = pg_catalog as $$
begin
    perform pg_advisory_xact_lock(7483921656::bigint);
    return null;
end
$$;
create trigger ticket_action_guard before insert or update or delete on business.tickets
for each statement execute function business.guard_action_sources();
create trigger order_action_guard before insert or update or delete on business.orders
for each statement execute function business.guard_action_sources();
create trigger delivery_action_guard before insert or update or delete on business.deliveries
for each statement execute function business.guard_action_sources();
create trigger policy_action_guard before insert or update or delete on business.policy_versions
for each statement execute function business.guard_action_sources();
create trigger index_action_guard before insert or update or delete on business.policy_indexes
for each statement execute function business.guard_action_sources();
create trigger policy_revision before update on business.policy_versions
for each row execute function business.guard_source_revision();

create function business.guard_resolution_immutable() returns trigger
language plpgsql set search_path = pg_catalog as $$
begin
    raise exception 'business resolution and receipt are immutable';
end
$$;
create trigger resolution_immutable before update or delete on business.ticket_resolutions
for each row execute function business.guard_resolution_immutable();
revoke all on all functions in schema business from public;
reset role;

grant select on business.tickets, business.orders, business.deliveries,
business.policy_versions, business.policy_indexes, business.snapshots,
business.ticket_resolutions to jobforge_business_writer;
grant update (revision, body) on business.tickets to jobforge_business_writer;
grant insert on business.ticket_resolutions to jobforge_business_writer;
grant select on business.ticket_resolutions to jobforge_business_action_reader;
