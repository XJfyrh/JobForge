-- Batch-bound control evidence, read-only; never mounted into the Worker.
begin transaction isolation level repeatable read read only;
select jsonb_build_object('schema_version', 1, 'sampled_at', clock_timestamp(),
    'batch_account_id', :'batch_id',
    'batch_account', (select to_jsonb(a) from budget_accounts a where a.account_id = :'batch_id'),
    'runs', coalesce(jsonb_agg(jsonb_build_object(
        'run_id', r.run_id, 'tenant_id', r.tenant_id,
        'run', to_jsonb(r), 'business_request_key', b.business_request_key,
        'business_request_id', r.business_request_id,
        'model_calls', (select coalesce(jsonb_agg(to_jsonb(c) order by ordinal), '[]'::jsonb)
            from physical_calls c where c.tenant_id = r.tenant_id and c.run_id = r.run_id),
        'attempts', (select coalesce(jsonb_agg(to_jsonb(a) order by attempt_no), '[]'::jsonb)
            from run_attempts a where a.tenant_id = r.tenant_id and a.run_id = r.run_id),
        'authorization', (select to_jsonb(a) from action_authorizations a
            where a.tenant_id = r.tenant_id and a.business_request_id = r.business_request_id),
        'approval', (select to_jsonb(a) from run_approvals a
            where a.tenant_id = r.tenant_id and a.run_id = r.run_id),
        'receipt_queries', (select coalesce(jsonb_agg(to_jsonb(q) order by reserved_at), '[]'::jsonb)
            from action_receipt_queries q where q.tenant_id = r.tenant_id and q.source_run_id = r.run_id)
    ) order by r.created_at, r.run_id), '[]'::jsonb))
from runs r join business_requests b
    on b.tenant_id = r.tenant_id and b.business_request_id = r.business_request_id
where b.batch_account_id = :'batch_id';
commit;
