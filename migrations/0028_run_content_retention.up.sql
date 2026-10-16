-- Separate expired protected content from permanent identities and ledgers.
-- Stop admission for deployment; short metadata locks and a conservative
-- timestamp backfill retain existing terminal content for another seven days.
-- No account, call, signature, receipt, or foreign key is deleted. Down refuses
-- purged content; restore protected backups or roll forward instead.
-- Verify isolated up/down/re-up, terminal transitions, late audit and PG races.
set local lock_timeout = '5s';

alter table runs add column terminal_at timestamptz;
alter table runs add column content_purged_at timestamptz;
update runs set terminal_at = greatest(updated_at, clock_timestamp())
where state in ('succeeded', 'failed', 'cancelled');
alter table runs add constraint runs_terminal_time check (
    (state in ('succeeded', 'failed', 'cancelled')) = (terminal_at is not null)
);
alter table runs add constraint runs_content_retention check (
    content_purged_at is null or (
        state in ('succeeded', 'failed', 'cancelled')
        and content_purged_at >= terminal_at + interval '7 days'
    )
);
alter table runs alter column ticket_binding drop not null;
alter table runs add constraint runs_ticket_content check (
    (ticket_binding is null) = (content_purged_at is not null)
);
alter table run_steps alter column output drop not null;

create function set_run_terminal_time() returns trigger language plpgsql as $$
begin
    if new.state in ('succeeded', 'failed', 'cancelled') then
        if new.terminal_at is null then
            new.terminal_at := clock_timestamp();
        end if;
    end if;
    return new;
end;
$$;
create trigger run_terminal_time before insert or update of state on runs
for each row execute function set_run_terminal_time();

create function guard_expired_step_content() returns trigger language plpgsql as $$
begin
    if new.output is null and not exists (
        select 1 from runs where tenant_id = new.tenant_id and run_id = new.run_id
            and content_purged_at is not null
    ) then
        raise exception 'step content requires an expired parent';
    end if;
    if tg_op = 'UPDATE' and old.output is null and new.output is not null then
        raise exception 'expired content cannot be reconstructed';
    end if;
    return new;
end;
$$;
create trigger expired_step_content before insert or update of output on run_steps
for each row execute function guard_expired_step_content();
create index runs_content_retention_idx on runs (terminal_at, run_id)
where content_purged_at is null and state in ('succeeded', 'failed', 'cancelled');
