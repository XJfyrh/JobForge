-- The marker and restricted global roles survive an ordinary schema rollback.
-- Removing them requires explicit isolated-instance decommissioning.
-- No data is changed; verify Down stops at version 1 in real PG.
do $$
begin
    raise exception 'business identity cannot be rolled back automatically';
end
$$;
