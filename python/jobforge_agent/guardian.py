"""Supervise only the fixed step and its independent parent-liveness pipe."""

from __future__ import annotations

import os
import select
import signal
import subprocess
import sys
import time

SUPERVISION_FAILURE = 72


def main() -> int:
    """Relay actual child exits; parent loss always terminates the entire group."""
    sys.dont_write_bytecode = True
    if sys.platform != "linux":
        return SUPERVISION_FAILURE
    stopping = False

    def stop(_signum: int, _frame: object) -> None:
        nonlocal stopping
        stopping = True

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    child: subprocess.Popen[bytes] | None = None
    try:
        os.set_blocking(3, False)
        child = subprocess.Popen(
            [sys.executable, "-I", "-u", "-m", "jobforge_agent.step"],
            stdin=0,
            stdout=1,
            stderr=2,
            close_fds=True,
            pass_fds=(4, 5),
        )
        # Holding these copies would hide EOF and can deadlock Go's commit barrier.
        for descriptor in (0, 1, 4, 5):
            os.close(descriptor)
        stop_at: float | None = None
        while True:
            readable, _, _ = select.select([3], [], [], 0.01)
            if readable and os.read(3, 1) == b"":
                stopping = True
            if stopping and stop_at is None:
                stop_at = time.monotonic()
                os.killpg(os.getpgrp(), signal.SIGTERM)
            if stop_at is not None:
                # This intentionally includes the guardian. No asyncio task or
                # full output pipe can postpone the parent's 100ms kill bound.
                if time.monotonic() - stop_at >= 0.1:
                    os.killpg(os.getpgrp(), signal.SIGKILL)
                child.poll()
                continue
            code = child.poll()
            if code is not None:
                return code if code == 0 or 64 <= code <= 71 else SUPERVISION_FAILURE
    except (OSError, ValueError, subprocess.SubprocessError):
        if child is not None:
            # Failure after spawn is still a lifecycle event, never provider data.
            os.killpg(os.getpgrp(), signal.SIGKILL)
        return SUPERVISION_FAILURE


if __name__ == "__main__":
    raise SystemExit(main())
