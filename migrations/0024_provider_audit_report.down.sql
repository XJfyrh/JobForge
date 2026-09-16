-- Removes report evidence only after stopping audited workers and exporting it.
-- This rollback is for isolated/rebuildable databases, not a production thaw or
-- a way to discard unknown holds. Financial counters and frozen flags remain.
-- Validate re-up and legacy usage-only late settlement on an isolated database.

drop index physical_calls_chat_guard_idx;
drop index business_requests_batch_audit_idx;

alter table budget_accounts
drop constraint budget_accounts_batch_stop_code_check,
drop column batch_stop_code;

alter table physical_calls
drop constraint physical_calls_observation_detail_check,
drop constraint physical_calls_report_check,
drop constraint physical_calls_execution_binding_hash_check,
drop column observation_error_code,
drop column observation_http_status,
drop column report_conflict_hash,
drop column report_recorded_at,
drop column call_report,
drop column report_hash,
drop column execution_binding_hash;
