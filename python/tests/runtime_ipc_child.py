"""Linux pipe test helper, excluded from the installed production wheel."""

from __future__ import annotations

import asyncio
import copy
import fcntl
import json
import os
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from jobforge_agent.dispatch import DispatchError  # noqa: E402 -- test source import
from jobforge_agent.executor_ipc import PipeHooks  # noqa: E402 -- test source import
from jobforge_agent.protocol_v2 import report_hash  # noqa: E402 -- test source import
from jobforge_agent.provider_audit import (
    capture_chat_report,  # noqa: E402 -- test source import
)


async def run(mode: str) -> int:
    """Exercise the production transport over inherited OS pipes."""
    hooks = await PipeHooks.open()
    code = 0
    try:
        frame = await hooks.read_execute()
        if mode == "dual":
            common = {
                key: copy.deepcopy(frame[key])
                for key in ("version", "request_id", "binding", "emitted_mono_ms")
            }
            common.update(
                call_sequence=1, physical_call_id="00000000-0000-4000-8000-000000000020"
            )
            body = json.dumps(
                {
                    "id": "synthetic",
                    "object": "chat.completion",
                    "created": 1,
                    "model": "deepseek-flash",
                    "system_fingerprint": "synthetic-fp",
                    "choices": [
                        {
                            "index": 0,
                            "finish_reason": "stop",
                            "message": {"role": "assistant", "content": "{}"},
                        }
                    ],
                    "usage": {
                        "prompt_tokens": 1,
                        "completion_tokens": 1,
                        "total_tokens": 2,
                        "prompt_cache_hit_tokens": 0,
                        "prompt_cache_miss_tokens": 1,
                    },
                }
            ).encode()
            captured = capture_chat_report(
                body,
                http_status=200,
                physical_call_id=common["physical_call_id"],
                expected_response_model="deepseek-flash",
            )
            report = {
                **common,
                "kind": "metering_report",
                "parameter_hash": "c" * 64,
                **captured.to_dict(),
            }
            report["report_hash"] = report_hash(report)
            observation = {
                **common,
                "kind": "call_observation",
                "transport_outcome": "response",
                "http_status": 200,
                "business_outcome": "accepted",
                "error_code": "",
                "usage_disposition": "reported",
                "usage_hash": report["usage"]["usage_hash"],
                "audit_hash": report["provider_audit"]["audit_hash"],
            }
            await asyncio.gather(hooks.settle(report), hooks.observe(observation))
        elif mode == "wait":
            await hooks.wait_failed()
        elif mode == "authorize":
            intent = {
                key: copy.deepcopy(frame[key])
                for key in ("version", "request_id", "binding", "emitted_mono_ms")
            }
            intent.update(
                kind="call_intent",
                call_sequence=1,
                subcall="chat",
                parameter_hash="c" * 64,
                tool_invocation_id="",
            )
            await hooks.authorize(intent)
        else:
            result = {
                key: copy.deepcopy(frame[key])
                for key in ("version", "request_id", "binding", "emitted_mono_ms")
            }
            result.update(
                kind="step_result",
                outcome="success",
                error_code="",
                result={"fixture": True},
            )
            await hooks.write_result(result)
    except DispatchError:
        code = 65
    finally:
        try:
            await hooks.aclose()
        except DispatchError:
            code = 65
    return code


if __name__ == "__main__":
    # Duplicate first so source FD collisions cannot change either pipe endpoint.
    metering_read = fcntl.fcntl(int(sys.argv[2]), fcntl.F_DUPFD_CLOEXEC, 6)
    metering_write = fcntl.fcntl(int(sys.argv[3]), fcntl.F_DUPFD_CLOEXEC, 6)
    os.dup2(metering_read, 4)
    os.dup2(metering_write, 5)
    for descriptor in {
        metering_read,
        metering_write,
        int(sys.argv[2]),
        int(sys.argv[3]),
    } - {0, 1, 2, 4, 5}:
        os.close(descriptor)
    raise SystemExit(asyncio.run(run(sys.argv[1])))
