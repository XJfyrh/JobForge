# Fixed Linux process supervision

This package owns only the installed `jobforge_agent.guardian`, its fixed step
child, the process group, and bounded ordinary/metering pipes. `runworker` owns
the original Conversation, authority, RPCs, error precedence, and commit barrier.
No exit code or EOF alone proves a successful step.

`Start` requires a deadline and always uses
`/usr/local/bin/python -I -u -m jobforge_agent.guardian`. FD 3 is the guardian's
parent-liveness reader; only FDs 0/1/2/4/5 reach the step. The environment is built
from fixed names and the already selected deployment credentials. Ambient
tokens, DSNs, proxies and Python paths are not inherited.

Drain `Events` until it closes, then inspect `Wait`. The event channel holds
eight events; each writer has one slot and rejects another concurrent call.
Frames and diagnostics always use fixed classifications, never rejected bytes.
`ChannelReceipt.Joined` requires both its reader and writer to have returned.
Clean EOF is separate from Join. A decoder failure ends only that reader; the
other lane can still drain original complete reports.

Only EPIPE on the metering ACK writer is recorded as `MeteringWriteClosed` and
returned as `ErrMeteringClosed`. It is an output fact: a later input/decoder
failure replaces it. The coordinator immediately closes ordinary execution and
allows at most 100 ms for natural exit before Stop. It may preserve a known
nonzero exit classification only after actual Wait, both clean EOFs, all Joins
and group disappearance. This fact never permits an exit-zero Commit or masks
input, size, other pipe, or cleanup failures.

`Stop` is irreversible and nonblocking. Its independent lifecycle owner sends
TERM, allows 100 ms, then kills the whole group. After KILL, actual Wait, group
disappearance, event delivery and I/O Join share a two-second local deadline.
Natural guardian exit also begins bounded cleanup. A surviving child is recorded
as a pipe/supervision failure even if subsequent cleanup removes it. Zombies
still count as a remaining group; only `kill(-pgid, 0) == ESRCH` proves it gone.

When event consumption stops, cleanup still kills the process. At the cleanup
deadline it abandons undelivered events, closes local FDs and publishes the
actual facts. This is a failed, potentially incomplete cleanup receipt, never
fabricated EOF or Join. The Worker must exit and cannot start another step when
the receipt lacks required cleanup facts or reports a timeout.

## Verification

Native non-Linux tests prove only explicit unsupported behavior and portable
environment/direction checks. Actual acceptance requires the fixed Linux image
with init, `JOBFORGE_RUNEXECUTOR_PROCESS_TESTS=1`, and the package directory as
the working directory. The root image in `tools/agentruntimecheck/Dockerfile`
builds the test binary with `-race` and installs the same Python package used by
the fixed production command.

`testdata/peer.py` is an adversarial OS fixture used only from `_test.go`. Its
modes cover malformed/oversized frames, all physical exit facts, late stderr,
backpressure, and residual child groups. It is not a registered business adapter
or a production command selector. The `TestFixed*` cases exercise the installed
guardian/step itself, FD inheritance, guardian death, and actual Go-parent
SIGKILL/liveness EOF. These process tests make no PG, gRPC, business HTTP or
real-model acceptance claim.
