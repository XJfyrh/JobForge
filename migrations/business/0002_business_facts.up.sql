-- Create bounded business facts, immutable snapshots and exact policy vectors.
-- Fresh-table DDL takes catalog locks; there is no legacy data backfill.
-- Down removes this isolated business data only and retains database identity.
-- Verify up/down, role boundaries, atomic publication and RR snapshots in real PG.
create schema extensions authorization jobforge_business_owner;
create extension vector with schema extensions version '0.8.6';
create schema business authorization jobforge_business_owner;
set local role jobforge_business_owner;

create table business.dataset_imports (
    dataset_version text primary key,
    content_hash text not null check (content_hash ~ '^[a-f0-9]{64}$'),
    imported_at timestamptz not null default now()
);
create table business.policy_versions (
    tenant_id text not null,
    policy_version text not null,
    revision bigint not null check (revision > 0),
    corpus_sha256 text not null check (corpus_sha256 ~ '^[a-f0-9]{64}$'),
    body jsonb not null check (octet_length(body::text) <= 2048),
    primary key (tenant_id, policy_version),
    check (body ->> 'tenant_id' = tenant_id),
    check (body ->> 'policy_version' = policy_version),
    check ((body ->> 'revision')::bigint = revision),
    check (body ->> 'corpus_sha256' = corpus_sha256)
);
create table business.tickets (
    tenant_id text not null,
    ticket_id text not null,
    revision bigint not null check (revision > 0),
    body jsonb not null check (octet_length(body::text) <= 4096),
    primary key (tenant_id, ticket_id),
    check (body ->> 'tenant_id' = tenant_id),
    check (body ->> 'ticket_id' = ticket_id),
    check ((body ->> 'revision')::bigint = revision)
);
create table business.orders (
    tenant_id text not null,
    order_id text not null,
    revision bigint not null check (revision > 0),
    body jsonb not null check (octet_length(body::text) <= 2048),
    primary key (tenant_id, order_id),
    check (body ->> 'tenant_id' = tenant_id),
    check (body ->> 'order_id' = order_id),
    check ((body ->> 'revision')::bigint = revision)
);
create table business.deliveries (
    tenant_id text not null,
    delivery_id text not null,
    order_id text not null,
    aggregate_revision bigint not null check (aggregate_revision > 0),
    body jsonb not null check (octet_length(body::text) <= 4096),
    primary key (tenant_id, delivery_id),
    check (body ->> 'tenant_id' = tenant_id),
    check (body ->> 'delivery_id' = delivery_id),
    check (body ->> 'order_id' = order_id),
    check ((body ->> 'aggregate_revision')::bigint = aggregate_revision)
);
create table business.policy_indexes (
    index_id uuid not null,
    tenant_id text not null,
    policy_version text not null,
    corpus_sha256 text not null,
    profile_hash text not null check (profile_hash ~ '^[a-f0-9]{64}$'),
    content_hash text not null check (content_hash ~ '^[a-f0-9]{64}$'),
    profile jsonb not null check (octet_length(profile::text) <= 2048),
    expected_chunks integer not null check (expected_chunks between 1 and 64),
    published_at timestamptz,
    primary key (tenant_id, index_id),
    unique (tenant_id, profile_hash),
    foreign key (tenant_id, policy_version)
    references business.policy_versions (tenant_id, policy_version)
);
create table business.policy_chunks (
    tenant_id text not null,
    index_id uuid not null,
    chunk_id text not null,
    source text not null check (octet_length(source) between 1 and 128),
    body text not null check (octet_length(body) between 1 and 768),
    embedding extensions.vector(384) not null,
    primary key (tenant_id, index_id, chunk_id),
    foreign key (tenant_id, index_id) references business.policy_indexes (tenant_id, index_id),
    check (extensions.vector_norm(embedding) > 0)
);
create table business.snapshots (
    snapshot_id uuid not null,
    tenant_id text not null,
    request_key text not null check (octet_length(request_key) between 1 and 128),
    request_hash text not null check (request_hash ~ '^[a-f0-9]{64}$'),
    index_id uuid not null,
    content_hash text not null check (content_hash ~ '^[a-f0-9]{64}$'),
    body jsonb not null check (octet_length(body::text) <= 8192),
    created_at timestamptz not null default now(),
    primary key (tenant_id, snapshot_id),
    unique (tenant_id, request_key),
    foreign key (tenant_id, index_id) references business.policy_indexes (tenant_id, index_id),
    check (body ->> 'tenant_id' = tenant_id),
    check (body ->> 'snapshot_id' = snapshot_id::text),
    check (body ->> 'content_hash' = content_hash),
    check (body -> 'index' ->> 'index_id' = index_id::text)
);

-- Publication is one-way. Late inserts cannot mutate an already visible index.
create function business.guard_index_publication() returns trigger
language plpgsql set search_path = pg_catalog as $$
begin
    if old.published_at is not null or new.published_at is null
        or new.expected_chunks <> (
            select count(*) from business.policy_chunks
            where tenant_id = old.tenant_id and index_id = old.index_id
        ) then
        raise exception 'invalid policy index publication';
    end if;
    return new;
end
$$;
create trigger policy_index_publication before update on business.policy_indexes
for each row execute function business.guard_index_publication();

create function business.guard_chunk_insert() returns trigger
language plpgsql set search_path = pg_catalog as $$
declare
    published timestamptz;
begin
    select published_at into published from business.policy_indexes
    where tenant_id = new.tenant_id and index_id = new.index_id for update;
    if not found or published is not null then
        raise exception 'policy index is not writable';
    end if;
    return new;
end
$$;
create trigger policy_chunk_insert before insert on business.policy_chunks
for each row execute function business.guard_chunk_insert();

create function business.guard_snapshot() returns trigger
language plpgsql set search_path = pg_catalog as $$
begin
    if tg_op <> 'INSERT' then
        raise exception 'business snapshot is immutable';
    end if;
    if not exists (
        select 1 from business.policy_indexes
        where tenant_id = new.tenant_id and index_id = new.index_id
            and published_at is not null
    ) then
        raise exception 'snapshot requires published policy index';
    end if;
    return new;
end
$$;
create trigger snapshot_immutable before insert or update or delete on business.snapshots
for each row execute function business.guard_snapshot();

-- Changing source facts requires increasing the applicable aggregate revision.
create function business.guard_source_revision() returns trigger
language plpgsql set search_path = pg_catalog as $$
begin
    if new.body is distinct from old.body then
        if tg_table_name = 'deliveries' then
            if new.aggregate_revision <= old.aggregate_revision then
                raise exception 'delivery aggregate revision must increase';
            end if;
        elsif new.revision <= old.revision then
            raise exception 'business revision must increase';
        end if;
    end if;
    return new;
end
$$;
create trigger ticket_revision before update on business.tickets
for each row execute function business.guard_source_revision();
create trigger order_revision before update on business.orders
for each row execute function business.guard_source_revision();
create trigger delivery_revision before update on business.deliveries
for each row execute function business.guard_source_revision();

revoke all on all functions in schema business from public;
reset role;
grant usage on schema business, extensions to jobforge_business_runtime, jobforge_business_loader;
grant select on all tables in schema business
to jobforge_business_runtime, jobforge_business_loader;
grant insert on business.snapshots to jobforge_business_runtime;
grant insert on business.dataset_imports, business.policy_versions, business.tickets,
business.orders, business.deliveries, business.policy_indexes, business.policy_chunks
to jobforge_business_loader;
grant update on business.tickets, business.orders, business.deliveries
to jobforge_business_loader;
grant update (published_at) on business.policy_indexes to jobforge_business_loader;
