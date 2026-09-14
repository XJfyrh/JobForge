-- Purpose: persist bounded artifact metadata atomically with job completion.
-- Lock/data risk: brief ACCESS EXCLUSIVE lock; nullable column, no backfill.
-- Rollback: 0020 down drops references only; business artifacts remain intact.
-- Verify: up/down/up, UTF-8 byte boundary, null and completion race tests.
alter table jobs add column result_ref text;
alter table jobs add constraint jobs_result_ref_bound check (
    result_ref is null or (
        octet_length(result_ref) between 1 and 2048
        and result_ref !~ '[[:cntrl:]]'
        and state = 'succeeded'
    )
);
