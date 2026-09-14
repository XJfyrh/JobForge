# Worker recovery

Workers renew leases with heartbeats. If a Worker crashes, an expired lease is recovered and the task is delivered again. Fencing tokens reject stale completion.

Handlers use a persistent business idempotency key. If a process dies after publishing an artifact but before reporting completion, the next execution reuses that artifact. Re-execution does not resume an interrupted model call.
