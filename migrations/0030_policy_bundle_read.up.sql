-- Register the fixed policy bundle read without changing historical evidence.
-- Takes access exclusive locks and validates three existing CHECK constraints.
-- No data rewrite; roll back only when new-tool evidence is absent, otherwise forward-fix.
-- Verify admission, physical accounting and recovery with real PostgreSQL tests.
alter table tool_invocations
drop constraint tool_invocations_tool_name_check,
add constraint tool_invocations_tool_name_check check (tool_name in (
    'get_order', 'get_delivery', 'search_policy', 'read_policy_bundle'
));

alter table physical_calls
drop constraint physical_calls_step_kind_check,
add constraint physical_calls_step_kind_check check (step_kind in (
    'read_ticket', 'get_order', 'get_delivery', 'search_policy',
    'model_proposal', 'protocol_correction', 'submit_proposal', 'model_decision',
    'read_policy_bundle'
));

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
            'apply_ticket_resolution', 'read_policy_bundle'
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
