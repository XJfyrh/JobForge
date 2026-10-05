-- Refuse rollback with accepted S4 decisions or audit; stop all services first.
set local lock_timeout = '5s';
do $$
begin
    if exists (select 1 from action_authorizations)
        or exists (select 1 from run_approvals where status <> 'pending') then
        raise exception 'S4 audit exists: restore backup or roll forward';
    end if;
end;
$$;
drop table action_receipt_queries;
drop table tenant_receipt_query_gates;
drop table action_receipt_views;
drop table action_calls;
drop table action_authorizations;
drop function guard_action_identity();
alter table run_approvals drop constraint run_approvals_decision_unique;
alter table run_approvals drop constraint run_approvals_decision_pair;
alter table run_approvals drop constraint run_approvals_status_check;
alter table run_approvals drop column decision_operation_id;
alter table run_approvals drop column actor_id;
alter table run_approvals drop column decided_at;
alter table run_approvals add constraint run_approvals_status_check check (status = 'pending');
alter table run_operations drop constraint run_operations_operation_kind_check;
alter table run_operations add constraint run_operations_operation_kind_check
check (operation_kind in ('submit', 'cancel', 'retry'));
alter table runs drop constraint runs_action_binding_unique;
alter table runs drop column terminal_disposition;

-- Accepted decisions are refused above; no S4 proof is erased.

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
            'model_proposal', 'model_decision', 'protocol_correction', 'submit_proposal'
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
