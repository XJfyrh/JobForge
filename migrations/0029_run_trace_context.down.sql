set local lock_timeout = '5s';
alter table runs drop column trace_context;
