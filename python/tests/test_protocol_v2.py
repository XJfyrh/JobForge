"""Shared v2 wire fixtures and execution/metering boundary regressions."""

import copy
import io
import json
from pathlib import Path
from typing import Any

import pytest
from jobforge_agent.protocol_v2 import (
    MAX_CHECKPOINT_BYTES,
    MAX_FRAME_BYTES,
    MAX_INTEGER,
    MAX_METERING_FRAME_BYTES,
    Conversation,
    ProtocolError,
    decode,
    decode_metering,
    encode,
    encode_metering,
    observation_error_code,
    observation_hash,
    read_frame,
    read_metering_frame,
    report_hash,
    usage_hash,
)

ROOT = Path(__file__).resolve().parents[2] / "api" / "executor" / "v2"
CORPORA = [
    json.loads((ROOT / "fixtures" / name).read_text(encoding="utf-8"))
    for name in ("frames.json", "audit-frames.json")
]
FIXTURES = {
    field: [entry for corpus in CORPORA for entry in corpus.get(field, [])]
    for field in (
        "valid_frames",
        "invalid_frames",
        "observation_hash_cases",
        "conversations",
    )
}
Frame = dict[str, Any]


def _frames() -> dict[str, Frame]:
    frames: dict[str, Frame] = {}
    for frame in FIXTURES["valid_frames"]:
        frames.setdefault(frame["kind"], copy.deepcopy(frame))
    for frame in frames.values():
        frame["emitted_mono_ms"] = 1000
    return frames


def _started(frames: dict[str, Frame], *, dispatch: bool = True) -> Conversation:
    conversation = Conversation()
    for kind in ("execute_step", "call_intent", "call_permit"):
        conversation.accept(frames[kind], 1000)
    if dispatch:
        conversation.can_dispatch(frames["call_permit"]["physical_call_id"], 1000)
    return conversation


def _free_frames() -> dict[str, Frame]:
    frames = _frames()
    for frame in frames.values():
        frame["binding"]["step_kind"] = "get_order"
    for name in ("call_intent", "call_permit"):
        frames[name].update(
            subcall="get_order",
            tool_invocation_id="00000000-0000-4000-8000-000000000088",
        )
    frames["call_permit"].update(
        input_token_limit=0, output_token_limit=0, call_ms=10000, dispatch_ms=1000
    )
    frames["call_observation"].update(
        usage_disposition="unknown", usage_hash=None, audit_hash=None
    )
    return frames


def _observation_ack(observation: Frame, emitted: int = 1000) -> Frame:
    return {
        "version": 2,
        "kind": "call_observation_ack",
        "request_id": observation["request_id"],
        "binding": copy.deepcopy(observation["binding"]),
        "emitted_mono_ms": emitted,
        "call_sequence": observation["call_sequence"],
        "physical_call_id": observation["physical_call_id"],
        "observation_hash": observation_hash(observation),
    }


@pytest.mark.parametrize("frame", FIXTURES["valid_frames"], ids=lambda f: f["kind"])
def test_shared_valid_frames(frame: Frame) -> None:
    """Both language codecs consume the same frozen valid corpus."""
    if frame["kind"].startswith("metering_"):
        assert decode_metering(encode_metering(frame)) == frame
    else:
        assert decode(encode(frame)) == frame


@pytest.mark.parametrize("case", FIXTURES["invalid_frames"], ids=lambda f: f["name"])
def test_shared_invalid_frames(case: Frame) -> None:
    """The source corpus rejects malformed, noncanonical, and cross-pipe data."""
    decoder = decode_metering if case.get("channel") == "metering" else decode
    with pytest.raises(ProtocolError):
        decoder(case["wire"].encode("utf-8"))


@pytest.mark.parametrize(
    "case", FIXTURES["observation_hash_cases"], ids=lambda case: case["name"]
)
def test_shared_observation_hash_uses_domain_errors(case: Frame) -> None:
    """Go, Python and the durable ledger use identical mapped hash bytes."""
    if not case["accept"]:
        with pytest.raises(ProtocolError):
            observation_hash(case["frame"])
        return
    observation = decode(encode(case["frame"]))
    assert observation_error_code(observation["error_code"]) == case["domain_error"]
    assert observation_hash(observation) == case["hash"]


@pytest.mark.parametrize("case", FIXTURES["conversations"], ids=lambda f: f["name"])
def test_shared_conversation_events(case: Frame) -> None:
    """Each shared event has the same acceptance result in Go and Python."""
    conversation = Conversation()
    for event in case["events"]:
        try:
            if event["op"] == "ordinary":
                conversation.accept(event["frame"], event["now_mono_ms"])
            elif event["op"] == "metering":
                conversation.accept_metering(event["frame"], event["now_mono_ms"])
            elif event["op"] == "dispatch":
                conversation.can_dispatch(
                    event["physical_call_id"], event["now_mono_ms"]
                )
            else:
                assert event["op"] == "stop"
                conversation.stop()
        except ProtocolError:
            assert not event["accept"], event
        else:
            assert event["accept"], event


def test_schema_closed_field_sets_and_bounded_channels() -> None:
    """The human-reviewed source explicitly closes every wire object."""
    source = json.loads((ROOT / "schema.json").read_text(encoding="utf-8"))
    assert source["x-max-frame-bytes"] == MAX_FRAME_BYTES
    assert source["x-max-metering-frame-bytes"] == MAX_METERING_FRAME_BYTES
    definitions = source["$defs"]
    for frame in FIXTURES["valid_frames"]:
        definition = definitions[frame["kind"]]
        assert (
            set(definition["properties"]) == set(definition["required"]) == set(frame)
        )
        assert not definition["additionalProperties"]
        assert set(definitions["binding"]["required"]) == set(frame["binding"])
    assert set(definitions["usage"]["required"]) == set(
        _frames()["metering_report"]["usage"]
    )


@pytest.mark.parametrize("metering", [False, True])
def test_bounded_readers_and_wrong_pipe(metering: bool) -> None:
    """Readers consume one line and reject incomplete or wrong-pipe frames."""
    frames = _frames()
    frame = frames["metering_report" if metering else "execute_step"]
    encoder = encode_metering if metering else encode
    decoder = decode_metering if metering else decode
    reader = read_metering_frame if metering else read_frame
    other_reader = read_frame if metering else read_metering_frame
    maximum = MAX_METERING_FRAME_BYTES if metering else MAX_FRAME_BYTES
    wire = encoder(frame)
    stream = io.BytesIO(wire + wire)
    assert reader(stream) == reader(stream) == frame
    with pytest.raises(EOFError):
        reader(stream)
    for invalid in (wire[:-1], b"\xff" + wire, wire + wire, wire[:-1] + b"secret\n"):
        with pytest.raises(ProtocolError) as error:
            decoder(invalid)
        assert "secret" not in str(error.value)
    with pytest.raises(ProtocolError):
        other_reader(io.BytesIO(wire))
    assert decoder(b" " * (maximum - len(wire)) + wire) == frame
    with pytest.raises(ProtocolError, match="FRAME_LIMIT"):
        reader(io.BytesIO(b" " * (maximum + 1 - len(wire)) + wire))


@pytest.mark.parametrize("value", [2**53 + 1, 10**300, -(10**300)])
def test_protected_integer_range_check_preserves_exact_value(value: int) -> None:
    """The finite-range check must not coerce protected integers to floats."""
    frame = _frames()["execute_step"]
    frame["input"] = {"value": value}
    parsed = decode(encode(frame))["input"]["value"]
    assert type(parsed) is int and parsed == value


def test_protected_raw_bytes_and_depth() -> None:
    """Checkpoint limits include source whitespace and escaped UTF-8 bytes."""
    frame = _frames()["execute_step"]
    frame["checkpoint"] = {"value": "x" * (MAX_CHECKPOINT_BYTES - 12)}
    encode(frame)
    frame["checkpoint"]["value"] += "x"
    with pytest.raises(ProtocolError):
        encode(frame)
    frame["checkpoint"] = {}
    wire = encode(frame)
    padded = wire.replace(
        b'"checkpoint":{}', b'"checkpoint":{' + b" " * MAX_CHECKPOINT_BYTES + b"}"
    )
    with pytest.raises(ProtocolError):
        decode(padded)
    nested: Any = 0
    for _ in range(65):
        nested = [nested]
    frame["checkpoint"] = {"value": nested}
    with pytest.raises(ProtocolError):
        encode(frame)


@pytest.mark.parametrize(
    "field", ["input_tokens", "output_tokens", "cached_input_tokens"]
)
@pytest.mark.parametrize("value", [-1, True, 1.0, MAX_INTEGER + 1, None])
def test_metering_requires_exact_safe_integers(field: str, value: Any) -> None:
    """Lossy, boolean, missing, negative, and overflowing counters never settle."""
    report = _frames()["metering_report"]
    report["usage"][field] = value
    with pytest.raises(ProtocolError):
        encode_metering(report)


@pytest.mark.parametrize(
    "scenario", ["cached_exceeds_input", "wrong_hash", "extra_secret"]
)
def test_metering_internal_consistency_and_closed_shape(scenario: str) -> None:
    """Narrow reports validate complete usage and never accept hidden fields."""
    report = _frames()["metering_report"]
    usage = report["usage"]
    if scenario == "cached_exceeds_input":
        usage["cached_input_tokens"] = usage["input_tokens"] + 1
    elif scenario == "extra_secret":
        report["secret"] = "sensitive-test-sentinel"
    else:
        usage["usage_hash"] = "a" * 64
    if scenario != "wrong_hash":
        usage["usage_hash"] = usage_hash(usage)
    with pytest.raises(ProtocolError) as error:
        encode_metering(report)
    assert str(error.value) == "PROTOCOL_ERROR"


@pytest.mark.parametrize("first", ["ordinary", "metering"])
@pytest.mark.parametrize("first_ack", ["ordinary", "metering"])
def test_report_and_observation_join_across_independent_pipes(
    first: str, first_ack: str
) -> None:
    """Older metering emissions can arrive after a later ordinary observation."""
    frames = _frames()
    conversation = _started(frames)
    report, ack, observation = (
        frames[kind] for kind in ("metering_report", "metering_ack", "call_observation")
    )
    observation["emitted_mono_ms"] = 1010
    if first == "ordinary":
        conversation.accept(observation, 1010)
        conversation.accept_metering(report, 1020)
    else:
        conversation.accept_metering(report, 1000)
        conversation.accept(observation, 1010)
    ack["emitted_mono_ms"] = 1020
    ordinary_ack = _observation_ack(observation, 1030)
    if first_ack == "ordinary":
        conversation.accept(ordinary_ack, 1030)
        assert conversation.metering_pending
        conversation.accept_metering(ack, 1040)
    else:
        conversation.accept_metering(ack, 1020)
        assert not conversation.metering_pending
        conversation.accept(ordinary_ack, 1040)
    frames["step_result"]["emitted_mono_ms"] = 1040
    conversation.accept(frames["step_result"], 1040)


@pytest.mark.parametrize("missing", ["report", "ack"])
def test_reported_observation_cannot_skip_settlement(missing: str) -> None:
    """A valid ordinary result cannot bypass its pending durable usage join."""
    frames = _frames()
    conversation = _started(frames)
    conversation.accept(frames["call_observation"], 1000)
    conversation.accept(_observation_ack(frames["call_observation"]), 1000)
    if missing == "ack":
        conversation.accept_metering(frames["metering_report"], 1000)
    with pytest.raises(ProtocolError):
        conversation.accept(frames["step_result"], 1000)
    if missing == "report":
        conversation.accept_metering(frames["metering_report"], 1000)
    conversation.accept_metering(frames["metering_ack"], 1000)
    with pytest.raises(ProtocolError):
        conversation.accept(frames["step_result"], 1000)


@pytest.mark.parametrize("end", ["stop", "expiry", "ordinary_rejection"])
def test_original_metering_survives_closure_without_reopening(end: str) -> None:
    """Closed ordinary execution preserves only the dispatched report/ACK path."""
    frames = _frames()
    conversation = _started(frames)
    now = 1000
    if end == "stop":
        conversation.stop()
    elif end == "expiry":
        now += frames["execute_step"]["remaining_ms"]
    else:
        bad = copy.deepcopy(frames["call_observation"])
        bad["binding"]["fencing_token"] += 1
        with pytest.raises(ProtocolError):
            conversation.accept(bad, now)
    conversation.accept_metering(frames["metering_report"], now)
    conversation.accept_metering(frames["metering_ack"], now)
    with pytest.raises(ProtocolError):
        conversation.accept(frames["call_observation"], now)


@pytest.mark.parametrize("field", ["input_tokens", "output_tokens"])
def test_overrun_is_metered_but_never_restores_execution(field: str) -> None:
    """Over-reservation counters are retained before an anomaly ACK arrives."""
    frames = _frames()
    conversation = _started(frames)
    report, ack = frames["metering_report"], frames["metering_ack"]
    limit = "input_token_limit" if field == "input_tokens" else "output_token_limit"
    report["usage"][field] = frames["call_permit"][limit] + 1
    report["usage"]["usage_hash"] = usage_hash(report["usage"])
    report["report_hash"] = report_hash(report)
    ack["report_hash"] = report["report_hash"]
    assert decode_metering(encode_metering(report)) == report
    conversation.accept_metering(report, 1000)
    assert conversation.closed
    with pytest.raises(ProtocolError):
        conversation.accept(frames["call_observation"], 1000)
    with pytest.raises(ProtocolError):
        conversation.accept_metering(ack, 1000)
    ack["settlement"] = "anomaly"
    conversation.accept_metering(ack, 1000)
    with pytest.raises(ProtocolError):
        conversation.accept(frames["step_result"], 1000)


def test_unconfirmed_can_confirm_but_never_reopen() -> None:
    """A later idempotent ledger confirmation does not regain execution rights."""
    frames = _frames()
    conversation = _started(frames)
    conversation.accept(frames["call_observation"], 1000)
    conversation.accept_metering(frames["metering_report"], 1000)
    frames["metering_ack"]["settlement"] = "unconfirmed"
    conversation.accept_metering(frames["metering_ack"], 1000)
    frames["metering_ack"]["settlement"] = "settled"
    conversation.accept_metering(frames["metering_ack"], 1000)
    with pytest.raises(ProtocolError):
        conversation.accept(frames["step_result"], 1000)


@pytest.mark.parametrize("output_tokens", [1, MAX_INTEGER])
def test_chat_fields_with_unsafe_sum_cannot_claim_complete_provider_usage(
    output_tokens: int,
) -> None:
    """Chat complete evidence requires the original aggregate total to be safe."""
    frames = _frames()
    report = frames["metering_report"]
    report["usage"]["input_tokens"] = MAX_INTEGER
    report["usage"]["output_tokens"] = output_tokens
    report["usage"]["usage_hash"] = usage_hash(report["usage"])
    with pytest.raises(ProtocolError):
        encode_metering(report)


def test_report_duplicates_are_idempotent_but_distinct_usage_conflicts() -> None:
    """One original call admits one complete report content across duplicates."""
    frames = _frames()
    conversation = _started(frames)
    report = frames["metering_report"]
    conversation.stop()
    conversation.accept_metering(report, 1000)
    conversation.accept_metering(copy.deepcopy(report), 1000)
    report["usage"]["output_tokens"] += 1
    report["usage"]["usage_hash"] = usage_hash(report["usage"])
    report["report_hash"] = report_hash(report)
    with pytest.raises(ProtocolError):
        conversation.accept_metering(report, 1000)


@pytest.mark.parametrize(
    "field",
    [
        "request_id",
        "physical_call_id",
        "parameter_hash",
        "call_sequence",
        "binding.fencing_token",
    ],
)
def test_stopped_metering_identity_stays_narrow(field: str) -> None:
    """Stopping never permits another identity, attempt, or unbound call."""
    frames = _frames()
    conversation = _started(frames)
    conversation.stop()
    report = frames["metering_report"]
    target = report
    keys = field.split(".")
    for key in keys[:-1]:
        target = target[key]
    previous = target[keys[-1]]
    target[keys[-1]] = (
        previous + 1
        if isinstance(previous, int)
        else (
            "e" * 64
            if field == "parameter_hash"
            else "00000000-0000-4000-8000-000000000099"
        )
    )
    with pytest.raises(ProtocolError):
        conversation.accept_metering(report, 1000)


def test_undispatched_permit_never_admits_metering() -> None:
    """A granted permit alone is insufficient evidence of a physical call."""
    frames = _frames()
    conversation = _started(frames, dispatch=False)
    conversation.stop()
    with pytest.raises(ProtocolError):
        conversation.accept_metering(frames["metering_report"], 1000)


@pytest.mark.parametrize("arrival", ["before_unknown", "after_unknown"])
def test_unknown_cannot_bypass_an_existing_report(arrival: str) -> None:
    """Unknown retains the hold and cannot hide a concurrently received report."""
    frames = _frames()
    conversation = _started(frames)
    observation = frames["call_observation"]
    observation["usage_disposition"], observation["usage_hash"] = "unknown", None
    if arrival == "before_unknown":
        conversation.accept_metering(frames["metering_report"], 1000)
        conversation.accept(observation, 1000)
    else:
        conversation.accept(observation, 1000)
        conversation.accept_metering(frames["metering_report"], 1000)
    with pytest.raises(ProtocolError):
        conversation.accept(frames["step_result"], 1000)


def test_free_call_without_report_can_finish_after_ordinary_ack() -> None:
    """Free calls do not fabricate audit reports or wait for a metering ACK."""
    frames = _free_frames()
    conversation = _started(frames)
    frames["call_observation"]["usage_disposition"] = "unknown"
    frames["call_observation"]["usage_hash"] = None
    conversation.accept(frames["call_observation"], 1000)
    conversation.accept(_observation_ack(frames["call_observation"]), 1000)
    conversation.accept(frames["step_result"], 1000)


@pytest.mark.parametrize(
    "scenario",
    [
        "start_queue",
        "permit_queue",
        "dispatch_queue",
        "call_queue",
        "settlement_queue",
        "future",
        "overflow",
        "clock_backward",
    ],
)
def test_anchored_deadlines_do_not_reset_on_queue_or_metering(scenario: str) -> None:
    """IPC delay, settlement waits, and corrupt clocks never create more time."""
    frames = _frames()
    conversation = Conversation()
    if scenario in {"start_queue", "future", "overflow"}:
        now = 1000
        if scenario == "start_queue":
            now += frames["execute_step"]["remaining_ms"]
        elif scenario == "future":
            frames["execute_step"]["emitted_mono_ms"] += 1
        else:
            now = MAX_INTEGER
            frames["execute_step"]["emitted_mono_ms"] = MAX_INTEGER
        with pytest.raises(ProtocolError):
            conversation.accept(frames["execute_step"], now)
        return
    conversation.accept(frames["execute_step"], 1000)
    conversation.accept(frames["call_intent"], 1000)
    if scenario == "permit_queue":
        with pytest.raises(ProtocolError):
            conversation.accept(
                frames["call_permit"], 1000 + frames["call_permit"]["dispatch_ms"]
            )
        return
    conversation.accept(frames["call_permit"], 1000)
    if scenario in {"dispatch_queue", "clock_backward"}:
        now = (
            999
            if scenario == "clock_backward"
            else 1000 + frames["call_permit"]["dispatch_ms"]
        )
        with pytest.raises(ProtocolError):
            conversation.can_dispatch(frames["call_permit"]["physical_call_id"], now)
        return
    conversation.can_dispatch(frames["call_permit"]["physical_call_id"], 1000)
    expiry = 1000 + frames["call_permit"]["call_ms"]
    if scenario == "call_queue":
        with pytest.raises(ProtocolError):
            conversation.accept(frames["call_observation"], expiry)
    else:
        conversation.accept(frames["call_observation"], 1000)
        conversation.accept_metering(frames["metering_report"], expiry)
        conversation.accept_metering(frames["metering_ack"], expiry)
        with pytest.raises(ProtocolError):
            conversation.accept(frames["step_result"], expiry)


@pytest.mark.parametrize("reported", [False, True])
def test_settlement_and_pipe_write_cannot_replace_observation_ack(
    reported: bool,
) -> None:
    """The last HTTP still needs its ordinary ACK even after complete settlement."""
    frames = _frames() if reported else _free_frames()
    conversation = _started(frames)
    observation = frames["call_observation"]
    if reported:
        conversation.accept_metering(frames["metering_report"], 1000)
        conversation.accept_metering(frames["metering_ack"], 1000)
    else:
        observation.update(usage_disposition="unknown", usage_hash=None)
    conversation.accept(observation, 1000)
    with pytest.raises(ProtocolError):
        conversation.accept(frames["step_result"], 1000)
    with pytest.raises(ProtocolError):
        conversation.accept(_observation_ack(observation), 1000)


@pytest.mark.parametrize(
    "field",
    [
        "request_id",
        "physical_call_id",
        "observation_hash",
        "call_sequence",
        "binding.fencing_token",
        "binding.worker_id",
        "binding.profile_hash",
        "binding.snapshot_hash",
    ],
)
def test_observation_ack_matches_full_original_identity_and_content(field: str) -> None:
    """An otherwise valid ACK cannot acknowledge a different call or authority."""
    frames = _free_frames()
    conversation = _started(frames)
    observation = frames["call_observation"]
    observation.update(usage_disposition="unknown", usage_hash=None)
    conversation.accept(observation, 1000)
    ack = _observation_ack(observation)
    target = ack
    keys = field.split(".")
    for key in keys[:-1]:
        target = target[key]
    previous = target[keys[-1]]
    if isinstance(previous, int):
        target[keys[-1]] += 1
    elif keys[-1].endswith("hash"):
        target[keys[-1]] = "e" * 64
    elif keys[-1] == "worker_id":
        target[keys[-1]] = "other-worker"
    else:
        target[keys[-1]] = "00000000-0000-4000-8000-000000000099"
    with pytest.raises(ProtocolError):
        conversation.accept(ack, 1000)
    assert conversation.closed


@pytest.mark.parametrize("pending", [False, True])
def test_duplicate_observation_ack_is_never_reusable(pending: bool) -> None:
    """A pending or already joined ACK is one use, unlike idempotent control RPCs."""
    frames = _frames() if pending else _free_frames()
    conversation = _started(frames)
    observation = frames["call_observation"]
    if not pending:
        observation.update(usage_disposition="unknown", usage_hash=None)
    conversation.accept(observation, 1000)
    ack = _observation_ack(observation)
    conversation.accept(ack, 1000)
    with pytest.raises(ProtocolError):
        conversation.accept(ack, 1000)
    if pending:
        conversation.accept_metering(frames["metering_report"], 1000)
        conversation.accept_metering(frames["metering_ack"], 1000)
    assert conversation.closed


@pytest.mark.parametrize("first", ["ordinary", "metering"])
def test_observation_ack_cannot_be_emitted_before_settlement(first: str) -> None:
    """FD read order is free; a durable observation cannot predate its settlement."""
    frames = _frames()
    conversation = _started(frames)
    conversation.accept(frames["call_observation"], 1000)
    conversation.accept_metering(frames["metering_report"], 1000)
    ordinary = _observation_ack(frames["call_observation"], 1010)
    frames["metering_ack"]["emitted_mono_ms"] = 1020
    if first == "ordinary":
        conversation.accept(ordinary, 1010)
        with pytest.raises(ProtocolError):
            conversation.accept_metering(frames["metering_ack"], 1020)
    else:
        conversation.accept_metering(frames["metering_ack"], 1020)
        with pytest.raises(ProtocolError):
            conversation.accept(ordinary, 1020)
    assert conversation.closed


@pytest.mark.parametrize("end", ["stop", "expiry", "anomaly", "unconfirmed"])
def test_pending_observation_ack_never_revives_after_loss_of_authority(
    end: str,
) -> None:
    """A valid late settlement preserves narrow metering only, never execution."""
    frames = _frames()
    conversation = _started(frames)
    conversation.accept(frames["call_observation"], 1000)
    conversation.accept(_observation_ack(frames["call_observation"]), 1000)
    now = 1000
    if end == "stop":
        conversation.stop()
    elif end == "expiry":
        now += frames["call_permit"]["call_ms"]
    else:
        frames["metering_ack"]["settlement"] = end
    conversation.accept_metering(frames["metering_report"], now)
    conversation.accept_metering(frames["metering_ack"], now)
    assert conversation.closed
    with pytest.raises(ProtocolError):
        conversation.accept(frames["step_result"], now)


@pytest.mark.parametrize(
    "scenario", ["before_observation", "future", "expired", "backward"]
)
def test_observation_ack_clock_and_original_deadline(scenario: str) -> None:
    """ACK receipt cannot refresh time or move the parent clock backwards."""
    frames = _free_frames()
    conversation = _started(frames)
    observation = frames["call_observation"]
    observation.update(
        usage_disposition="unknown", usage_hash=None, emitted_mono_ms=1010
    )
    conversation.accept(observation, 1010)
    ack = _observation_ack(observation, 1010)
    now = 1010
    if scenario == "before_observation":
        ack["emitted_mono_ms"] = 1000
    elif scenario == "future":
        ack["emitted_mono_ms"] = 1011
    elif scenario == "backward":
        ack["emitted_mono_ms"] = 999
    else:
        now = 1000 + frames["call_permit"]["call_ms"]
    with pytest.raises(ProtocolError):
        conversation.accept(ack, now)


@pytest.mark.parametrize("next_kind", ["call_intent", "step_result"])
@pytest.mark.parametrize("invalid", ["before_ack", "call_expired"])
def test_followup_requires_ack_causal_time_and_confirmation_deadline(
    next_kind: str, invalid: str
) -> None:
    """The first follow-up observes the previous call's conservative barrier."""
    frames = _frames()
    for frame in frames.values():
        frame["binding"]["step_kind"] = "search_policy"
    for kind in ("call_intent", "call_permit"):
        frames[kind].update(
            subcall="profile_version",
            tool_invocation_id="00000000-0000-4000-8000-000000000088",
        )
    frames["call_permit"].update(
        input_token_limit=0, output_token_limit=0, call_ms=1000
    )
    frames["call_permit"]["dispatch_ms"] = 500
    observation = frames["call_observation"]
    observation.update(usage_disposition="unknown", usage_hash=None, audit_hash=None)
    conversation = _started(frames)
    conversation.accept(observation, 1000)
    conversation.accept(_observation_ack(observation, 1010), 1010)
    following = frames[next_kind]
    if next_kind == "call_intent":
        following.update(call_sequence=2, subcall="profile_tags")
    else:
        following.update(outcome="error", error_code="OUTPUT_INVALID")
    following["emitted_mono_ms"] = 1005 if invalid == "before_ack" else 1010
    with pytest.raises(ProtocolError):
        conversation.accept(following, 1010 if invalid == "before_ack" else 2000)


def test_observation_and_pending_ack_are_immutable_snapshots() -> None:
    """Caller mutation cannot rewrite the expected hash or pending confirmation."""
    frames = _frames()
    conversation = _started(frames)
    observation = frames["call_observation"]
    ack = _observation_ack(observation)
    conversation.accept(observation, 1000)
    observation["http_status"] = 201
    conversation.accept(ack, 1000)
    ack["observation_hash"] = "e" * 64
    conversation.accept_metering(frames["metering_report"], 1000)
    conversation.accept_metering(frames["metering_ack"], 1000)
    conversation.accept(frames["step_result"], 1000)


def test_timely_next_intent_does_not_inherit_old_deadline_after_denied_permit() -> None:
    """A new control decision uses the step deadline after its prior ACK barrier."""
    frames = _frames()
    for frame in frames.values():
        frame["binding"]["step_kind"] = "search_policy"
    for kind in ("call_intent", "call_permit"):
        frames[kind].update(
            subcall="profile_version",
            tool_invocation_id="00000000-0000-4000-8000-000000000088",
        )
    permit = frames["call_permit"]
    permit.update(
        input_token_limit=0, output_token_limit=0, call_ms=1000, dispatch_ms=500
    )
    observation = frames["call_observation"]
    observation.update(usage_disposition="unknown", usage_hash=None, audit_hash=None)
    conversation = _started(frames)
    conversation.accept(observation, 1000)
    conversation.accept(_observation_ack(observation), 1000)
    frames["call_intent"].update(call_sequence=2, subcall="profile_tags")
    conversation.accept(frames["call_intent"], 1500)
    permit.update(
        call_sequence=2,
        subcall="profile_tags",
        granted=False,
        physical_call_id="",
        error_code="BUDGET_EXHAUSTED",
        emitted_mono_ms=2000,
        dispatch_ms=0,
        call_ms=0,
    )
    conversation.accept(permit, 2000)
    frames["step_result"].update(
        outcome="error", error_code="BUDGET_EXHAUSTED", emitted_mono_ms=2000
    )
    conversation.accept(frames["step_result"], 2000)
