-- Rollback is safe only before any protected content has been purged.
-- Stop services and use a short lock timeout; never reconstruct fake results.
-- Verify isolated rollback/re-up and refusal after an actual cleanup.
set local lock_timeout = '5s';
do $$
begin
    if exists (select 1 from runs where content_purged_at is not null) then
        raise exception 'purged content requires a backup or forward migration';
    end if;
end;
$$;
drop index runs_content_retention_idx;
drop trigger expired_step_content on run_steps;
drop function guard_expired_step_content();
drop trigger run_terminal_time on runs;
drop function set_run_terminal_time();
alter table run_steps alter column output set not null;
alter table runs drop constraint runs_ticket_content;
alter table runs alter column ticket_binding set not null;
alter table runs drop constraint runs_content_retention;
alter table runs drop constraint runs_terminal_time;
alter table runs drop column content_purged_at;
alter table runs drop column terminal_at;
