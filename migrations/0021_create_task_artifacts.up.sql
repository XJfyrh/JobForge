-- Independent business storage for the two reference adapters (ADR-0011).
-- No foreign key to jobs: business identity survives redelivery/manual retry.
create table task_artifacts (
    tenant_id text not null,
    task_type text not null,
    business_key text not null,
    result_ref text not null unique,
    fingerprint text not null,
    body jsonb not null,
    published_at timestamptz not null default now(),
    primary key (tenant_id, task_type, business_key),
    constraint task_artifacts_key_length check (
        octet_length(business_key) between 1 and 128
    ),
    constraint task_artifacts_ref_format check (
        result_ref ~ '^jobforge-artifact:[a-f0-9]{64}$'
    ),
    constraint task_artifacts_fingerprint_format check (
        fingerprint ~ '^[a-f0-9]{64}$'
    ),
    constraint task_artifacts_body_bound check (
        octet_length(body::text) <= 2097152
    )
);
