-- Purpose: match the published UTF-8/C0/DEL domain contract independent of locale.
-- Lock/data risk: ACCESS EXCLUSIVE during constraint replacement and validation.
-- Existing rows satisfy the weaker explicit C0/DEL rule; no data is rewritten.
-- Rollback: may reject a newly accepted C1 reference; never rewrite it silently.
alter table jobs drop constraint jobs_result_ref_bound;
alter table jobs add constraint jobs_result_ref_bound check (
    result_ref is null or (
        octet_length(result_ref) between 1 and 2048
        and result_ref !~ U&'[\0001-\001F\007F]'
        and state = 'succeeded'
    )
);
