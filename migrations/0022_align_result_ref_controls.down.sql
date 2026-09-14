-- Restores 0020's locale-dependent check. A C1 reference can block downgrade;
-- inspect the affected business reference rather than deleting task metadata.
alter table jobs drop constraint jobs_result_ref_bound;
alter table jobs add constraint jobs_result_ref_bound check (
    result_ref is null or (
        octet_length(result_ref) between 1 and 2048
        and result_ref !~ '[[:cntrl:]]'
        and state = 'succeeded'
    )
);
