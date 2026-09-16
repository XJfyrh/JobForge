-- Roll back only v3 control tables in an isolated/rebuildable environment.
-- Stop v3 services first. This destroys their Runs, checkpoints, and budget audit;
-- production removal requires an export and an explicit retention decision.
-- Existing jobs and the separate business database are not affected.
-- Validate by up/down/re-up and by confirming the historical jobs schema remains.
alter table runs drop constraint runs_active_call_fk;
alter table business_requests drop constraint business_requests_root_run_fk;
drop table physical_calls;
drop table tool_invocations;
drop table run_approvals;
drop table run_events;
drop table run_steps;
drop table run_attempts;
drop table run_operations;
drop table runs;
drop table worker_sessions;
drop table execution_slots;
drop table business_requests;
drop table budget_batch_tenants;
drop table budget_accounts;
drop table agent_profiles;
