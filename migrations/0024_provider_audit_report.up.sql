-- Add immutable original-call reports and batch-only stop reasons (ADR-0020).
-- Nullable additions preserve legacy rows without inventing response evidence.
-- ALTER TABLE briefly takes ACCESS EXCLUSIVE; no data backfill or financial
-- rewrite occurs. Run this before deploying the new audited runtime/profile.
-- Validate up/down/re-up on an isolated database and real report/guard races.

alter table physical_calls
add column execution_binding_hash text,
add column report_hash text,
add column call_report jsonb,
add column report_recorded_at timestamptz,
add column report_conflict_hash text,
add column observation_http_status integer,
add column observation_error_code text,
add constraint physical_calls_execution_binding_hash_check check (
    execution_binding_hash is null or execution_binding_hash ~ '^[0-9a-f]{64}$'
),
add constraint physical_calls_report_check check (
    (
        report_hash is null and call_report is null and report_recorded_at is null
        and report_conflict_hash is null
    )
    or (
        execution_binding_hash is not null
        and report_hash is not null and report_hash ~ '^[0-9a-f]{64}$'
        and call_report is not null and report_recorded_at is not null
        and jsonb_typeof(call_report) = 'object'
        and call_report ?& array['usage', 'provider_audit']
        and call_report - 'usage' - 'provider_audit' = '{}'::jsonb
        and octet_length(call_report::text) <= 8192
        and jsonb_typeof(call_report -> 'usage') in ('object', 'null')
        and jsonb_typeof(call_report -> 'provider_audit') in ('object', 'null')
        and (
            jsonb_typeof(call_report -> 'usage') = 'object'
            or jsonb_typeof(call_report -> 'provider_audit') = 'object'
        )
        and (
            call_report -> 'provider_audit' = 'null'::jsonb
            or (
                jsonb_typeof(call_report -> 'provider_audit') = 'object'
                and octet_length((call_report -> 'provider_audit')::text) <= 2048
            )
        )
        and (
            report_conflict_hash is null
            or (
                report_conflict_hash ~ '^[0-9a-f]{64}$'
                and report_conflict_hash <> report_hash
            )
        )
    )
),
add constraint physical_calls_observation_detail_check check (
    (observation_http_status is null and observation_error_code is null)
    or (
        observation_hash is not null and observation_error_code is not null
        and observation_http_status is not null
        and (
            (transport_outcome = 'unknown' and observation_http_status = 0)
            or (
                transport_outcome = 'response'
                and observation_http_status between 100 and 599
            )
        )
    )
);

alter table budget_accounts
add column batch_stop_code text,
add constraint budget_accounts_batch_stop_code_check check (
    batch_stop_code is null
    or (
        scope = 'batch' and frozen
        and batch_stop_code in (
            'MEASUREMENT_ANOMALY', 'PROVIDER_HTTP_REJECTED',
            'PROVIDER_IDENTITY_INVALID', 'PROVIDER_MODE_INVALID',
            'CHAT_USAGE_UNKNOWN', 'REPORT_CONFLICT'
        )
    )
);

create index business_requests_batch_audit_idx
on business_requests (batch_account_id, tenant_id, business_request_id);

create index physical_calls_chat_guard_idx
on physical_calls (tenant_id, run_id, ordinal)
where kind = 'chat';
