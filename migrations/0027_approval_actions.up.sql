-- S4 child records only; no historical payload or ledger is rewritten.
-- Short metadata locks: stop admission for deployment and use the lock timeout.
-- Down refuses accepted decisions/effects; back up audit records and roll forward.
-- Verify isolated up/down/re-up, tenant FKs, Run/account lock order and PG races.
set local lock_timeout = '5s';

alter table runs add constraint runs_action_binding_unique unique (
    tenant_id, run_id, business_request_id
);
-- First terminal disposition is frozen separately from later family effects.
-- Historical rows remain byte-identical; their absent value uses legacy result facts.
alter table runs add column terminal_disposition text check (
    terminal_disposition in ('none', 'proposal', 'applied', 'rejected', 'unknown', 'no_action')
);
alter table run_operations drop constraint run_operations_operation_kind_check;
alter table run_operations add constraint run_operations_operation_kind_check
check (operation_kind in ('submit', 'cancel', 'retry', 'approval'));

alter table run_approvals drop constraint run_approvals_status_check;
alter table run_approvals add column decision_operation_id uuid;
alter table run_approvals add column actor_id text;
alter table run_approvals add column decided_at timestamptz;
alter table run_approvals add constraint run_approvals_status_check check (
    status in ('pending', 'approved', 'rejected')
);
alter table run_approvals add constraint run_approvals_decision_pair check (
    (
        status = 'pending'
        and decision_operation_id is null
        and actor_id is null
        and decided_at is null
    )
    or (
        status in ('approved', 'rejected') and decision_operation_id is not null
        and actor_id is not null
        and actor_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'
        and decided_at is not null
        and decided_at >= created_at and decided_at < permission_expires_at
    )
);
alter table run_approvals add constraint run_approvals_decision_unique unique (
    tenant_id, decision_operation_id
);

create table action_authorizations (
    tenant_id text not null,
    business_request_id uuid not null,
    authorizing_run_id uuid not null,
    operation_id uuid not null,
    approval_id uuid not null,
    authorization_hash text not null check (authorization_hash ~ '^[0-9a-f]{64}$'),
    action jsonb not null check (
        jsonb_typeof(action) = 'object' and octet_length(action::text) <= 32768
    ),
    query_count integer not null default 0 check (query_count between 0 and 4),
    write_count integer not null default 0 check (write_count between 0 and 4),
    authorized_at timestamptz not null,
    expires_at timestamptz not null check (expires_at > authorized_at),
    primary key (tenant_id, business_request_id),
    unique (tenant_id, operation_id),
    unique (tenant_id, authorizing_run_id, operation_id),
    foreign key (tenant_id, authorizing_run_id, business_request_id)
    references runs (tenant_id, run_id, business_request_id),
    foreign key (tenant_id, approval_id) references run_approvals (
        tenant_id, decision_operation_id
    ),
    check (action -> 'authorization' ->> 'tenant_id' = tenant_id),
    check (action -> 'authorization' ->> 'business_request_id' = business_request_id::text),
    check (action -> 'authorization' ->> 'run_id' = authorizing_run_id::text),
    check (action -> 'authorization' ->> 'operation_id' = operation_id::text),
    check (action -> 'authorization' ->> 'approval_id' = approval_id::text)
);

create table action_calls (
    physical_call_id uuid primary key,
    tenant_id text not null,
    run_id uuid not null,
    operation_id uuid not null,
    authorization_hash text not null check (authorization_hash ~ '^[0-9a-f]{64}$'),
    attempt_no bigint not null,
    worker_id text not null,
    session_id uuid not null,
    fencing_token bigint not null,
    step jsonb not null check (jsonb_typeof(step) = 'object' and octet_length(step::text) <= 2048),
    kind text not null check (kind in ('receipt_query', 'action_write')),
    status text not null check (status in ('reserved', 'unknown', 'observed')),
    reserved_at timestamptz not null,
    dispatch_expires_at timestamptz not null,
    call_deadline timestamptz not null,
    transport_outcome text check (transport_outcome in ('response', 'timeout', 'network_error')),
    observation_hash text check (observation_hash ~ '^[0-9a-f]{64}$'),
    observed_at timestamptz,
    unique (tenant_id, run_id, attempt_no, kind),
    foreign key (tenant_id, run_id, operation_id)
    references action_authorizations (tenant_id, authorizing_run_id, operation_id),
    foreign key (tenant_id, run_id, attempt_no) references run_attempts (
        tenant_id, run_id, attempt_no
    ),
    foreign key (worker_id, session_id) references worker_sessions (worker_id, session_id),
    check (dispatch_expires_at > reserved_at and dispatch_expires_at <= call_deadline),
    check (call_deadline <= reserved_at + interval '10 seconds'),
    check ((transport_outcome is null) = (observation_hash is null)),
    check ((transport_outcome is null) = (observed_at is null)),
    check ((status = 'observed') = (observed_at is not null))
);
create index action_calls_run_idx on action_calls (
    tenant_id, run_id, reserved_at, physical_call_id
);

create table action_receipt_views (
    tenant_id text not null,
    operation_id uuid not null,
    receipt jsonb not null check (
        jsonb_typeof(receipt) = 'object' and octet_length(receipt::text) <= 4096
    ),
    receipt_hash text not null check (receipt_hash ~ '^[0-9a-f]{64}$'),
    effect_version bigint not null default 1 check (effect_version between 1 and 9007199254740991),
    source text not null check (
        source in ('action_response', 'worker_query', 'reconcile', 'retry')
    ),
    observed_at timestamptz not null,
    primary key (tenant_id, operation_id),
    foreign key (tenant_id, operation_id) references action_authorizations (
        tenant_id, operation_id
    ),
    check (receipt ->> 'operation_id' = operation_id::text and receipt ->> 'tenant_id' = tenant_id),
    check (receipt ->> 'receipt_hash' = receipt_hash)
);

create table tenant_receipt_query_gates (
    tenant_id text primary key,
    query_id uuid,
    query_until timestamptz,
    next_allowed_at timestamptz not null,
    check ((query_id is null) = (query_until is null))
);
create table action_receipt_queries (
    query_id uuid primary key,
    tenant_id text not null,
    operation_id uuid not null,
    source_run_id uuid not null,
    source text not null check (source in ('reconcile', 'retry')),
    reserved_at timestamptz not null,
    deadline timestamptz not null,
    outcome text check (outcome in ('found', 'absent', 'unavailable')),
    finished_at timestamptz,
    foreign key (tenant_id, operation_id) references action_authorizations (
        tenant_id, operation_id
    ),
    foreign key (tenant_id, source_run_id) references runs (tenant_id, run_id),
    check (deadline > reserved_at and deadline <= reserved_at + interval '10 seconds'),
    check ((outcome is null) = (finished_at is null))
);

create function guard_action_identity() returns trigger language plpgsql as $$
begin
    if tg_table_name = 'action_authorizations' then
        if row(new.tenant_id, new.business_request_id, new.authorizing_run_id, new.operation_id,
               new.approval_id, new.authorization_hash, new.action, new.authorized_at, new.expires_at)
            is distinct from row(old.tenant_id, old.business_request_id, old.authorizing_run_id,
               old.operation_id, old.approval_id, old.authorization_hash, old.action,
               old.authorized_at, old.expires_at) then
            raise exception 'immutable action identity';
        end if;
        if new.query_count < old.query_count or new.write_count < old.write_count then
            raise exception 'action counters cannot decrease';
        end if;
    else
        raise exception 'immutable action receipt';
    end if;
    return new;
end;
$$;
create trigger action_identity_guard before update on action_authorizations
for each row execute function guard_action_identity();
create trigger action_receipt_guard before update or delete on action_receipt_views
for each row execute function guard_action_identity();

-- Extend the exact S3 closing proof to the registered S4 action.

alter table run_attempts drop constraint run_attempts_recovery_proof_check;
alter table run_attempts
add constraint run_attempts_recovery_proof_check check (
    (recovery_step is null and recovery_ordinal is null)
    or (
        recovery_step is not null and recovery_ordinal is not null
        and recovery_ordinal between 1 and 3
        and finished_at is not null
        and outcome is not null and error_code is not null
        and outcome in ('failed_retry', 'lease_expired_retry')
        and error_code in (
            'LEASE_EXPIRED', 'TIMEOUT', 'DEPENDENCY_UNAVAILABLE',
            'ATTEMPT_DEADLINE_EXCEEDED'
        )
        and jsonb_typeof(recovery_step) = 'object'
        and octet_length(recovery_step::text) <= 2048
        and recovery_step ?& array[
            'step_id', 'sequence', 'kind', 'cursor_version', 'input_hash',
            'profile_id', 'profile_hash', 'snapshot_id', 'snapshot_hash'
        ]
        and recovery_step - array[
            'step_id', 'sequence', 'kind', 'cursor_version', 'input_hash',
            'profile_id', 'profile_hash', 'snapshot_id', 'snapshot_hash'
        ] = '{}'::jsonb
        and jsonb_typeof(recovery_step -> 'sequence') = 'number'
        and jsonb_typeof(recovery_step -> 'cursor_version') = 'number'
        and recovery_step ->> 'sequence' ~ '^([1-9]|[12][0-9]|3[0-2])$'
        and recovery_step ->> 'cursor_version' ~ '^([0-9]|[12][0-9]|3[01])$'
        and (recovery_step ->> 'sequence')::numeric
        = (recovery_step ->> 'cursor_version')::numeric + 1
        and jsonb_typeof(recovery_step -> 'step_id') = 'string'
        and recovery_step ->> 'step_id'
        ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
        and jsonb_typeof(recovery_step -> 'snapshot_id') = 'string'
        and recovery_step ->> 'snapshot_id'
        ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
        and jsonb_typeof(recovery_step -> 'kind') = 'string'
        and recovery_step ->> 'kind' in (
            'read_ticket', 'get_order', 'get_delivery', 'search_policy',
            'model_proposal', 'model_decision', 'protocol_correction', 'submit_proposal',
            'apply_ticket_resolution'
        )
        and jsonb_typeof(recovery_step -> 'profile_id') = 'string'
        and recovery_step ->> 'profile_id' ~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$'
        and jsonb_typeof(recovery_step -> 'input_hash') = 'string'
        and recovery_step ->> 'input_hash' ~ '^[0-9a-f]{64}$'
        and jsonb_typeof(recovery_step -> 'profile_hash') = 'string'
        and recovery_step ->> 'profile_hash' ~ '^[0-9a-f]{64}$'
        and jsonb_typeof(recovery_step -> 'snapshot_hash') = 'string'
        and recovery_step ->> 'snapshot_hash' ~ '^[0-9a-f]{64}$'
    )
);
