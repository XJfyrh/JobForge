"""Single-step composition with trusted fixture inputs and the real finalizer."""

from __future__ import annotations

import copy

import httpx
import pytest
from jobforge_agent.deepseek import MAX_CONTENT_BYTES
from jobforge_agent.dispatch import AuthorizedDispatcher, DispatchError, Endpoint
from jobforge_agent.runtime_input import parse_runtime_input
from jobforge_agent.step import execute_registered_step
from runtime_fixture_registry import MechanismAdapter
from test_deepseek import _envelope, _wire
from test_dispatch import Clock, Hooks, run_async
from test_runtime_input import FIXTURE


@run_async
@pytest.mark.parametrize("kind", ["read_ticket", "submit_proposal"])
async def test_local_step_copies_original_content_without_network(kind: str) -> None:
    """Read/final steps neither acquire physical calls nor synthesize proposals."""
    frame = copy.deepcopy(
        next(item["frame"] for item in FIXTURE["valid"] if item["name"] == kind)
    )
    clock = Clock()
    frame["emitted_mono_ms"] = clock.now
    runtime = parse_runtime_input(frame)
    hooks = Hooks(clock)
    dispatcher = AuthorizedDispatcher(frame, hooks=hooks, endpoints={}, clock=clock)
    try:
        result = await execute_registered_step(
            frame, runtime, MechanismAdapter(), dispatcher
        )
        assert hooks.intents == hooks.reports == hooks.observations == []
        assert result["outcome"] == "success"
        assert result["result"]["physical_call_id"] == ""
        assert result["result"]["tool_invocation_id"] == ""
        if kind == "read_ticket":
            assert (
                result["result"]["content"]
                == frame["checkpoint"]["snapshot"]["ticket_binding_json"]
            )
        else:
            assert (
                result["result"]["proposal"]
                == frame["checkpoint"]["steps"][-1]["result_json"]["proposal"]
            )
    finally:
        await dispatcher.aclose()


@run_async
@pytest.mark.parametrize("content", ["{broken", "[]", "{}"])
async def test_confirmed_invalid_model_content_requests_only_initial_correction(
    content: str,
) -> None:
    """Parser and schema rejection share the real dispatcher's confirmed barrier."""
    for kind in ("model_proposal", "protocol_correction"):
        frame = copy.deepcopy(
            next(
                item["frame"]
                for item in FIXTURE["valid"]
                if item["name"] == "model_proposal"
            )
        )
        frame["binding"]["step_kind"] = kind
        frame["checkpoint"]["next_step"]["kind"] = kind
        clock = Clock()
        frame["emitted_mono_ms"] = clock.now
        hooks = Hooks(clock)
        envelope = _envelope()
        envelope["choices"][0]["message"]["content"] = content
        dispatcher = AuthorizedDispatcher(
            frame,
            hooks=hooks,
            endpoints={"deepseek": Endpoint("https://api.deepseek.com", "fixture")},
            clock=clock,
        )
        body = _wire(envelope)
        dispatcher._clients["deepseek"] = httpx.AsyncClient(
            transport=httpx.MockTransport(
                lambda request, body=body: httpx.Response(200, content=body)
            )
        )
        try:
            operation = execute_registered_step(
                frame, parse_runtime_input(frame), MechanismAdapter(), dispatcher
            )
            if kind == "model_proposal":
                result = await operation
                assert result["outcome"] == "error"
                assert result["result"]["correction_required"] is True
                assert result["result"]["proposal"] is None
            else:
                with pytest.raises(DispatchError, match="OUTPUT_INVALID"):
                    await operation
            assert (
                len(hooks.intents) == len(hooks.reports) == len(hooks.observations) == 1
            )
            assert dispatcher.last_confirmed_observation() is not None
        finally:
            await dispatcher.aclose()


@run_async
async def test_model_content_size_failure_cannot_schedule_correction() -> None:
    """A complete billable response still cannot turn a local limit into repair."""
    frame = copy.deepcopy(
        next(
            item["frame"]
            for item in FIXTURE["valid"]
            if item["name"] == "model_proposal"
        )
    )
    clock = Clock()
    frame["emitted_mono_ms"] = clock.now
    hooks = Hooks(clock)
    envelope = _envelope()
    envelope["choices"][0]["message"]["content"] = "x" * (MAX_CONTENT_BYTES + 1)
    dispatcher = AuthorizedDispatcher(
        frame,
        hooks=hooks,
        endpoints={"deepseek": Endpoint("https://api.deepseek.com", "fixture")},
        clock=clock,
    )
    dispatcher._clients["deepseek"] = httpx.AsyncClient(
        transport=httpx.MockTransport(
            lambda request: httpx.Response(200, content=_wire(envelope))
        )
    )
    try:
        with pytest.raises(DispatchError) as caught:
            await execute_registered_step(
                frame, parse_runtime_input(frame), MechanismAdapter(), dispatcher
            )
        assert caught.value.fact == "size_limit"
        assert dispatcher.last_confirmed_observation() is None
        assert len(hooks.intents) == len(hooks.reports) == 1
        assert hooks.observations == []
    finally:
        await dispatcher.aclose()
