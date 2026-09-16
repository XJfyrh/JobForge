"""Cross-language fixtures and failure boundaries for the executor wire protocol."""

import copy
import io
import json
from pathlib import Path
from typing import Any

import pytest
from jobforge_agent.protocol import (
    MAX_CHECKPOINT_BYTES,
    MAX_FRAME_BYTES,
    Conversation,
    ProtocolError,
    decode,
    encode,
    read_frame,
)

ROOT = Path(__file__).resolve().parents[2] / "api" / "executor" / "v1"
FIXTURES = json.loads((ROOT / "fixtures" / "frames.json").read_text(encoding="utf-8"))


@pytest.mark.parametrize("frame", FIXTURES["valid_frames"], ids=lambda f: f["kind"])
def test_common_valid_frame_roundtrip(frame: dict[str, Any]) -> None:
    """Every shared frame has identical wire meaning after encode/decode."""
    assert decode(encode(frame)) == frame


@pytest.mark.parametrize("case", FIXTURES["invalid_frames"], ids=lambda f: f["name"])
def test_common_invalid_frames_are_rejected(case: dict[str, str]) -> None:
    """Duplicate, aliased, malformed and excess stdout never pass the codec."""
    with pytest.raises(ProtocolError):
        decode(case["wire"].encode("utf-8"))


@pytest.mark.parametrize("case", FIXTURES["valid_sequences"], ids=lambda f: f["name"])
def test_common_sequence_and_single_dispatch(case: dict[str, Any]) -> None:
    """Chat, tools, refusal and invalid output have distinct accepted traces."""
    conversation = Conversation()
    for index, frame in enumerate(case["frames"]):
        conversation.accept(frame, index * 10)
        if frame["kind"] == "call_permit" and frame["granted"]:
            conversation.can_dispatch(frame["physical_call_id"], index * 10)
    with pytest.raises(ProtocolError):
        conversation.accept(case["frames"][-1], 1000)


def test_frame_and_checkpoint_exact_byte_limits() -> None:
    """LF and serialized protected data both count toward their fixed limits."""
    frame = copy.deepcopy(FIXTURES["valid_frames"][0])
    frame["checkpoint"] = {"value": "x" * (MAX_CHECKPOINT_BYTES - 12)}
    encode(frame)
    frame["checkpoint"]["value"] += "x"
    with pytest.raises(ProtocolError):
        encode(frame)
    frame["checkpoint"] = {}
    wire = encode(frame)
    decode(b" " * (MAX_FRAME_BYTES - len(wire)) + wire)
    with pytest.raises(ProtocolError, match="FRAME_LIMIT"):
        decode(b" " * (MAX_FRAME_BYTES + 1 - len(wire)) + wire)
    with pytest.raises(ProtocolError):
        decode(b"\xff" + wire)
    # Count wire bytes, including insignificant object whitespace and escapes.
    padded = wire.replace(
        b'"checkpoint":{}', b'"checkpoint":{' + b" " * MAX_CHECKPOINT_BYTES + b"}"
    )
    with pytest.raises(ProtocolError):
        decode(padded)


def test_tool_result_limit_and_nesting() -> None:
    """Tools cannot use model output bounds and JSON nesting stays bounded."""
    frame = copy.deepcopy(FIXTURES["valid_frames"][4])
    frame["binding"]["step_kind"] = "get_order"
    frame["result"] = {"value": "x" * (8192 - 12)}
    encode(frame)
    frame["result"]["value"] += "x"
    with pytest.raises(ProtocolError):
        encode(frame)
    start = copy.deepcopy(FIXTURES["valid_frames"][0])
    nested: Any = 0
    for _ in range(65):
        nested = [nested]
    start["checkpoint"] = {"value": nested}
    with pytest.raises(ProtocolError):
        encode(start)


def test_read_frame_never_consumes_the_next_frame() -> None:
    """A bounded line reader distinguishes clean EOF from a partial frame."""
    frame = FIXTURES["valid_frames"][0]
    wire = encode(frame)
    stream = io.BytesIO(wire + wire)
    assert read_frame(stream) == read_frame(stream) == frame
    with pytest.raises(EOFError):
        read_frame(stream)
    with pytest.raises(ProtocolError):
        read_frame(io.BytesIO(wire[:-1]))
    with pytest.raises(ProtocolError, match="FRAME_LIMIT"):
        read_frame(io.BytesIO(b"x" * (MAX_FRAME_BYTES + 1)))


@pytest.mark.parametrize(
    ("before", "field", "value", "now"),
    [
        (1, "request_id", "00000000-0000-4000-8000-000000000099", 1),
        (1, "binding.session_id", "00000000-0000-4000-8000-000000000099", 1),
        (1, "binding.run_id", "00000000-0000-4000-8000-000000000099", 1),
        (1, "binding.cursor_version", 1, 1),
        (1, "binding.fencing_token", 2, 1),
        (2, "parameter_hash", "a" * 64, 2),
        (3, "physical_call_id", "00000000-0000-4000-8000-000000000099", 3),
        (3, "usage.input_tokens", 101, 3),
        (1, "call_sequence", 1, 180000),
        (3, "http_status", 200, 60002),
        (2, "granted", True, 150000),
    ],
)
def test_identity_bounds_and_deadlines_fail_closed(
    before: int, field: str, value: Any, now: int
) -> None:
    """Identity and deadline mismatches poison the exchange before any send."""
    frames = copy.deepcopy(FIXTURES["valid_frames"])
    conversation = Conversation()
    for index in range(before):
        conversation.accept(frames[index], index)
        if index == 2:
            conversation.can_dispatch(frames[index]["physical_call_id"], index)
    target = frames[before]
    keys = field.split(".")
    for key in keys[:-1]:
        target = target[key]
    target[keys[-1]] = value
    with pytest.raises(ProtocolError):
        conversation.accept(frames[before], now)
    with pytest.raises(ProtocolError):
        conversation.accept(FIXTURES["valid_frames"][before], now)


@pytest.mark.parametrize(
    "scenario", ["replay", "expiry", "stop", "observation_before_send"]
)
def test_dispatch_is_single_use_and_stops_immediately(scenario: str) -> None:
    """Local guard rejects reused permits, expired dispatch and stopped work."""
    frames = copy.deepcopy(FIXTURES["valid_frames"])
    conversation = Conversation()
    for frame in frames[:3]:
        conversation.accept(frame, 0)
    now = 0
    if scenario == "replay":
        conversation.can_dispatch(frames[2]["physical_call_id"], 0)
    elif scenario == "expiry":
        now = 30000
    elif scenario == "stop":
        conversation.stop()
    else:
        with pytest.raises(ProtocolError):
            conversation.accept(frames[3], 1)
        return
    with pytest.raises(ProtocolError):
        conversation.can_dispatch(frames[2]["physical_call_id"], now)


@pytest.mark.parametrize(
    "scenario",
    [
        "parallel_intent",
        "early_result",
        "wrong_subcall",
        "new_tool_id",
        "free_usage",
        "repeat_call_id",
    ],
)
def test_policy_subcalls_remain_serial_and_bound(scenario: str) -> None:
    """All four policy HTTP requests bind one tool and distinct physical IDs."""
    frames = copy.deepcopy(
        next(
            case["frames"]
            for case in FIXTURES["valid_sequences"]
            if case["name"] == "four_policy_subcalls"
        )
    )
    before = {
        "parallel_intent": 2,
        "early_result": 1,
        "wrong_subcall": 1,
        "new_tool_id": 4,
        "free_usage": 3,
        "repeat_call_id": 5,
    }[scenario]
    conversation = Conversation()
    for index in range(before):
        conversation.accept(frames[index], index)
        if frames[index]["kind"] == "call_permit":
            conversation.can_dispatch(frames[index]["physical_call_id"], index)
    bad = frames[before]
    if scenario == "parallel_intent":
        bad = frames[1]
    elif scenario == "early_result":
        bad = frames[-1]
    elif scenario == "wrong_subcall":
        bad["subcall"] = "query_embedding"
    elif scenario == "new_tool_id":
        bad["tool_invocation_id"] = "00000000-0000-4000-8000-000000000088"
    elif scenario == "free_usage":
        bad["usage_known"] = True
        bad["usage"] = FIXTURES["valid_frames"][3]["usage"]
    else:
        bad["physical_call_id"] = frames[2]["physical_call_id"]
    with pytest.raises(ProtocolError):
        conversation.accept(bad, before)


def test_schema_matches_explicit_closed_wire_objects() -> None:
    """The reviewed source field sets match both shared fixtures and this codec."""
    source = json.loads((ROOT / "schema.json").read_text(encoding="utf-8"))
    assert source["x-max-frame-bytes"] == MAX_FRAME_BYTES
    definitions = source["$defs"]
    for frame in FIXTURES["valid_frames"]:
        definition = definitions[frame["kind"]]
        assert set(definition["required"]) == set(frame)
        assert set(definition["properties"]) == set(frame)
        assert not definition["additionalProperties"]
        assert set(definitions["binding"]["required"]) == set(frame["binding"])
    assert set(definitions["usage"]["required"]) == set(
        FIXTURES["valid_frames"][3]["usage"]
    )
