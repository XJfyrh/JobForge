"""Deterministic audit wire joins; synthetic ACKs do not prove persistence."""

from __future__ import annotations

import asyncio
import copy
import itertools
import json
from typing import Any

import pytest
from jobforge_agent.executor_ipc import PipeHooks
from jobforge_agent.protocol_v2 import (
    Conversation,
    ProtocolError,
    decode_metering,
    encode,
    encode_metering,
    observation_hash,
    report_hash,
)
from jobforge_agent.provider_audit import capture_chat_report
from test_dispatch import chat_body, start_frame

Frame = dict[str, Any]


def _frames(body: bytes | None = chat_body()) -> dict[str, Frame]:
    start = start_frame()
    common = {
        key: copy.deepcopy(start[key])
        for key in ("version", "request_id", "binding", "emitted_mono_ms")
    }
    intent = {
        **common,
        "kind": "call_intent",
        "call_sequence": 1,
        "subcall": "chat",
        "parameter_hash": "c" * 64,
        "tool_invocation_id": "",
    }
    call_id = "00000000-0000-4000-8000-000000000020"
    permit = {
        **intent,
        "kind": "call_permit",
        "physical_call_id": call_id,
        "granted": True,
        "error_code": "",
        "dispatch_ms": 500,
        "call_ms": 2000,
        "input_token_limit": 2000,
        "output_token_limit": 1024,
    }
    captured = capture_chat_report(
        body,
        http_status=200,
        physical_call_id=call_id,
        expected_response_model="deepseek-flash",
    )
    report = {
        **common,
        "kind": "metering_report",
        "call_sequence": 1,
        "physical_call_id": call_id,
        "parameter_hash": "c" * 64,
        **captured.to_dict(),
    }
    report["report_hash"] = report_hash(report)
    report["emitted_mono_ms"] = 1001
    observation = {
        **common,
        "kind": "call_observation",
        "call_sequence": 1,
        "physical_call_id": call_id,
        "transport_outcome": "response",
        "http_status": 200,
        "business_outcome": "accepted",
        "error_code": "",
        "usage_disposition": "reported",
        "usage_hash": report["usage"]["usage_hash"] if report["usage"] else None,
        "audit_hash": report["provider_audit"]["audit_hash"],
        "emitted_mono_ms": 1002,
    }
    meter_ack = {
        **common,
        "kind": "metering_ack",
        "call_sequence": 1,
        "physical_call_id": call_id,
        "report_hash": report["report_hash"],
        "settlement": "settled",
        "emitted_mono_ms": 1003,
    }
    ordinary_ack = {
        **common,
        "kind": "call_observation_ack",
        "call_sequence": 1,
        "physical_call_id": call_id,
        "observation_hash": observation_hash(observation)
        if body is not None and report["usage"]
        else "a" * 64,
        "emitted_mono_ms": 1004,
    }
    result = {
        **common,
        "kind": "step_result",
        "outcome": "success",
        "error_code": "",
        "result": {},
        "emitted_mono_ms": 1005,
    }
    return {
        "start": start,
        "intent": intent,
        "permit": permit,
        "report": report,
        "observation": observation,
        "meter_ack": meter_ack,
        "ordinary_ack": ordinary_ack,
        "result": result,
    }


def _start(frames: dict[str, Frame]) -> Conversation:
    conversation = Conversation()
    for name in ("start", "intent", "permit"):
        conversation.accept(frames[name], 1000)
    conversation.can_dispatch(frames["permit"]["physical_call_id"], 1000)
    return conversation


_ORDERS = [
    order
    for order in itertools.permutations(
        ("report", "observation", "meter_ack", "ordinary_ack")
    )
    if order.index("report") < order.index("meter_ack")
    and order.index("observation") < order.index("ordinary_ack")
]


@pytest.mark.parametrize("order", _ORDERS, ids=lambda values: "-".join(values))
def test_two_fd_join_needs_both_matching_acknowledgements(
    order: tuple[str, ...],
) -> None:
    """Neither read order nor one ACK alone can create ordinary continuation."""
    frames = _frames()
    conversation = _start(frames)
    for index, name in enumerate(order):
        method = (
            conversation.accept_metering
            if name in {"report", "meter_ack"}
            else conversation.accept
        )
        method(frames[name], 1005)
        if index < len(order) - 1:
            with pytest.raises(ProtocolError):
                copy.deepcopy(conversation).accept(frames["result"], 1005)
    conversation.accept(frames["result"], 1005)


@pytest.mark.parametrize(
    "scenario",
    ["absent", "incompatible", "reasoning", "tools", "invalid_choice", "incomplete"],
)
def test_report_facts_stop_ordinary_execution_before_ack(scenario: str) -> None:
    """Unknown, identity and mode violations preserve reports without waiting."""
    value = json.loads(chat_body())
    if scenario == "absent":
        del value["usage"]
    elif scenario == "incompatible":
        value["model"] = "other-model"
    elif scenario == "reasoning":
        value["usage"]["completion_tokens_details"] = {"reasoning_tokens": 1}
    elif scenario == "tools":
        value["choices"][0]["message"]["tool_calls"] = [{"private": "not stored"}]
    elif scenario == "invalid_choice":
        value["choices"] = []
    frames = _frames(None if scenario == "incomplete" else json.dumps(value).encode())
    conversation = _start(frames)
    conversation.accept_metering(frames["report"], 1005)
    assert conversation.closed and not conversation.metering_pending
    frames["meter_ack"]["settlement"] = (
        "recorded"
        if scenario in {"absent", "incompatible", "incomplete"}
        else "settled"
    )
    conversation.accept_metering(frames["meter_ack"], 1005)
    with pytest.raises(ProtocolError):
        conversation.accept(frames["result"], 1005)


@pytest.mark.parametrize(
    "settlement", ["recorded", "anomaly", "conflict", "unconfirmed"]
)
def test_nonsettled_ack_never_authorizes_result(settlement: str) -> None:
    """Recorded is rejected for priceable usage; all other negative ACKs stop."""
    frames = _frames()
    conversation = _start(frames)
    conversation.accept_metering(frames["report"], 1005)
    frames["meter_ack"]["settlement"] = settlement
    if settlement == "recorded":
        with pytest.raises(ProtocolError):
            conversation.accept_metering(frames["meter_ack"], 1005)
    else:
        conversation.accept_metering(frames["meter_ack"], 1005)
    assert conversation.closed
    with pytest.raises(ProtocolError):
        conversation.accept(frames["result"], 1005)


@pytest.mark.parametrize("source", ["audit", "report", "receipt", "ack"])
def test_hash_mismatch_closes_without_accepting_report_or_result(source: str) -> None:
    """Self-contained report/receipt/hash checks precede cross-FD continuation."""
    frames = _frames()
    conversation = _start(frames)
    if source == "ack":
        conversation.accept_metering(frames["report"], 1005)
        frame = frames["meter_ack"]
        frame["report_hash"] = "0" * 64
    else:
        frame = frames["report"]
        if source == "audit":
            frame["provider_audit"]["audit_hash"] = "0" * 64
        elif source == "receipt":
            frame["usage"]["receipt_hash"] = "0" * 64
        else:
            frame["report_hash"] = "0" * 64
    with pytest.raises(ProtocolError):
        conversation.accept_metering(frame, 1005)
    assert conversation.closed


@pytest.mark.parametrize("report_first", [False, True])
def test_observation_wrong_audit_cannot_join_a_valid_metering_report(
    report_first: bool,
) -> None:
    """A valid metering report survives a conflicting ordinary observation."""
    frames = _frames()
    conversation = _start(frames)
    frames["observation"]["audit_hash"] = "f" * 64
    if report_first:
        conversation.accept_metering(frames["report"], 1005)
    conversation.accept(frames["observation"], 1005)
    if not report_first:
        conversation.accept_metering(frames["report"], 1005)
    assert conversation.closed
    conversation.accept_metering(frames["meter_ack"], 1005)
    assert conversation.closed


def test_old_wire_fields_and_nested_raw_audit_size_fail() -> None:
    """There is one audit codec, without old usage-only chat compatibility."""
    frames = _frames()
    for key in ("provider_audit", "report_hash"):
        frame = copy.deepcopy(frames["report"])
        del frame[key]
        with pytest.raises(ProtocolError):
            encode_metering(frame)
    old_ack = copy.deepcopy(frames["meter_ack"])
    old_ack["usage_hash"] = old_ack.pop("report_hash")
    with pytest.raises(ProtocolError):
        encode_metering(old_ack)
    missing = copy.deepcopy(frames["observation"])
    del missing["audit_hash"]
    with pytest.raises(ProtocolError):
        encode(missing)
    raw = encode_metering(frames["report"])
    raw = raw.replace(b'"provider_audit":{', b'"provider_audit":{' + b" " * 2048)
    with pytest.raises(ProtocolError):
        decode_metering(raw)


@pytest.mark.parametrize("stop", ["stop", "deadline", "unconfirmed"])
def test_late_settled_ack_cannot_restore_closed_ordinary_authority(stop: str) -> None:
    """Narrow reporting is retained after stop; it never resumes the conversation."""
    frames = _frames()
    conversation = _start(frames)
    if stop == "stop":
        conversation.stop()
    now = 12000 if stop == "deadline" else 1005
    conversation.accept_metering(frames["report"], now)
    if stop == "unconfirmed":
        negative = {**frames["meter_ack"], "settlement": "unconfirmed"}
        conversation.accept_metering(negative, now)
    conversation.accept_metering(frames["meter_ack"], now)
    assert conversation.closed
    with pytest.raises(ProtocolError):
        conversation.accept(frames["result"], now)


@pytest.mark.parametrize("incompatible", [False, True])
def test_recorded_evidence_cannot_be_acknowledged_as_settled(
    incompatible: bool,
) -> None:
    """Only compatible complete usage can receive a normal settlement ACK."""
    body = json.loads(chat_body())
    if incompatible:
        body["model"] = "other-model"
    else:
        del body["usage"]
    frames = _frames(json.dumps(body).encode())
    conversation = _start(frames)
    conversation.accept_metering(frames["report"], 1005)
    with pytest.raises(ProtocolError):
        conversation.accept_metering(frames["meter_ack"], 1005)
    assert conversation.closed


def test_free_call_rejects_chat_audit_and_needs_ordinary_ack_only() -> None:
    """Nonchat free HTTP never gains or waits for a provider audit report."""
    frames = _frames()
    for frame in frames.values():
        frame["binding"]["step_kind"] = "get_order"
    for name in ("intent", "permit"):
        frames[name].update(
            subcall="get_order",
            tool_invocation_id="00000000-0000-4000-8000-000000000088",
        )
    frames["permit"].update(input_token_limit=0, output_token_limit=0)
    observation = frames["observation"]
    observation.update(usage_disposition="unknown", usage_hash=None)
    with pytest.raises(ProtocolError):
        encode(observation)
    observation["audit_hash"] = None
    frames["ordinary_ack"]["observation_hash"] = observation_hash(observation)
    conversation = _start(frames)
    conversation.accept(observation, 1005)
    with pytest.raises(ProtocolError):
        copy.deepcopy(conversation).accept(frames["result"], 1005)
    conversation.accept(frames["ordinary_ack"], 1005)
    conversation.accept(frames["result"], 1005)


@pytest.mark.parametrize("wrong_hash", [False, True])
def test_pipe_route_matches_report_hash_before_delivering_ack(wrong_hash: bool) -> None:
    """The existing pipe route compares the new identity without another lane."""

    async def run() -> None:
        hooks = PipeHooks()
        hooks._started = True
        written = asyncio.Event()

        async def write(frame: Frame, metering: bool, *, flush: bool = False) -> None:
            assert metering and not flush
            encode_metering(frame)
            written.set()

        hooks._write = write
        frames = _frames()
        task = asyncio.create_task(hooks.settle(frames["report"]))
        try:
            await written.wait()
            ack = frames["meter_ack"]
            if wrong_hash:
                ack["report_hash"] = "f" * 64
                with pytest.raises(ProtocolError):
                    hooks._route(decode_metering(encode_metering(ack)), True)
                assert not task.done()
            else:
                hooks._route(decode_metering(encode_metering(ack)), True)
                assert await task == ack
        finally:
            if not task.done():
                task.cancel()
            await asyncio.gather(task, return_exceptions=True)

    asyncio.run(run())
