"""Execute precisely one registered step; Go retains all Run and RPC ownership."""

from __future__ import annotations

import asyncio
import copy
import os
import signal
import sys
from typing import Any, Literal

from jobforge_agent.business import SnapshotBinding
from jobforge_agent.deepseek import DeepSeekChat
from jobforge_agent.dispatch import (
    AuthorizedDispatcher,
    DispatchError,
    Endpoint,
    EndpointAlias,
    RunCallContext,
    boottime_ms,
)
from jobforge_agent.executor_ipc import PipeHooks
from jobforge_agent.protocol_v2 import Frame, ProtocolError
from jobforge_agent.run_tools import RunBusinessTools
from jobforge_agent.runtime_adapters import RegisteredAdapter, resolve_adapter
from jobforge_agent.runtime_input import (
    RuntimeInput,
    RuntimeInputError,
    parse_runtime_input,
)


def _endpoints(kind: str) -> dict[EndpointAlias, Endpoint]:
    values: dict[EndpointAlias, Endpoint] = {}
    if kind in {"get_order", "get_delivery", "search_policy"}:
        origin = os.environ.get("JOBFORGE_BUSINESS_ORIGIN", "")
        key = os.environ.get("JOBFORGE_BUSINESS_READ_KEY", "")
        if not origin or not key:
            raise DispatchError("PROFILE_UNAVAILABLE")
        values["business"] = Endpoint(origin, key)
    if kind == "search_policy":
        origin = os.environ.get("JOBFORGE_OLLAMA_ORIGIN", "")
        if not origin:
            raise DispatchError("PROFILE_UNAVAILABLE")
        values["ollama"] = Endpoint(origin)
    if kind in {"model_proposal", "protocol_correction"}:
        key = os.environ.get("DEEPSEEK_API_KEY", "")
        if not key:
            raise DispatchError("PROFILE_UNAVAILABLE")
        values["deepseek"] = Endpoint("https://api.deepseek.com", key)
    return values


def _result() -> dict[str, Any]:
    return {
        "schema_version": 1,
        "tool_invocation_id": "",
        "physical_call_id": "",
        "evidence_refs": [],
        "content": None,
        "proposal": None,
        "correction_required": False,
    }


async def execute_registered_step(
    frame: Frame,
    runtime: RuntimeInput,
    adapter: RegisteredAdapter,
    dispatcher: AuthorizedDispatcher,
) -> Frame:
    """Run a fixed kind and bind its result to the last acknowledged call."""
    kind = frame["binding"]["step_kind"]
    checkpoint, result = runtime.checkpoint, _result()
    snapshot = checkpoint["snapshot"]
    context = RunCallContext(
        frame["binding"]["profile_hash"],
        frame["binding"]["snapshot_hash"],
        runtime.tool_invocation_id,
    )
    outcome: Literal["success", "error"] = "success"
    error_code = ""
    if kind == "read_ticket":
        result["content"] = copy.deepcopy(snapshot["ticket_binding_json"])
        result["evidence_refs"] = [
            f"business-evidence:{snapshot['snapshot_id']}:ticket"
        ]
    elif kind in {"get_order", "get_delivery", "search_policy"}:
        ticket = snapshot["ticket_binding_json"]
        binding = SnapshotBinding(
            snapshot["snapshot_id"],
            ticket["order_id"],
            snapshot["index_profile_hash"],
            snapshot["index_id"],
            ticket["policy_version"],
        )
        arguments = (
            {"query": adapter.policy_query(copy.deepcopy(checkpoint))}
            if kind == "search_policy"
            else {"order_id": binding.order_id}
        )
        content = await RunBusinessTools(dispatcher, binding).execute(
            kind, arguments, context=context
        )
        result["content"] = content
        result["evidence_refs"] = (
            [item["evidence_ref"] for item in content["matches"]]
            if kind == "search_policy"
            else [content["evidence_ref"]]
        )
    elif kind in {"model_proposal", "protocol_correction"}:
        try:
            result["proposal"] = await DeepSeekChat(dispatcher).propose(
                adapter.proposal_messages(
                    copy.deepcopy(checkpoint), correction=kind == "protocol_correction"
                ),
                context=context,
                validate_proposal=lambda value: adapter.validate_proposal(
                    value, copy.deepcopy(checkpoint)
                ),
            )
        except DispatchError as error:
            confirmed = dispatcher.last_confirmed_observation()
            if not (
                kind == "model_proposal"
                and error.code == "OUTPUT_INVALID"
                and not error.fact
                and not error.stop
                and confirmed is not None
                and confirmed.transport_outcome == "response"
                and confirmed.business_outcome == "rejected"
                and confirmed.error_code == "OUTPUT_INVALID"
            ):
                raise
            result["correction_required"] = True
            outcome, error_code = "error", "OUTPUT_INVALID"
    elif kind == "submit_proposal":
        steps = checkpoint["steps"]
        if (
            not steps
            or steps[-1]["step"]["kind"]
            not in {"model_proposal", "protocol_correction"}
            or steps[-1]["result_json"]["proposal"] is None
        ):
            raise DispatchError("INPUT_INVALID")
        result["proposal"] = copy.deepcopy(steps[-1]["result_json"]["proposal"])
    else:
        raise DispatchError("INPUT_INVALID")
    if kind not in {"read_ticket", "submit_proposal"}:
        confirmed = dispatcher.last_confirmed_observation()
        if confirmed is None:
            raise DispatchError("PROTOCOL_ERROR")
        result["tool_invocation_id"] = confirmed.tool_invocation_id
        result["physical_call_id"] = confirmed.physical_call_id
    return dispatcher.finalize_result(result, outcome=outcome, error_code=error_code)


def exit_code(error: BaseException) -> int:
    """Map only trusted local facts to ADR-0019's existing exit contract."""
    if isinstance(error, RuntimeInputError):
        return 67 if error.size_limit else 64
    if isinstance(error, ProtocolError):
        return 67 if error.code == "FRAME_LIMIT" else 65
    if isinstance(error, DispatchError):
        if error.fact == "size_limit":
            return 67
        return {
            "INPUT_INVALID": 64,
            "PROFILE_UNAVAILABLE": 66,
            "DEPENDENCY_UNAVAILABLE": 68,
            "TIMEOUT": 69,
            "OUTPUT_INVALID": 70,
            "STOP_REQUESTED": 71,
            "STALE_LEASE": 71,
            "BUDGET_EXHAUSTED": 71,
        }.get(error.code, 65)
    if isinstance(error, (asyncio.CancelledError, KeyboardInterrupt)):
        return 71
    if isinstance(error, TimeoutError):
        return 69
    if isinstance(error, OSError):
        return 68
    return 65


async def run() -> int:
    """Own the business task, transport tasks and bounded stop-only cleanup."""
    hooks: PipeHooks | None = None
    dispatcher: AuthorizedDispatcher | None = None
    tasks: list[asyncio.Task[Any]] = []
    stopped = asyncio.Event()
    loop = asyncio.get_running_loop()
    for signum in (signal.SIGTERM, signal.SIGINT):
        loop.add_signal_handler(signum, stopped.set)
    code = 65
    try:
        hooks = await PipeHooks.open()

        async def work() -> None:
            nonlocal dispatcher
            assert hooks is not None
            frame = await hooks.read_execute()
            runtime = parse_runtime_input(frame)
            binding = frame["binding"]
            adapter = resolve_adapter(
                runtime.adapter_id,
                binding["profile_id"],
                binding["profile_hash"],
                runtime.executor_version,
            )
            dispatcher = AuthorizedDispatcher(
                frame, hooks=hooks, endpoints=_endpoints(binding["step_kind"])
            )
            remaining = frame["emitted_mono_ms"] + frame["remaining_ms"] - boottime_ms()
            if remaining <= 0:
                raise DispatchError("TIMEOUT")
            async with asyncio.timeout(remaining / 1000):
                result = await execute_registered_step(
                    frame, runtime, adapter, dispatcher
                )
                await hooks.write_result(result)

        business = asyncio.create_task(work())
        failed = asyncio.create_task(hooks.wait_failed())
        stop = asyncio.create_task(stopped.wait())
        tasks.extend((business, failed, stop))
        await asyncio.wait(tasks, return_when=asyncio.FIRST_COMPLETED)
        if stop.done():
            raise DispatchError("STOP_REQUESTED")
        if failed.done():
            await failed
        await business
        code = 0
    except (Exception, asyncio.CancelledError) as error:
        code = exit_code(error)
    finally:
        if dispatcher is not None:
            dispatcher.stop()
        for task in tasks:
            if not task.done():
                task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        if code != 0 and dispatcher is not None and hooks is not None:
            try:
                async with asyncio.timeout(0.02):
                    await hooks.flush_captured(dispatcher.recorded_usage())
            except (DispatchError, TimeoutError, OSError):
                pass
        if dispatcher is not None:
            try:
                async with asyncio.timeout(0.1):
                    await dispatcher.aclose()
            except (DispatchError, TimeoutError, OSError):
                if code == 0:
                    code = 65
        if hooks is not None:
            try:
                await hooks.aclose()
            except DispatchError as error:
                if code == 0:
                    code = exit_code(error)
        for signum in (signal.SIGTERM, signal.SIGINT):
            loop.remove_signal_handler(signum)
    if stopped.is_set() and code == 0:
        code = 71
    return code


def main() -> int:
    """Use the installed package and emit no provider or protocol diagnostics."""
    sys.dont_write_bytecode = True
    if sys.platform != "linux":
        return 66
    try:
        return asyncio.run(run())
    except (Exception, KeyboardInterrupt) as error:
        return exit_code(error)


if __name__ == "__main__":
    raise SystemExit(main())
