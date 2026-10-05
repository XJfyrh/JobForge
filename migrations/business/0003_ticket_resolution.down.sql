-- Roll back an unused S4 deployment only, retaining marked global roles.
-- Stop all writers first. Existing effects/receipts require forward repair.
-- DDL takes short table/catalog locks. Verify refusal and an empty up/down in PG.
set local lock_timeout = '5s';
do $$
begin
    if exists (select 1 from business.ticket_resolutions) then
        raise exception 'business effects forbid destructive rollback';
    end if;
end
$$;
drop trigger ticket_action_guard on business.tickets;
drop trigger order_action_guard on business.orders;
drop trigger delivery_action_guard on business.deliveries;
drop trigger policy_action_guard on business.policy_versions;
drop trigger index_action_guard on business.policy_indexes;
drop trigger policy_revision on business.policy_versions;
drop table business.ticket_resolutions;
drop function business.guard_action_sources();
drop function business.guard_resolution_immutable();
revoke update (revision, body) on business.tickets from jobforge_business_writer;
revoke select on business.tickets, business.orders, business.deliveries,
business.policy_versions, business.policy_indexes, business.snapshots
from jobforge_business_writer;
