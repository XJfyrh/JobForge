-- Restore the S1 kind set with the same short access exclusive lock.
-- This atomic statement fails without changing the constraint if S2 rows exist.
-- Never delete audit evidence to force downgrade; use a forward fix instead.
-- Verify rejection with S2 rows and round trip in an empty test database.
alter table physical_calls
drop constraint physical_calls_step_kind_check,
add constraint physical_calls_step_kind_check check (step_kind in (
    'read_ticket', 'get_order', 'get_delivery', 'search_policy',
    'model_proposal', 'protocol_correction', 'submit_proposal'
));
