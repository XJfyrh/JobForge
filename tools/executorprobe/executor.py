"""Fixed, Linux-only S0 executor guardian; this is not a production protocol."""

import json
import os
import selectors
import signal
import subprocess
import sys
import time
from typing import NoReturn

MAX_REQUEST = 4096
MAX_RESPONSE = 16384
OPERATIONS = frozenset(
    {
        "echo",
        "block",
        "ignore_term",
        "bad_json",
        "wrong_id",
        "oversize",
        "stderr",
        "trailing_bytes",
    }
)


def terminate_group() -> NoReturn:
    """Kill the dedicated executor group, including this guardian."""
    os.killpg(os.getpgrp(), signal.SIGKILL)
    raise SystemExit(70)


def emit(value: dict[str, object]) -> None:
    """Write one small metadata or result frame."""
    sys.stdout.write(json.dumps(value, separators=(",", ":")) + "\n")
    sys.stdout.flush()


def read_request() -> dict[str, object]:
    """Reject malformed or oversized requests before starting a child."""
    raw = sys.stdin.buffer.readline(MAX_REQUEST + 1)
    if not raw.endswith(b"\n") or len(raw) > MAX_REQUEST:
        raise ValueError("invalid request frame")
    value = json.loads(raw)
    if not isinstance(value, dict) or set(value) != {"v", "id", "op", "value"}:
        raise ValueError("invalid request shape")
    if type(value["v"]) is not int or value["v"] != 1:
        raise ValueError("unsupported protocol")
    if not isinstance(value["id"], str) or not 1 <= len(value["id"]) <= 64:
        raise ValueError("invalid request identity")
    if not isinstance(value["op"], str) or value["op"] not in OPERATIONS:
        raise ValueError("unregistered operation")
    if not isinstance(value["value"], str):
        raise ValueError("invalid value")
    return value


def step(operation: str) -> None:
    """Run only fixed probe operations, never commands supplied by a payload."""
    request = read_request()
    if operation != request["op"]:
        raise ValueError("operation mismatch")
    if operation == "ignore_term":
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
    # Startup and interpreter imports are separate from the active step timer.
    print("READY", flush=True)
    if operation in {"block", "ignore_term"}:
        time.sleep(30)
    elif operation == "bad_json":
        print("not json", flush=True)
        return
    elif operation == "oversize":
        print("x" * (MAX_RESPONSE + 1), flush=True)
        return
    elif operation == "stderr":
        sys.stderr.write("synthetic diagnostic\n" * 1024)
        sys.stderr.flush()
    emit(
        {
            "v": 1,
            "id": "wrong" if operation == "wrong_id" else request["id"],
            "kind": "result",
            "value": request["value"],
        }
    )
    if operation == "trailing_bytes":
        sys.stdout.write("invalid trailing bytes")
        sys.stdout.flush()


def guard() -> None:
    """Watch control EOF separately from a potentially blocked business process."""
    if os.getpgrp() != os.getpid():
        raise ValueError("guardian requires its own process group")
    request = read_request()
    # Only this constant program and a prevalidated operation reach execve.
    child = subprocess.Popen(
        [sys.executable, "-I", "-u", __file__, "--step", str(request["op"])],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        close_fds=True,
    )
    assert child.stdin is not None and child.stdout is not None
    try:
        child.stdin.write(
            json.dumps(request, ensure_ascii=False, separators=(",", ":")).encode()
            + b"\n"
        )
        child.stdin.close()
        with selectors.DefaultSelector() as selector:
            selector.register(sys.stdin.buffer, selectors.EVENT_READ, "control")
            selector.register(child.stdout, selectors.EVENT_READ, "result")
            output = bytearray()
            started = False
            while True:
                for key, _ in selector.select(timeout=1):
                    if key.data == "control":
                        # EOF means the Go owner died; any second request is invalid.
                        terminate_group()
                    chunk = os.read(child.stdout.fileno(), 4096)
                    if not chunk:
                        return_code = child.wait(timeout=1)
                        if output:
                            raise ValueError("unterminated response frame")
                        if return_code != 0:
                            raise ValueError("step process failed")
                        return
                    output.extend(chunk)
                    if not started and output.startswith(b"READY\n"):
                        del output[:6]
                        started = True
                        emit(
                            {
                                "v": 1,
                                "id": request["id"],
                                "kind": "started",
                                "pid": child.pid,
                            }
                        )
                    if len(output) > MAX_RESPONSE + 1:
                        # Send a deliberately invalid bounded frame to signal the cap.
                        sys.stdout.buffer.write(b"x" * (MAX_RESPONSE + 1) + b"\n")
                        sys.stdout.buffer.flush()
                        terminate_group()
                    if b"\n" in output:
                        # Preserve the suffix across reads; EOF must reject a partial
                        # frame even when it follows a previously valid result.
                        boundary = output.rindex(b"\n") + 1
                        sys.stdout.buffer.write(output[:boundary])
                        sys.stdout.buffer.flush()
                        del output[:boundary]
    finally:
        if child.poll() is None:
            child.kill()
        child.wait(timeout=1)
        child.stdout.close()


if __name__ == "__main__":
    try:
        if len(sys.argv) == 3 and sys.argv[1] == "--step":
            step(sys.argv[2])
        elif len(sys.argv) == 1:
            guard()
        else:
            raise ValueError("invalid fixed entry point")
    except (ValueError, json.JSONDecodeError, OSError, subprocess.TimeoutExpired):
        # Deliberately exclude exception text and request contents from diagnostics.
        print("executor protocol or process failure", file=sys.stderr)
        raise SystemExit(65) from None
