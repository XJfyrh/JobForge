-- Isolated diagnosis database only. Additive tables; no production Run changes.
-- Creation locks only these new tables. Preserve evidence; do not roll back after sends.
create table diagnosis_database_identity (
    singleton boolean primary key default true check (singleton),
    database_id uuid not null default gen_random_uuid()
);
insert into diagnosis_database_identity (singleton) values (true);

create table diagnosis_experiments (
    experiment_id text primary key,
    manifest_sha256 text not null check (length(manifest_sha256) = 64),
    manifest jsonb not null,
    prior_cost_microyuan bigint not null check (prior_cost_microyuan >= 0),
    cost_cap_microyuan bigint not null check (cost_cap_microyuan > prior_cost_microyuan),
    stopped_reason text,
    created_at timestamptz not null default clock_timestamp()
);

create table diagnosis_calls (
    experiment_id text not null references diagnosis_experiments (experiment_id),
    trial_id text not null,
    ordinal integer not null check (ordinal > 0),
    batch_account_id uuid not null references budget_accounts (account_id),
    request_sha256 text not null check (length(request_sha256) = 64),
    state text not null check (state in ('reserved', 'known', 'unknown', 'anomaly')),
    reserved_input bigint not null,
    reserved_output bigint not null,
    reserved_cost bigint not null,
    input_tokens bigint,
    output_tokens bigint,
    cached_input_tokens bigint,
    reasoning_tokens bigint,
    known_cost bigint,
    report_sha256 text,
    response_sha256 text,
    outcome text,
    reserved_at timestamptz not null default clock_timestamp(),
    settled_at timestamptz,
    elapsed_ms bigint,
    primary key (experiment_id, trial_id),
    unique (experiment_id, ordinal)
);

create unique index diagnosis_one_active_call
on diagnosis_calls ((true)) where state = 'reserved';
