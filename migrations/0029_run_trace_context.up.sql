set local lock_timeout = '5s';

-- Opaque, bounded correlation metadata; never part of business identity.
alter table runs add column trace_context text not null default '';
alter table runs add constraint runs_trace_context_check check (
    trace_context = '' or (
        trace_context ~ '^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$'
        and substring(trace_context from 4 for 32) <> '00000000000000000000000000000000'
        and substring(trace_context from 37 for 16) <> '0000000000000000'
    )
);
