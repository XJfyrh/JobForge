-- Add the v3 single-Run control schema without changing historical jobs.
-- Only new empty tables and indexes are created; no historical rows are rewritten.
-- Rollback drops only these new tables after stopping v3 services. Applied files
-- are immutable; later fixes use a new migration. Validate up/down/re-up on an
-- isolated test database, then exercise tenant FKs, concurrency, and race tests.

create table agent_profiles (
    profile_id text primary key,
    profile_hash text not null check (profile_hash ~ '^[0-9a-f]{64}$'),
    definition jsonb not null check (jsonb_typeof(definition) = 'object'),
    created_at timestamptz not null default clock_timestamp()
);

create table budget_accounts (
    account_id uuid primary key,
    scope text not null check (scope in ('family', 'tenant', 'batch')),
    scope_key text not null,
    currency text not null default 'CNY' check (currency = 'CNY'),
    valid_from timestamptz not null,
    valid_until timestamptz not null,
    frozen boolean not null default false,
    limit_chat bigint not null check (limit_chat between 0 and 9007199254740991),
    used_chat bigint not null default 0 check (used_chat between 0 and 9007199254740991),
    limit_logical_tools bigint not null check (limit_logical_tools between 0 and 9007199254740991),
    used_logical_tools bigint not null default 0 check (
        used_logical_tools between 0 and 9007199254740991
    ),
    limit_query_embedding bigint not null check (
        limit_query_embedding between 0 and 9007199254740991
    ),
    used_query_embedding bigint not null default 0 check (
        used_query_embedding between 0 and 9007199254740991
    ),
    limit_profile_metadata_http bigint not null check (
        limit_profile_metadata_http between 0 and 9007199254740991
    ),
    used_profile_metadata_http bigint not null default 0 check (
        used_profile_metadata_http between 0 and 9007199254740991
    ),
    limit_business_tool_http bigint not null check (
        limit_business_tool_http between 0 and 9007199254740991
    ),
    used_business_tool_http bigint not null default 0 check (
        used_business_tool_http between 0 and 9007199254740991
    ),
    limit_physical_http bigint not null check (limit_physical_http between 0 and 9007199254740991),
    used_physical_http bigint not null default 0 check (
        used_physical_http between 0 and 9007199254740991
    ),
    limit_protocol_corrections bigint not null check (
        limit_protocol_corrections between 0 and 9007199254740991
    ),
    used_protocol_corrections bigint not null default 0 check (
        used_protocol_corrections between 0 and 9007199254740991
    ),
    limit_tokens bigint not null check (limit_tokens between 0 and 9007199254740991),
    used_tokens bigint not null default 0 check (used_tokens between 0 and 9007199254740991),
    limit_cost_microyuan bigint not null check (
        limit_cost_microyuan between 0 and 9007199254740991
    ),
    used_cost_microyuan bigint not null default 0 check (
        used_cost_microyuan between 0 and 9007199254740991
    ),
    known_tokens bigint not null default 0 check (known_tokens >= 0),
    known_cost_microyuan bigint not null default 0 check (known_cost_microyuan >= 0),
    held_tokens bigint not null default 0 check (held_tokens >= 0),
    held_cost_microyuan bigint not null default 0 check (held_cost_microyuan >= 0),
    created_at timestamptz not null default clock_timestamp(),
    unique (scope, scope_key),
    check (valid_until > valid_from),
    check (used_tokens = known_tokens + held_tokens),
    check (used_cost_microyuan = known_cost_microyuan + held_cost_microyuan)
);

create table budget_batch_tenants (
    batch_account_id uuid not null references budget_accounts (account_id),
    tenant_id text not null,
    tenant_account_id uuid not null references budget_accounts (account_id),
    primary key (batch_account_id, tenant_id)
);

create table business_requests (
    business_request_id uuid primary key,
    tenant_id text not null,
    business_request_key text not null,
    request_hash text not null check (request_hash ~ '^[0-9a-f]{64}$'),
    root_run_id uuid not null,
    family_account_id uuid not null unique references budget_accounts (account_id),
    tenant_account_id uuid not null references budget_accounts (account_id),
    batch_account_id uuid not null references budget_accounts (account_id),
    created_at timestamptz not null,
    retry_until timestamptz not null,
    unique (tenant_id, business_request_key),
    unique (tenant_id, business_request_id),
    check (retry_until = created_at + interval '7 days')
);

create table worker_sessions (
    session_id uuid primary key,
    worker_id text not null,
    startup_id uuid not null,
    version text not null,
    created_at timestamptz not null,
    seen_at timestamptz not null,
    expires_at timestamptz not null,
    unique (worker_id, startup_id),
    unique (worker_id, session_id),
    check (expires_at > seen_at)
);

create table execution_slots (
    resource_kind text not null check (resource_kind in ('worker', 'tenant', 'profile')),
    resource_id text not null,
    capacity integer not null check (capacity between 1 and 100),
    used integer not null default 0 check (used >= 0),
    primary key (resource_kind, resource_id),
    check (used <= capacity)
);

create table runs (
    run_id uuid primary key,
    tenant_id text not null,
    business_request_id uuid not null,
    business_request_key text not null,
    ticket_id text not null,
    retry_of_run_id uuid unique,
    admission_hash text not null check (admission_hash ~ '^[0-9a-f]{64}$'),
    profile_id text not null references agent_profiles (profile_id),
    profile_hash text not null check (profile_hash ~ '^[0-9a-f]{64}$'),
    budget_batch_id text not null,
    snapshot_id uuid not null,
    snapshot_hash text not null check (snapshot_hash ~ '^[0-9a-f]{64}$'),
    version_vector jsonb not null check (jsonb_typeof(version_vector) = 'object'),
    ticket_binding jsonb not null check (jsonb_typeof(ticket_binding) = 'object'),
    index_id uuid not null,
    index_profile_hash text not null check (index_profile_hash ~ '^[0-9a-f]{64}$'),
    state text not null check (state in (
        'ready', 'running', 'stopping', 'retry_wait',
        'awaiting_approval', 'succeeded', 'failed', 'cancelled'
    )),
    outcome text check (outcome in ('no_action', 'applied', 'rejected')),
    error_code text,
    error_message text,
    attempt_no bigint not null default 0 check (attempt_no between 0 and 9007199254740991),
    recovery_count bigint not null default 0 check (recovery_count between 0 and 3),
    cursor_version bigint not null default 0 check (cursor_version between 0 and 32),
    run_timeout_seconds bigint not null check (run_timeout_seconds between 1 and 86400),
    run_deadline timestamptz not null,
    attempt_deadline timestamptz,
    lease_until timestamptz,
    next_attempt_at timestamptz,
    permission_expires_at timestamptz,
    proposal_ref text,
    result_kind text check (result_kind in ('proposal', 'no_action', 'final')),
    result_ref text,
    stop_reason text check (stop_reason in ('cancel', 'run_deadline', 'attempt_timeout')),
    cancel_requested_at timestamptz,
    worker_id text,
    session_id uuid,
    fencing_token bigint not null default 0 check (fencing_token between 0 and 9007199254740991),
    active_call_id uuid,
    checkpoint_bytes bigint not null default 0 check (checkpoint_bytes between 0 and 262144),
    event_sequence bigint not null default 0 check (event_sequence between 0 and 9007199254740991),
    next_step_id uuid not null,
    next_step_kind text not null,
    next_input_hash text not null check (next_input_hash ~ '^[0-9a-f]{64}$'),
    created_at timestamptz not null,
    updated_at timestamptz not null,
    unique (tenant_id, run_id),
    foreign key (tenant_id, business_request_id)
    references business_requests (tenant_id, business_request_id),
    foreign key (tenant_id, retry_of_run_id) references runs (tenant_id, run_id),
    foreign key (worker_id, session_id) references worker_sessions (worker_id, session_id),
    check ((error_code is null) = (error_message is null)),
    check ((result_kind is null) = (result_ref is null)),
    check ((state in ('running', 'stopping')) = (worker_id is not null)),
    check ((state in ('running', 'stopping')) = (session_id is not null)),
    check ((state in ('running', 'stopping')) = (lease_until is not null)),
    check ((state in ('running', 'stopping')) = (attempt_deadline is not null)),
    check (lease_until is null or lease_until <= attempt_deadline),
    check (attempt_deadline is null or attempt_deadline <= run_deadline),
    check (run_deadline > created_at)
);

alter table business_requests add constraint business_requests_root_run_fk
foreign key (tenant_id, root_run_id) references runs (tenant_id, run_id)
deferrable initially deferred;

create table run_operations (
    operation_id uuid primary key,
    tenant_id text not null,
    operation_kind text not null check (operation_kind in ('submit', 'cancel', 'retry')),
    source_run_id uuid,
    operation_scope text not null,
    operation_key text not null,
    request_hash text not null check (request_hash ~ '^[0-9a-f]{64}$'),
    result_run_id uuid not null,
    created_at timestamptz not null,
    unique (tenant_id, operation_scope, operation_key),
    foreign key (tenant_id, source_run_id) references runs (tenant_id, run_id),
    foreign key (tenant_id, result_run_id) references runs (tenant_id, run_id),
    check ((operation_kind = 'submit') = (source_run_id is null))
);

create table run_attempts (
    tenant_id text not null,
    run_id uuid not null,
    attempt_no bigint not null,
    worker_id text not null,
    session_id uuid not null,
    fencing_token bigint not null,
    started_at timestamptz not null,
    deadline timestamptz not null,
    finished_at timestamptz,
    outcome text,
    error_code text,
    primary key (tenant_id, run_id, attempt_no),
    foreign key (tenant_id, run_id) references runs (tenant_id, run_id),
    foreign key (worker_id, session_id) references worker_sessions (worker_id, session_id),
    check ((finished_at is null) = (outcome is null))
);

create table run_steps (
    tenant_id text not null,
    run_id uuid not null,
    attempt_no bigint not null,
    sequence bigint not null check (sequence between 1 and 32),
    step_id uuid not null,
    kind text not null,
    input_hash text not null,
    profile_hash text not null,
    snapshot_hash text not null,
    commit_hash text not null,
    output_ref text not null,
    output jsonb not null,
    output_bytes integer not null check (output_bytes between 1 and 16384),
    cursor_version bigint not null,
    created_at timestamptz not null,
    primary key (tenant_id, run_id, sequence),
    unique (tenant_id, run_id, step_id),
    foreign key (tenant_id, run_id) references runs (tenant_id, run_id),
    foreign key (tenant_id, run_id, attempt_no)
    references run_attempts (tenant_id, run_id, attempt_no)
);

create table run_events (
    tenant_id text not null,
    run_id uuid not null,
    sequence bigint not null check (sequence > 0),
    event_type text not null,
    state text not null,
    attempt_no bigint not null,
    cursor_version bigint not null,
    created_at timestamptz not null,
    primary key (tenant_id, run_id, sequence),
    foreign key (tenant_id, run_id) references runs (tenant_id, run_id)
);

create table run_approvals (
    tenant_id text not null,
    run_id uuid not null,
    status text not null check (status = 'pending'),
    proposal_hash text not null,
    proposal_ref text not null,
    snapshot_id uuid not null,
    snapshot_hash text not null,
    version_vector jsonb not null,
    permission_expires_at timestamptz not null,
    created_at timestamptz not null,
    primary key (tenant_id, run_id),
    foreign key (tenant_id, run_id) references runs (tenant_id, run_id)
);

create table tool_invocations (
    invocation_id uuid primary key,
    tenant_id text not null,
    run_id uuid not null,
    step_id uuid not null,
    attempt_no bigint not null,
    fencing_token bigint not null,
    tool_name text not null check (tool_name in ('get_order', 'get_delivery', 'search_policy')),
    input_hash text not null,
    created_at timestamptz not null,
    unique (tenant_id, run_id, invocation_id),
    foreign key (tenant_id, run_id, attempt_no)
    references run_attempts (tenant_id, run_id, attempt_no)
);

create table physical_calls (
    physical_call_id uuid primary key,
    tenant_id text not null,
    run_id uuid not null,
    step_id uuid not null,
    step_kind text not null check (step_kind in (
        'read_ticket', 'get_order', 'get_delivery', 'search_policy',
        'model_proposal', 'protocol_correction', 'submit_proposal'
    )),
    attempt_no bigint not null,
    worker_id text not null,
    session_id uuid not null,
    fencing_token bigint not null,
    tool_invocation_id uuid,
    kind text not null check (kind in (
        'chat', 'query_embedding', 'profile_metadata_http', 'business_tool_http'
    )),
    subcall text not null,
    ordinal integer not null check (ordinal between 1 and 44),
    input_hash text not null,
    profile_hash text not null,
    price_hash text not null,
    reserved_tokens bigint not null check (reserved_tokens between 0 and 9007199254740991),
    reserved_cost_microyuan bigint not null
    check (reserved_cost_microyuan between 0 and 9007199254740991),
    status text not null check (status in ('reserved', 'unknown', 'known')),
    transport_outcome text,
    business_outcome text,
    observation_hash text,
    usage_hash text,
    usage jsonb,
    known_tokens bigint check (known_tokens is null or known_tokens between 0 and 9007199254740991),
    known_cost_microyuan bigint check (
        known_cost_microyuan is null or known_cost_microyuan between 0 and 9007199254740991
    ),
    measurement_anomaly boolean not null default false,
    reserved_at timestamptz not null,
    dispatch_expires_at timestamptz not null,
    call_deadline timestamptz not null,
    observed_at timestamptz,
    settled_at timestamptz,
    unique (tenant_id, run_id, physical_call_id),
    foreign key (tenant_id, run_id, attempt_no)
    references run_attempts (tenant_id, run_id, attempt_no),
    foreign key (worker_id, session_id) references worker_sessions (worker_id, session_id),
    foreign key (tenant_id, run_id, tool_invocation_id)
    references tool_invocations (tenant_id, run_id, invocation_id),
    check (
        (
            status = 'known' and usage_hash is not null and settled_at is not null
            and not measurement_anomaly
        )
        or (
            status in ('reserved', 'unknown') and settled_at is null
            and (
                (not measurement_anomaly and usage_hash is null)
                or (status = 'unknown' and measurement_anomaly and usage_hash is not null)
            )
        )
    ),
    check (dispatch_expires_at > reserved_at),
    check (call_deadline >= dispatch_expires_at)
);

alter table runs add constraint runs_active_call_fk
foreign key (tenant_id, run_id, active_call_id)
references physical_calls (tenant_id, run_id, physical_call_id)
deferrable initially deferred;

create index runs_ready_claim_idx on runs (profile_id, created_at, run_id)
where state = 'ready';
create index runs_active_lease_idx on runs (lease_until, run_id)
where state in ('running', 'stopping');
create index runs_deadline_idx on runs (run_deadline, run_id)
where state not in ('succeeded', 'failed', 'cancelled');
create index runs_retry_wait_idx on runs (next_attempt_at, run_id)
where state = 'retry_wait';
create index runs_approval_expiry_idx on runs (permission_expires_at, run_id)
where state = 'awaiting_approval';
create index runs_tenant_list_idx on runs (tenant_id, created_at desc, run_id desc);
create index physical_calls_run_idx on physical_calls (tenant_id, run_id, reserved_at);
create index worker_sessions_principal_idx on worker_sessions (worker_id, expires_at desc);
