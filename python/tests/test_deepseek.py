"""DeepSeek adapter contracts using synthetic envelopes and real loopback TCP.

The provider and authorization/settlement coordinator below are explicit test
fixtures. These checks prove neither durable budget reservation nor real billing.
"""

from __future__ import annotations

import asyncio
import copy
import hashlib
import json
from dataclasses import FrozenInstanceError
from pathlib import Path
from typing import Any

import pytest
from http_fault_server import HTTPFaultServer, ReceivedRequest, respond
from jobforge_agent import dispatch as dispatch_module
from jobforge_agent.deepseek import (
    CHAT_PATH,
    MAX_CONTENT_BYTES,
    MAX_MESSAGE_BYTES,
    MAX_REQUEST_BYTES,
    MAX_RESPONSE_BYTES,
    MODEL,
    ORIGIN,
    DeepSeekChat,
    complete_usage,
    extract_chat_usage,
    prepare_chat_request,
    proposal_object,
)
from jobforge_agent.dispatch import (
    AuthorizedDispatcher,
    CompleteResponse,
    DispatchError,
    Endpoint,
    RunCallContext,
)
from jobforge_agent.errors import ToolError
from jobforge_agent.protocol_v2 import MAX_INTEGER, usage_hash

CALL_ID = "00000000-0000-4000-8000-000000000006"
CONTEXT = RunCallContext("a" * 64, "b" * 64, "")
MESSAGES = [{"role": "user", "content": "Return a synthetic JSON object."}]


def _envelope() -> dict[str, Any]:
    return {
        "id": "synthetic-chat-1",
        "object": "chat.completion",
        "created": 1,
        "model": MODEL,
        "system_fingerprint": "fp_synthetic",
        "choices": [
            {
                "index": 0,
                "finish_reason": "stop",
                "logprobs": None,
                "message": {"role": "assistant", "content": '{"answer":"synthetic"}'},
            }
        ],
        "usage": {
            "prompt_tokens": 10,
            "completion_tokens": 5,
            "total_tokens": 15,
            "prompt_cache_hit_tokens": 2,
            "prompt_cache_miss_tokens": 8,
            "prompt_tokens_details": {"cached_tokens": 2},
        },
    }


def _wire(value: Any) -> bytes:
    return json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode("utf-8")


def _digest(*fields: str) -> str:
    result = hashlib.sha256()
    for field in fields:
        data = field.encode("utf-8")
        result.update(len(data).to_bytes(8, "big"))
        result.update(data)
    return result.hexdigest()


def test_request_is_fixed_immutable_and_hashed_over_exact_bytes() -> None:
    """Provider settings and physical hashes cannot follow mutable model data."""
    messages = copy.deepcopy(MESSAGES)
    request = prepare_chat_request(messages, context=CONTEXT)
    messages[0]["content"] = "modified after preparation"
    assert ORIGIN == "https://api.deepseek.com"
    assert (request.endpoint, request.subcall, request.method, request.path) == (
        "deepseek",
        "chat",
        "POST",
        CHAT_PATH,
    )
    body = json.loads(request.body)
    assert body == {
        "model": "deepseek-flash",
        "messages": MESSAGES,
        "thinking": {"type": "disabled"},
        "stream": False,
        "max_tokens": 1024,
        "response_format": {"type": "json_object"},
        "temperature": 0,
    }
    assert request.parameter_hash == _digest(
        "jobforge.run.physical-input.v1",
        CONTEXT.run_profile_hash,
        CONTEXT.snapshot_content_hash,
        "chat",
        "POST",
        CHAT_PATH,
        hashlib.sha256(request.body).hexdigest(),
    )
    assert request.max_response_bytes == MAX_RESPONSE_BYTES
    with pytest.raises(FrozenInstanceError):
        request.body = b"changed"  # type: ignore[misc] -- Verify frozen ownership.


@pytest.mark.parametrize(
    "messages",
    [
        [],
        "not a message list",
        [None],
        [{"role": "tool", "content": "x"}],
        [{"role": "user", "content": []}],
        [{"role": "user", "content": "x", "name": "arbitrary"}],
        [{"role": "user", "content": "x", "model": "another-model"}],
        [{"role": "user", "content": "x", "url": "https://invalid.example"}],
        [{"role": "user", "content": "\ud800"}],
    ],
)
def test_invalid_message_shapes_never_prepare(messages: Any) -> None:
    """Only text roles and closed fields may enter the fixed request builder."""
    with pytest.raises(DispatchError, match="INPUT_INVALID") as caught:
        prepare_chat_request(messages, context=CONTEXT)
    assert caught.value.__context__ is None and caught.value.__cause__ is None


def test_request_limits_count_utf8_sum_and_serialized_escaping() -> None:
    """Size failures remain distinct from correctable model output failures."""
    boundary = [{"role": "user", "content": "x" * MAX_MESSAGE_BYTES}]
    assert (
        len(prepare_chat_request(boundary, context=CONTEXT).body) <= MAX_REQUEST_BYTES
    )
    for content in (
        "x" * (MAX_MESSAGE_BYTES + 1),
        "界" * 5462,
        "\0" * MAX_MESSAGE_BYTES,
    ):
        with pytest.raises(DispatchError) as caught:
            prepare_chat_request(
                [{"role": "user", "content": content}], context=CONTEXT
            )
        assert caught.value.fact == "size_limit"
        assert caught.value.stop
    messages = [
        {"role": role, "content": "x" * (MAX_MESSAGE_BYTES // 2 + 1)}
        for role in ("system", "user")
    ]
    with pytest.raises(DispatchError) as caught:
        prepare_chat_request(messages, context=CONTEXT)
    assert caught.value.fact == "size_limit"


def test_usage_receipt_binds_call_full_digest_and_identity_without_text() -> None:
    """Receipt identities retain no provider body, and usage does not self-price."""
    raw = _wire(_envelope())
    report = complete_usage(raw, physical_call_id=CALL_ID)
    assert report is not None
    assert (report.input_tokens, report.output_tokens, report.cached_input_tokens) == (
        10,
        5,
        2,
    )
    assert report.reasoning_tokens is None
    assert report.receipt_hash == _digest(
        "jobforge.deepseek.receipt.v1",
        CALL_ID,
        hashlib.sha256(raw).hexdigest(),
        "synthetic-chat-1",
        MODEL,
        "fp_synthetic",
        "1",
    )
    assert report.usage_hash == usage_hash(
        {
            "input_tokens": 10,
            "output_tokens": 5,
            "cached_input_tokens": 2,
            "receipt_hash": report.receipt_hash,
        }
    )
    changed_call = complete_usage(raw, physical_call_id=CALL_ID[:-1] + "7")
    changed_raw = complete_usage(raw + b" ", physical_call_id=CALL_ID)
    assert changed_call is not None and changed_call.receipt_hash != report.receipt_hash
    assert changed_raw is not None and changed_raw.receipt_hash != report.receipt_hash
    assert "answer" not in repr(report)
    evidence = extract_chat_usage(CompleteResponse(200, raw, CALL_ID, "d" * 64))
    assert evidence is not None
    assert (
        json.loads(evidence.provider_identity)["sha256"]
        == hashlib.sha256(raw).hexdigest()
    )
    assert "answer" not in evidence.provider_identity


def test_maximum_provider_identifiers_stay_within_audit_limit() -> None:
    """The identity representation stays bounded without truncating identifiers."""
    value = _envelope()
    value.update(id="r" * 128, system_fingerprint="f" * 128, created=MAX_INTEGER)
    evidence = extract_chat_usage(
        CompleteResponse(200, _wire(value), CALL_ID, "d" * 64)
    )
    assert evidence is not None
    assert len(evidence.provider_identity.encode("utf-8")) <= 512
    assert json.loads(evidence.provider_identity)["id"] == "r" * 128


@pytest.mark.parametrize("tokens", [1025, MAX_INTEGER - 10])
def test_complete_overrun_usage_is_never_clamped(tokens: int) -> None:
    """Per-field valid over-reservation counts reach the original ledger report."""
    value = _envelope()
    value["usage"]["completion_tokens"] = tokens
    value["usage"]["total_tokens"] = tokens + 10
    report = complete_usage(_wire(value), physical_call_id=CALL_ID)
    assert report is not None and report.output_tokens == tokens


@pytest.mark.parametrize(
    "field",
    [
        "prompt_tokens",
        "completion_tokens",
        "total_tokens",
        "prompt_cache_hit_tokens",
        "prompt_cache_miss_tokens",
    ],
)
@pytest.mark.parametrize("bad", [-1, True, 1.0, None, MAX_INTEGER + 1])
def test_usage_counters_require_exact_nonnegative_safeints(
    field: str, bad: Any
) -> None:
    """Lossy or missing metering is unknown instead of a fabricated zero report."""
    value = _envelope()
    value["usage"][field] = bad
    assert complete_usage(_wire(value), physical_call_id=CALL_ID) is None


@pytest.mark.parametrize(
    "scenario",
    [
        "missing",
        "null",
        "missing_counter",
        "cache_sum",
        "total_sum",
        "cached_detail",
        "cached_bool",
        "null_details",
        "reasoning_too_large",
        "reasoning_bool",
        "reasoning_null",
        "unknown_usage_field",
        "unsafe_total",
    ],
)
def test_incomplete_or_inconsistent_usage_remains_unknown(scenario: str) -> None:
    """Provider total itself must be safe even though v2 retains anomalous sums."""
    value = _envelope()
    usage = value["usage"]
    if scenario == "missing":
        del value["usage"]
    elif scenario == "null":
        value["usage"] = None
    elif scenario == "missing_counter":
        del usage["completion_tokens"]
    elif scenario == "cache_sum":
        usage["prompt_cache_miss_tokens"] += 1
    elif scenario == "total_sum":
        usage["total_tokens"] += 1
    elif scenario == "cached_detail":
        usage["prompt_tokens_details"]["cached_tokens"] += 1
    elif scenario == "cached_bool":
        usage["prompt_tokens_details"]["cached_tokens"] = True
    elif scenario == "null_details":
        usage["prompt_tokens_details"] = None
    elif scenario.startswith("reasoning_"):
        usage["completion_tokens_details"] = {
            "reasoning_tokens": {
                "reasoning_too_large": 6,
                "reasoning_bool": True,
                "reasoning_null": None,
            }[scenario]
        }
    elif scenario == "unknown_usage_field":
        usage["price"] = 0
    else:
        usage.update(
            prompt_tokens=MAX_INTEGER,
            prompt_cache_hit_tokens=0,
            prompt_cache_miss_tokens=MAX_INTEGER,
            total_tokens=MAX_INTEGER + 5,
        )
    assert complete_usage(_wire(value), physical_call_id=CALL_ID) is None


@pytest.mark.parametrize("reasoning", [0, 3])
def test_observed_reasoning_is_preserved_without_double_counting(
    reasoning: int,
) -> None:
    """Optional reasoning metadata remains distinct from total billed completion."""
    value = _envelope()
    value["usage"]["completion_tokens_details"] = {"reasoning_tokens": reasoning}
    report = complete_usage(_wire(value), physical_call_id=CALL_ID)
    assert report is not None
    assert report.reasoning_tokens == reasoning and report.output_tokens == 5
    if reasoning > 0:
        with pytest.raises(ToolError, match="PROFILE_UNAVAILABLE"):
            proposal_object(_wire(value))


@pytest.mark.parametrize(
    "field,bad",
    [
        ("model", "deepseek-v4-pro"),
        ("model", "DeepSeek-V4.1-Flash"),
        ("id", "x" * 129),
        ("id", "response\ntext"),
        ("system_fingerprint", None),
        ("system_fingerprint", ""),
        ("created", True),
        ("created", -1),
        ("object", "chat.completion.chunk"),
    ],
)
def test_incompatible_identity_cannot_supply_priced_usage(field: str, bad: Any) -> None:
    """An unexpected model/configuration keeps the full unknown hold."""
    value = _envelope()
    value[field] = bad
    raw = _wire(value)
    assert complete_usage(raw, physical_call_id=CALL_ID) is None
    with pytest.raises(ToolError, match="PROFILE_UNAVAILABLE"):
        proposal_object(raw)


@pytest.mark.parametrize(
    "mutate",
    [
        lambda value: value["choices"].append(copy.deepcopy(value["choices"][0])),
        lambda value: value["choices"][0].update(index=True),
        lambda value: value["choices"][0]["message"].update(role="user"),
        lambda value: value["choices"][0]["message"].update(content=123),
        lambda value: value.pop("choices"),
    ],
)
def test_malformed_model_envelope_preserves_trusted_usage(mutate: Any) -> None:
    """Wrong choices/messages cannot erase complete independently trusted usage."""
    value = _envelope()
    mutate(value)
    raw = _wire(value)
    assert complete_usage(raw, physical_call_id=CALL_ID) is not None
    with pytest.raises(ToolError) as caught:
        proposal_object(raw)
    assert "sensitive-test-sentinel" not in str(caught.value)


@pytest.mark.parametrize("choices", [None, [], [{"message": {"content": 123}}]])
def test_invalid_choices_still_keep_complete_usage(choices: Any) -> None:
    """Structural output errors never masquerade as missing provider counters."""
    value = _envelope()
    value["choices"] = choices
    raw = _wire(value)
    assert complete_usage(raw, physical_call_id=CALL_ID) is not None
    with pytest.raises(ToolError, match="PROTOCOL_ERROR"):
        proposal_object(raw)


def test_unknown_provider_envelope_fields_fail_closed() -> None:
    """Undeclared provider-level fields cannot enter the fixed identity contract."""
    value = _envelope()
    value["unexpected"] = "sensitive-test-sentinel"
    raw = _wire(value)
    assert complete_usage(raw, physical_call_id=CALL_ID) is None
    with pytest.raises(ToolError) as caught:
        proposal_object(raw)
    assert "sensitive-test-sentinel" not in str(caught.value)


@pytest.mark.parametrize("suffix", [b" {}", b"sensitive-test-sentinel", b"\xff"])
def test_complete_json_with_tail_never_salvages_usage(suffix: bytes) -> None:
    """No substring scan extracts apparently valid counters from a damaged frame."""
    assert complete_usage(_wire(_envelope()) + suffix, physical_call_id=CALL_ID) is None


@pytest.mark.parametrize(
    "damage", ["duplicate", "invalid_utf8", "surrogate", "truncated"]
)
def test_invalid_complete_envelope_never_salvages_usage(damage: str) -> None:
    """Only a complete strict JSON response can attest to canonical counters."""
    raw = _wire(_envelope())
    if damage == "duplicate":
        raw = raw.replace(b'"total_tokens":15', b'"total_tokens":15,"total_tokens":15')
    elif damage == "invalid_utf8":
        raw = raw.replace(b"synthetic-chat-1", b"invalid-\xff")
    elif damage == "surrogate":
        raw = raw.replace(b"synthetic-chat-1", b"invalid-\\ud800")
    else:
        raw = raw[:-2]
    assert complete_usage(raw, physical_call_id=CALL_ID) is None
    with pytest.raises(ToolError, match="PROTOCOL_ERROR"):
        proposal_object(raw)


@pytest.mark.parametrize(
    "content",
    [
        '{"answer":1,"answer":2}',
        "{} {}",
        "[]",
        "not JSON",
        '{"a":NaN}',
        '{"a":"\\ud800"}',
        '{"a":' + "[" * 65 + "0" + "]" * 65 + "}",
    ],
)
def test_invalid_content_keeps_complete_usage(content: str) -> None:
    """Content parsing is separate from the trusted complete provider envelope."""
    value = _envelope()
    value["choices"][0]["message"]["content"] = content
    raw = _wire(value)
    assert complete_usage(raw, physical_call_id=CALL_ID) is not None
    with pytest.raises(ToolError, match="OUTPUT_INVALID"):
        proposal_object(raw)


@pytest.mark.parametrize(
    "reason", ["length", "content_filter", "aborted", "unknown", None]
)
def test_invalid_finish_reason_does_not_discard_usage(reason: Any) -> None:
    """A complete billed response need not contain a successful model output."""
    value = _envelope()
    value["choices"][0]["finish_reason"] = reason
    raw = _wire(value)
    assert complete_usage(raw, physical_call_id=CALL_ID) is not None
    with pytest.raises(ToolError, match="OUTPUT_INVALID"):
        proposal_object(raw)


def test_content_and_envelope_byte_limits_do_not_truncate() -> None:
    """An oversized model body is a size fact; no corrected/truncated JSON exists."""
    value = _envelope()
    value["choices"][0]["message"]["content"] = (
        '{"x":"' + "x" * (MAX_CONTENT_BYTES - 8) + '"}'
    )
    assert len(value["choices"][0]["message"]["content"].encode()) == MAX_CONTENT_BYTES
    assert proposal_object(_wire(value))["x"]
    value["choices"][0]["message"]["content"] += " "
    raw = _wire(value)
    assert complete_usage(raw, physical_call_id=CALL_ID) is not None
    with pytest.raises(ToolError, match="SIZE_LIMIT"):
        proposal_object(raw)
    huge = raw + b" " * MAX_RESPONSE_BYTES
    assert complete_usage(huge, physical_call_id=CALL_ID) is None
    with pytest.raises(ToolError, match="SIZE_LIMIT"):
        proposal_object(huge)


@pytest.mark.parametrize(
    "field,value",
    [
        ("reasoning_content", "unexpected reasoning"),
        ("tool_calls", [{"id": "unexpected-tool-call"}]),
    ],
)
def test_unsupported_thinking_or_tools_never_gain_execution(
    field: str, value: Any
) -> None:
    """Unexpected provider features stop output without erasing billed counters."""
    envelope = _envelope()
    envelope["choices"][0]["message"][field] = value
    raw = _wire(envelope)
    assert complete_usage(raw, physical_call_id=CALL_ID) is not None
    with pytest.raises(ToolError, match="PROTOCOL_ERROR"):
        proposal_object(raw)


class _FakeCoordinator:
    """Issue synthetic permits and acknowledgements, without a control database."""

    def __init__(self, *, pause_settlement: bool = False) -> None:
        self.intents: list[dict[str, Any]] = []
        self.observations: list[dict[str, Any]] = []
        self.reports: list[dict[str, Any]] = []
        self.pause_settlement = pause_settlement
        self.settlement_entered = asyncio.Event()
        self.release_settlement = asyncio.Event()

    async def authorize(self, intent: dict[str, Any]) -> dict[str, Any]:
        self.intents.append(copy.deepcopy(intent))
        return {
            **copy.deepcopy(intent),
            "kind": "call_permit",
            "granted": True,
            "error_code": "",
            "physical_call_id": CALL_ID,
            "dispatch_ms": 10000,
            "call_ms": 10000,
            "input_token_limit": 100,
            "output_token_limit": 1024,
        }

    async def observe(self, observation: dict[str, Any]) -> None:
        self.observations.append(copy.deepcopy(observation))

    async def settle(self, report: dict[str, Any]) -> dict[str, Any]:
        self.reports.append(copy.deepcopy(report))
        self.settlement_entered.set()
        if self.pause_settlement:
            await self.release_settlement.wait()
        return {
            "version": 2,
            "kind": "metering_ack",
            "request_id": report["request_id"],
            "binding": copy.deepcopy(report["binding"]),
            "emitted_mono_ms": 1000,
            "call_sequence": report["call_sequence"],
            "physical_call_id": CALL_ID,
            "usage_hash": report["usage"]["usage_hash"],
            "settlement": "anomaly"
            if report["usage"]["output_tokens"] > 1024
            else "settled",
        }


def _dispatcher(
    server: HTTPFaultServer, hooks: _FakeCoordinator
) -> AuthorizedDispatcher:
    fixtures = (
        Path(__file__).resolve().parents[2] / "api/executor/v2/fixtures/frames.json"
    )
    start = json.loads(fixtures.read_text(encoding="utf-8"))["valid_frames"][0]
    start["emitted_mono_ms"] = 1000
    return AuthorizedDispatcher(
        start,
        hooks=hooks,
        endpoints={"deepseek": Endpoint(server.origin, "dummy-test-bearer")},
        clock=lambda: 1000,
    )


@pytest.mark.parametrize(
    "scenario",
    [
        "valid",
        "bad_content",
        "schema_rejection",
        "finish_length",
        "overrun",
        "unknown_usage",
        "wrong_identity",
        "bad_choices",
        "size_limit",
        "reasoning_enabled",
        "http429",
        "http503",
    ],
)
def test_real_tcp_chat_captures_usage_before_independent_validation(
    monkeypatch: pytest.MonkeyPatch, scenario: str
) -> None:
    """Real local bytes traverse one permit and metering/observation barriers."""

    async def run() -> None:
        value = _envelope()
        if scenario == "bad_content":
            value["choices"][0]["message"]["content"] = "invalid model JSON"
        elif scenario == "finish_length":
            value["choices"][0]["finish_reason"] = "length"
        elif scenario == "overrun":
            value["usage"].update(completion_tokens=1025, total_tokens=1035)
        elif scenario == "unknown_usage":
            del value["usage"]
        elif scenario == "wrong_identity":
            value["model"] = "deepseek-v4-pro"
        elif scenario == "bad_choices":
            value["choices"] = []
        elif scenario == "size_limit":
            value["choices"][0]["message"]["content"] = "x" * (MAX_CONTENT_BYTES + 1)
        elif scenario == "reasoning_enabled":
            value["usage"]["completion_tokens_details"] = {"reasoning_tokens": 3}

        async def handler(
            _reader: asyncio.StreamReader,
            writer: asyncio.StreamWriter,
            _request: ReceivedRequest,
        ) -> None:
            status = int(scenario[4:]) if scenario.startswith("http") else 200
            await respond(writer, body=_wire(value), status=status)

        async with HTTPFaultServer(handler) as server:
            monkeypatch.setattr(dispatch_module, "DEEPSEEK_ORIGIN", server.origin)
            hooks = _FakeCoordinator()
            dispatcher = _dispatcher(server, hooks)
            adapter = DeepSeekChat(dispatcher)

            def validate(proposal: dict[str, Any]) -> str:
                if scenario != "unknown_usage":
                    assert len(dispatcher.recorded_usage()) == 1
                if scenario == "schema_rejection":
                    raise ValueError("sensitive-test-sentinel")
                if proposal != {"answer": "synthetic"}:
                    raise ValueError("invalid synthetic schema")
                return "accepted synthetic proposal"

            try:
                if scenario in {"valid", "unknown_usage"}:
                    assert (
                        await adapter.propose(
                            MESSAGES, context=CONTEXT, validate_proposal=validate
                        )
                        == "accepted synthetic proposal"
                    )
                else:
                    with pytest.raises(DispatchError) as caught:
                        await adapter.propose(
                            MESSAGES, context=CONTEXT, validate_proposal=validate
                        )
                    assert "sensitive-test-sentinel" not in str(caught.value)
                    if scenario in {"schema_rejection", "bad_content"}:
                        assert caught.value.__context__ is None
                        assert caught.value.__cause__ is None
                    if scenario == "size_limit":
                        assert caught.value.fact == "size_limit"
                    if scenario in {
                        "bad_content",
                        "schema_rejection",
                        "finish_length",
                        "http429",
                        "http503",
                    }:
                        # C1 blocks further calls after a confirmed rejection,
                        # while preserving the error step_result path.
                        assert not dispatcher.closed
                        assert hooks.observations[0]["business_outcome"] == "rejected"
                    else:
                        assert dispatcher.closed
                assert len(server.requests) == len(hooks.intents) == 1
                expected = prepare_chat_request(MESSAGES, context=CONTEXT)
                assert server.requests[0].body == expected.body
                assert hooks.intents[0]["parameter_hash"] == expected.parameter_hash
                assert server.requests[0].path == CHAT_PATH
                if scenario in {
                    "unknown_usage",
                    "wrong_identity",
                    "http429",
                    "http503",
                }:
                    assert not dispatcher.recorded_usage() and not hooks.reports
                    assert not dispatcher.recorded_audit()
                else:
                    assert len(dispatcher.recorded_usage()) == len(hooks.reports) == 1
                    expected_output = 1025 if scenario == "overrun" else 5
                    assert hooks.reports[0]["usage"]["output_tokens"] == expected_output
                    audit = dispatcher.recorded_audit()[0]
                    assert audit.physical_call_id == CALL_ID
                    assert (
                        audit.receipt_hash == hooks.reports[0]["usage"]["receipt_hash"]
                    )
                    assert json.loads(audit.provider_identity)["model"] == MODEL
                    assert (
                        json.loads(audit.provider_identity)["sha256"]
                        == hashlib.sha256(_wire(value)).hexdigest()
                    )
                    assert audit.reasoning_tokens == (
                        3 if scenario == "reasoning_enabled" else None
                    )
                    assert "answer" not in audit.provider_identity
                if scenario in {"bad_content", "schema_rejection", "finish_length"}:
                    assert hooks.observations[0]["business_outcome"] == "rejected"
                    assert hooks.observations[0]["usage_disposition"] == "reported"
                if scenario in {
                    "size_limit",
                    "wrong_identity",
                    "overrun",
                    "bad_choices",
                    "reasoning_enabled",
                }:
                    assert not hooks.observations
                if scenario.startswith("http"):
                    assert hooks.observations[0]["transport_outcome"] == "response"
                    assert hooks.observations[0]["business_outcome"] == "rejected"
                    assert hooks.observations[0]["usage_disposition"] == "unknown"
                if scenario == "unknown_usage":
                    assert hooks.observations[0]["usage_disposition"] == "unknown"
                if scenario not in {"valid", "unknown_usage"}:
                    with pytest.raises(DispatchError):
                        await adapter.propose(
                            MESSAGES, context=CONTEXT, validate_proposal=validate
                        )
                    assert len(server.requests) == len(hooks.intents) == 1
                    assert dispatcher.closed
            finally:
                await dispatcher.aclose()

    asyncio.run(asyncio.wait_for(run(), timeout=5))


def test_cancel_during_settlement_keeps_already_captured_usage(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Cancellation stops waiting without promising provider withdrawal or refund."""

    async def run() -> None:
        async def handler(
            _reader: asyncio.StreamReader,
            writer: asyncio.StreamWriter,
            _request: ReceivedRequest,
        ) -> None:
            await respond(writer, body=_wire(_envelope()))

        async with HTTPFaultServer(handler) as server:
            monkeypatch.setattr(dispatch_module, "DEEPSEEK_ORIGIN", server.origin)
            hooks = _FakeCoordinator(pause_settlement=True)
            dispatcher = _dispatcher(server, hooks)
            task = asyncio.create_task(
                DeepSeekChat(dispatcher).propose(
                    MESSAGES,
                    context=CONTEXT,
                    validate_proposal=lambda value: value,
                )
            )
            try:
                await hooks.settlement_entered.wait()
                assert dispatcher.recorded_usage()[0]["usage"]["output_tokens"] == 5
                task.cancel()
                with pytest.raises(asyncio.CancelledError):
                    await task
                assert dispatcher.closed and len(server.requests) == 1
                assert dispatcher.recorded_usage()[0]["usage"]["output_tokens"] == 5
                assert not hooks.observations
                assert dispatcher.recorded_audit()[0].physical_call_id == CALL_ID
            finally:
                await dispatcher.aclose()
                await asyncio.gather(task, return_exceptions=True)

    asyncio.run(asyncio.wait_for(run(), timeout=5))
