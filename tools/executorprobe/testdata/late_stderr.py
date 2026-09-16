"""Fixed Linux test helper: emit excess stderr only after Go observes stdout EOF."""

import json
import os
import signal
import sys


def main() -> None:
    """Synchronize with the test's EOF callback using a blocked POSIX signal."""
    signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGUSR1})
    request = json.loads(sys.stdin.buffer.readline(4096))
    print(
        json.dumps(
            {"v": 1, "id": request["id"], "kind": "started", "pid": os.getpid()}
        ),
        flush=True,
    )
    print(
        json.dumps(
            {"v": 1, "id": request["id"], "kind": "result", "value": request["value"]}
        ),
        flush=True,
    )
    os.close(sys.stdout.fileno())
    signal.sigwait({signal.SIGUSR1})
    sys.stderr.write("x" * 8193)
    sys.stderr.flush()
    os._exit(0)


if __name__ == "__main__":
    main()
