-- Remove only unused S3 proof columns under a short access exclusive lock.
-- Refuse evidence loss; after S3 execution use a forward migration instead.
-- Verify refusal with a recorded proof and down/reapply with legacy-null rows.
set local lock_timeout = '2s';

lock table run_attempts in access exclusive mode;

do $$
begin
    if exists (select 1 from run_attempts where recovery_step is not null) then
        raise exception 'S3 recovery evidence exists; use a forward migration';
    end if;
end
$$;

alter table run_attempts
drop constraint run_attempts_recovery_proof_check,
drop column recovery_step,
drop column recovery_ordinal;
