"""Original-conversation result barriers, including rejected and unbilled calls."""

from __future__ import annotations

import copy
from dataclasses import FrozenInstanceError
from typing import Any

import pytest
from http_fault_server import HTTPFaultServer
from jobforge_agent.dispatch import (
    AuthorizedDispatcher,
    CompleteResponse,
    DispatchError,
)
from jobforge_agent.step import exit_code
from test_dispatch import (
    Clock,
    Hooks,
    context,
    good_response,
    make_dispatcher,
    prepared,
    run_async,
    start_frame,
    usage,
    valid,
)


def result(*, physical: str = "", correction: bool = False) -> dict[str, Any]:
    """Build the actual protected result shape, never a protocol-only placeholder."""
    return {
        "schema_version": 1,
        "tool_invocation_id": "",
        "physical_call_id": physical,
        "evidence_refs": [],
        "content": None,
        "proposal": None
        if correction
        else {
            "decision": "no_action",
            "summary": "fixture",
            "evidence_refs": ["business-evidence:fixture:ticket"],
            "action": "",
        },
        "correction_required": correction,
    }


@run_async
@pytest.mark.parametrize("reported", [False, True])
async def test_chat_result_requires_report_even_without_optional_usage_callback(
    monkeypatch: pytest.MonkeyPatch, reported: bool
) -> None:
    """Omitting a legacy callback cannot turn a chat into an unbilled free call."""
    clock = Clock()
    hooks = Hooks(clock)
    async with HTTPFaultServer(good_response) as server:
        dispatcher = make_dispatcher(monkeypatch, server, clock, hooks)
        try:
            await dispatcher.execute(
                prepared(),
                context=context(),
                validate=valid,
                extract_usage=usage if reported else None,
            )
            confirmed = dispatcher.last_confirmed_observation()
            assert confirmed is not None
            assert confirmed.usage_disposition == "reported"
            assert (
                confirmed.audit_hash == hooks.reports[0]["provider_audit"]["audit_hash"]
            )
            with pytest.raises(FrozenInstanceError):
                confirmed.physical_call_id = "changed"
            payload = result(physical=confirmed.physical_call_id)
            frame = dispatcher.finalize_result(
                payload, outcome="success", error_code=""
            )
            payload["proposal"]["summary"] = "changed"
            assert frame["result"]["proposal"]["summary"] == "fixture"
            assert dispatcher.closed
            with pytest.raises(DispatchError):
                dispatcher.finalize_result(payload, outcome="success", error_code="")
        finally:
            await dispatcher.aclose()


@run_async
@pytest.mark.parametrize(
    "failure",
    ["missing_ack", "wrong_ack", "wrong_call", "extra_result_field", "stop", "late"],
)
async def test_finalizer_cannot_bypass_original_authority(
    monkeypatch: pytest.MonkeyPatch, failure: str
) -> None:
    """A new result validator cannot restore closed, incomplete or expired state."""
    clock = Clock()
    hooks = Hooks(clock)
    if failure == "wrong_ack":
        hooks.observation_ack_patch["observation_hash"] = "0" * 64
    async with HTTPFaultServer(good_response) as server:
        dispatcher = make_dispatcher(monkeypatch, server, clock, hooks)
        try:
            if failure != "missing_ack":
                if failure == "wrong_ack":
                    with pytest.raises(DispatchError):
                        await dispatcher.execute(
                            prepared(), context=context(), validate=valid
                        )
                    assert dispatcher.last_confirmed_observation() is None
                else:
                    await dispatcher.execute(
                        prepared(), context=context(), validate=valid
                    )
            payload = result(physical="00000000-0000-4000-8000-000000000020")
            if failure == "wrong_call":
                payload["physical_call_id"] = "00000000-0000-4000-8000-000000000099"
            elif failure == "extra_result_field":
                payload["next_cursor"] = 10
            elif failure == "stop":
                dispatcher.stop()
            elif failure == "late":
                clock.now = 4000
            with pytest.raises(DispatchError):
                dispatcher.finalize_result(payload, outcome="success", error_code="")
        finally:
            await dispatcher.aclose()


@run_async
@pytest.mark.parametrize("fact", ["", "size_limit", "identity"])
async def test_only_confirmed_first_output_invalid_has_correction_result(
    monkeypatch: pytest.MonkeyPatch, fact: str
) -> None:
    """Size and resource failures cannot be converted to the unique correction."""
    clock = Clock()
    hooks = Hooks(clock)

    def rejected(response: CompleteResponse) -> None:
        raise DispatchError("OUTPUT_INVALID", fact=fact, stop=bool(fact))

    async with HTTPFaultServer(good_response) as server:
        dispatcher = make_dispatcher(monkeypatch, server, clock, hooks)
        try:
            with pytest.raises(DispatchError):
                await dispatcher.execute(
                    prepared(),
                    context=context(),
                    validate=rejected,
                    extract_usage=usage,
                )
            payload = result(
                physical="00000000-0000-4000-8000-000000000020", correction=True
            )
            if fact:
                with pytest.raises(DispatchError):
                    dispatcher.finalize_result(
                        payload, outcome="error", error_code="OUTPUT_INVALID"
                    )
            else:
                frame = dispatcher.finalize_result(
                    payload, outcome="error", error_code="OUTPUT_INVALID"
                )
                assert frame["result"]["correction_required"]
                assert len(hooks.reports) == 1
        finally:
            await dispatcher.aclose()


@run_async
async def test_local_result_has_no_call_or_metering_requirement() -> None:
    """A read step uses the same finalizer without fabricating a physical call."""
    clock = Clock()
    dispatcher = AuthorizedDispatcher(
        start_frame("read_ticket"), hooks=Hooks(clock), endpoints={}, clock=clock
    )
    payload = result()
    payload.update(
        proposal=None, content={"ticket": "fixture"}, evidence_refs=["fixture"]
    )
    assert dispatcher.finalize_result(payload, outcome="success", error_code="")[
        "result"
    ] == copy.deepcopy(payload)
    await dispatcher.aclose()


@pytest.mark.parametrize(
    "error,expected",
    [
        (DispatchError("INPUT_INVALID"), 64),
        (DispatchError("PROTOCOL_ERROR"), 65),
        (DispatchError("PROFILE_UNAVAILABLE", fact="identity"), 66),
        (DispatchError("OUTPUT_INVALID", fact="size_limit"), 67),
        (DispatchError("DEPENDENCY_UNAVAILABLE"), 68),
        (DispatchError("TIMEOUT"), 69),
        (DispatchError("OUTPUT_INVALID"), 70),
        (DispatchError("STOP_REQUESTED"), 71),
    ],
)
def test_fixed_exit_code_fact_mapping(error: DispatchError, expected: int) -> None:
    """Existing wire failures retain the fixed ADR-0019 meanings."""
    assert exit_code(error) == expected
