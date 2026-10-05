-- Receipt-reader metadata: no mutation or replacement of business receipts.
begin transaction isolation level repeatable read read only;
select jsonb_build_object(
    'schema_version', 1, 'observed_at', clock_timestamp(),
    'database', current_database(), 'role', current_user,
    'resolutions', coalesce(jsonb_agg(jsonb_build_object(
        'tenant_id', tenant_id, 'operation_id', operation_id,
        'business_request_id', business_request_id, 'ticket_id', ticket_id,
        'authorization_hash', authorization_hash, 'parameters', parameters,
        'receipt', receipt, 'applied_at', applied_at, 'retain_until', retain_until
    ) order by tenant_id, operation_id), '[]'::jsonb)
) from business.ticket_resolutions;
commit;
