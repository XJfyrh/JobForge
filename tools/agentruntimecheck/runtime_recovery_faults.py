"""Static integration-image barriers; never copied into production runtime.

The external Linux harness selects one exact Run/attempt/step and sends the
actual kill. A marker publishes process identity only, not a successful outcome.
"""

from __future__ import annotations

import json
import os
import signal
from pathlib import Path
from typing import Any

CONFIG = Path("/etc/jobforge/recovery/fault.json")
MARKER = Path("/etc/jobforge/recovery/marker.json")


def pause(binding: dict[str, Any], point: str) -> None:
    """SIGSTOP only the exact registered fixture process at a factual boundary."""
    try:
        raw = CONFIG.read_bytes()
    except FileNotFoundError:
        return
    if len(raw) > 1024:
        raise ValueError("invalid recovery fixture")
    config = json.loads(raw)
    expected = {"run_id", "attempt_no", "step_kind", "point"}
    if not isinstance(config, dict) or set(config) != expected:
        raise ValueError("invalid recovery fixture")
    selected = dict(config)
    fault = selected["point"]
    malformed = point == "before_intent" and fault in {
        "bad_frame",
        "ordinary_fragment",
        "metering_fragment",
        "stderr_oversize",
    }
    if malformed:
        selected["point"] = point
    if selected != {
        "run_id": binding["run_id"],
        "attempt_no": binding["attempt_no"],
        "step_kind": binding["step_kind"],
        "point": point,
    }:
        return
    temporary = MARKER.with_suffix(".tmp")
    temporary.write_text(
        json.dumps({**config, "pid": os.getpid(), "step_id": binding["step_id"]}),
        encoding="utf-8",
    )
    temporary.replace(MARKER)
    if malformed:
        if fault == "bad_frame":
            os.write(1, b"{\n")
        elif fault == "ordinary_fragment":
            os.write(1, b"{")
        elif fault == "metering_fragment":
            os.write(5, b"{")
        else:
            os.write(2, b"x" * 8193)
    os.kill(os.getpid(), signal.SIGSTOP)


def install() -> None:
    """Wrap fixed dispatcher boundaries solely in the synthetic test target."""
    from jobforge_agent.dispatch import AuthorizedDispatcher

    frame = AuthorizedDispatcher._frame
    send = AuthorizedDispatcher._send
    finalize = AuthorizedDispatcher.finalize_result

    def guarded_frame(self: Any, kind: str, **values: Any) -> Any:
        result = frame(self, kind, **values)
        if kind == "call_intent":
            pause(result["binding"], "before_intent")
        return result

    async def guarded_send(self: Any, *args: Any, **kwargs: Any) -> Any:
        pause(self._start["binding"], "before_send")
        return await send(self, *args, **kwargs)

    def guarded_finalize(self: Any, *args: Any, **kwargs: Any) -> Any:
        result = finalize(self, *args, **kwargs)
        pause(result["binding"], "after_confirmed")
        return result

    AuthorizedDispatcher._frame = guarded_frame
    AuthorizedDispatcher._send = guarded_send
    AuthorizedDispatcher.finalize_result = guarded_finalize
