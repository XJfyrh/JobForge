-- psql -X -q -A -t -v ON_ERROR_STOP=1 -v batch_id=<frozen UUID>.
-- Operator-only readonly metadata; never mounted into the Worker.
begin transaction isolation level repeatable read read only;
select jsonb_build_object(
    'schema_version', 1, 'batch_account_id', :'batch_id',
    'sampled_at', clock_timestamp(),
    'runs', coalesce(jsonb_agg(jsonb_build_object(
        'run_id', r.run_id, 'tenant_id', r.tenant_id,
        'profile_id', r.profile_id, 'profile_hash', r.profile_hash,
        'state', r.state, 'created_at', r.created_at, 'updated_at', r.updated_at,
        'attempts', (select coalesce(jsonb_agg(to_jsonb(a) order by a.attempt_no), '[]'::jsonb)
            from run_attempts a where a.tenant_id=r.tenant_id and a.run_id=r.run_id),
        'steps', (select coalesce(jsonb_agg(jsonb_build_object(
            'attempt_no', s.attempt_no, 'sequence', s.sequence, 'step_id', s.step_id,
            'kind', s.kind, 'created_at', s.created_at) order by s.sequence), '[]'::jsonb)
            from run_steps s where s.tenant_id=r.tenant_id and s.run_id=r.run_id)
    ) order by r.created_at, r.run_id), '[]'::jsonb)
)
from runs r join business_requests b
    on b.tenant_id=r.tenant_id and b.business_request_id=r.business_request_id
where b.batch_account_id=:'batch_id';
commit;
