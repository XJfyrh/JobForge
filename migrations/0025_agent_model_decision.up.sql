-- Add the registered S2 model decision to the physical-call binding.
-- Takes a short access exclusive lock and validates existing rows; no data rewrite.
-- Roll back only after S2 call evidence is absent; prefer a forward fix otherwise.
-- Verify old and new step kinds in the real PostgreSQL Run contract suite.
alter table physical_calls
drop constraint physical_calls_step_kind_check,
add constraint physical_calls_step_kind_check check (step_kind in (
    'read_ticket', 'get_order', 'get_delivery', 'search_policy',
    'model_proposal', 'protocol_correction', 'submit_proposal', 'model_decision'
));
