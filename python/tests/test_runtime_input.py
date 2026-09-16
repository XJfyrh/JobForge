"""Consume the same RPC-projection fixtures as the Go input builder."""

from __future__ import annotations

import copy
import json
from pathlib import Path

import pytest
from jobforge_agent.protocol_v2 import ProtocolError, decode, encode
from jobforge_agent.runtime_input import RuntimeInputError, parse_runtime_input

FIXTURE = json.loads(
    (
        Path(__file__).resolve().parents[2]
        / "api/executor/v2/fixtures/runtime-input.json"
    ).read_text(encoding="utf-8")
)


@pytest.mark.parametrize("case", FIXTURE["valid"], ids=lambda case: case["name"])
def test_shared_valid_runtime_projection(case: dict) -> None:
    """Accept the original hash strings and copied RPC field names unchanged."""
    frame = copy.deepcopy(case["frame"])
    runtime = parse_runtime_input(frame)
    original = copy.deepcopy(runtime.checkpoint)
    frame["checkpoint"]["snapshot"]["ticket_binding_json"]["subject"] = "mutated"
    assert runtime.checkpoint == original


@pytest.mark.parametrize("case", FIXTURE["invalid"], ids=lambda case: case["name"])
def test_shared_invalid_runtime_projection(case: dict) -> None:
    """Reject extra authority, null objects, wrong identity and broken hash chains."""
    with pytest.raises(RuntimeInputError):
        parse_runtime_input(case["frame"])


@pytest.mark.parametrize("case", FIXTURE["invalid_json"], ids=lambda case: case["name"])
def test_duplicate_runtime_fields_are_rejected_at_codec(case: dict) -> None:
    """A Python mapping cannot hide duplicate source JSON object members."""
    with pytest.raises((ProtocolError, RuntimeInputError)):
        parse_runtime_input(decode(case["json"].encode("utf-8").rstrip(b"\n") + b"\n"))


@pytest.mark.parametrize("field,limit", [("input", 16384), ("checkpoint", 256 * 1024)])
def test_source_field_limit_keeps_size_failure_category(field: str, limit: int) -> None:
    """Raw whitespace/escaping overhead is a size fact, not an unknown protocol bug."""
    frame = copy.deepcopy(FIXTURE["valid"][0]["frame"])
    frame[field] = {"padding": "x" * limit}
    with pytest.raises(ProtocolError) as failure:
        encode(frame)
    assert failure.value.code == "FRAME_LIMIT"


def test_protected_result_limit_keeps_size_failure_category() -> None:
    """A bounded whole frame can still exceed the read-step result limit."""
    frame = copy.deepcopy(FIXTURE["valid"][0]["frame"])
    for field in ("input", "checkpoint", "remaining_ms", "trace_context"):
        del frame[field]
    frame.update(
        kind="step_result",
        outcome="success",
        error_code="",
        result={"padding": "x" * 8192},
    )
    with pytest.raises(ProtocolError) as failure:
        encode(frame)
    assert failure.value.code == "FRAME_LIMIT"
