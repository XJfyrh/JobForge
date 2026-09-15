"""Deterministic checks for the S0 probe guardrails, not model acceptance."""

import argparse
import asyncio
import importlib.util
import json
from pathlib import Path

import httpx
import pytest

SPEC = importlib.util.spec_from_file_location(
    "agent_model_probe", Path(__file__).resolve().parents[1] / "agent_model_probe.py"
)
assert SPEC is not None and SPEC.loader is not None
PROBE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PROBE)
CASE = json.loads(PROBE.DATA_PATH.read_text(encoding="utf-8"))["cases"][0]


def call(name: str, arguments: object) -> dict:
    """Build a deliberately synthetic model message for guardrail tests."""
    return {"tool_calls": [{"function": {"name": name, "arguments": arguments}}]}


@pytest.mark.parametrize(
    ("message", "error"),
    [
        (call("write_ticket", {}), "UNKNOWN_TOOL"),
        (call("get_order", {"order_id": 42}), "INVALID_ARGUMENTS"),
        (
            call("get_order", {"order_id": CASE["order_id"], "tenant": "x"}),
            "INVALID_ARGUMENTS",
        ),
        (
            call("get_order", {"order_id": "other-tenant-order"}),
            "OBJECT_NOT_AUTHORIZED",
        ),
        ({"tool_calls": [None]}, "INVALID_TOOL_ENVELOPE"),
        (
            {"tool_calls": [call("get_order", {}), call("get_delivery", {})]},
            "EXPECTED_SINGLE_TOOL",
        ),
    ],
)
def test_rejects_entire_invalid_call(message: dict, error: str) -> None:
    """Reject unknown tools, wrong shapes and unauthorized objects before dispatch."""
    with pytest.raises(PROBE.ProbeError, match=error):
        PROBE.validate_call(message, CASE)


@pytest.mark.parametrize(
    "value",
    [
        "https://api.example.com",
        "http://localhost:11435",
        "http://127.0.0.1:11435/api",
        "http://secret@127.0.0.1:11435",
    ],
)
def test_rejects_nonlocal_or_credential_origins(value: str) -> None:
    """The probe never accepts a remote paid service or embedded credentials."""
    with pytest.raises(argparse.ArgumentTypeError):
        PROBE.local_origin(value)


def test_final_requires_actual_evidence_and_accurate_values() -> None:
    """Schema validity does not silently convert incorrect decisions into success."""
    refs = {
        PROBE.fixture_result(name, CASE)["evidence_ref"]
        for name in PROBE.TOOL_ARGUMENTS
    }
    final = {
        "decision": CASE["expected_decision"],
        "order_id": CASE["order_id"],
        "delivery_status": CASE["delivery_status"],
        "evidence_refs": sorted(refs),
    }
    assert PROBE.validate_final(json.dumps(final), CASE, refs)
    final["decision"] = "record_only"
    assert not PROBE.validate_final(json.dumps(final), CASE, refs)
    with pytest.raises(PROBE.ProbeError, match="EVIDENCE"):
        PROBE.validate_final(json.dumps(final), CASE, set())


def test_response_bound_and_wall_deadline() -> None:
    """Bound actual async reads independently of a transport's per-read timeout."""

    async def oversized(_: httpx.Request) -> httpx.Response:
        return httpx.Response(200, content=b"x" * (PROBE.MAX_RESPONSE + 1))

    async def delayed(_: httpx.Request) -> httpx.Response:
        await asyncio.sleep(1)
        return httpx.Response(200, json={})

    async def check() -> None:
        async with httpx.AsyncClient(
            transport=httpx.MockTransport(oversized)
        ) as client:
            with pytest.raises(PROBE.ProbeError, match="RESPONSE_TOO_LARGE"):
                await PROBE.request_json(client, "GET", "http://127.0.0.1:1")
        async with httpx.AsyncClient(transport=httpx.MockTransport(delayed)) as client:
            with pytest.raises(TimeoutError):
                await PROBE.request_json(
                    client, "GET", "http://127.0.0.1:1", timeout=0.01
                )

    asyncio.run(check())
