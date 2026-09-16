"""Synthetic pure provider facts; these tests make no runtime or cloud claim."""

from __future__ import annotations

import copy
import hashlib
import json
from dataclasses import FrozenInstanceError, replace
from typing import Any

import pytest
from jobforge_agent.provider_audit import (
    MAX_AUDIT_BYTES,
    MAX_INTEGER,
    MAX_RESPONSE_BYTES,
    CallReport,
    ProviderAudit,
    ProviderAuditError,
    ReportBinding,
    UsageReport,
    capture_chat_report,
    decode_call_report,
    decode_provider_audit,
    encode_call_report,
    encode_provider_audit,
    execution_binding_hash,
    observation_hash_v2,
)

CALL_ID = "00000000-0000-4000-8000-000000000006"
MODEL = "deepseek-flash"
SENTINEL = "synthetic-sensitive-text-never-in-audit"


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


def _capture(value: Any) -> CallReport:
    return _capture_bytes(_wire(value))


def _capture_bytes(raw: bytes | None, status: int = 200) -> CallReport:
    return capture_chat_report(
        raw, http_status=status, physical_call_id=CALL_ID, expected_response_model=MODEL
    )


def _audit(report: CallReport) -> ProviderAudit:
    assert report.provider_audit is not None
    return report.provider_audit


def _signed(audit: ProviderAudit, **changes: Any) -> ProviderAudit:
    changed = replace(audit, **changes)
    return replace(changed, audit_hash=changed.hash())


def _binding(**changes: Any) -> ReportBinding:
    return replace(ReportBinding("a" * 64, CALL_ID, "b" * 64, "chat", MODEL), **changes)


def _digest(*fields: str) -> str:
    digest = hashlib.sha256()
    for field in fields:
        raw = field.encode("utf-8")
        digest.update(len(raw).to_bytes(8, "big"))
        digest.update(raw)
    return digest.hexdigest()


@pytest.mark.parametrize(
    "body", [None, b"x" * (MAX_RESPONSE_BYTES + 1)], ids=["incomplete", "over-limit"]
)
@pytest.mark.parametrize("status", [0, 200, 502])
def test_incomplete_response_never_records_partial_digest_or_status(
    body: bytes | None, status: int
) -> None:
    """Timeout/truncation/overlimit facts cannot pretend a complete response."""
    report = _capture_bytes(body, status)
    audit = _audit(report)
    assert not audit.response_complete and audit.http_status == 0
    assert report.usage is None
    assert audit.response_sha256 is None
    assert (
        audit.identity_state
        == audit.usage_evidence
        == audit.reasoning_state
        == audit.mode_state
        == "unavailable"
    )
    assert (
        audit.response_id
        is audit.response_model
        is audit.system_fingerprint
        is audit.created
        is audit.reasoning_tokens
        is None
    )
    assert decode_call_report(encode_call_report(report)) == report


@pytest.mark.parametrize("status", [100, 199, 201, 400, 429, 500, 599])
def test_non200_body_is_hashed_but_never_interpreted_as_chat(status: int) -> None:
    """Even an error body resembling a completion supplies no billing identity."""
    value = _envelope()
    value["error"] = SENTINEL
    raw = _wire(value)
    report = _capture_bytes(raw, status)
    audit = _audit(report)
    assert audit.response_complete and audit.http_status == status
    assert audit.response_sha256 == hashlib.sha256(raw).hexdigest()
    assert (
        audit.identity_state
        == audit.usage_evidence
        == audit.reasoning_state
        == audit.mode_state
        == "unavailable"
    )
    assert report.usage is None and audit.response_id is None
    assert SENTINEL not in repr(report) and SENTINEL.encode() not in encode_call_report(
        report
    )


@pytest.mark.parametrize("status", [-1, 0, 99, 600, True, 200.0, "200", None])
def test_complete_response_requires_an_exact_http_status(status: Any) -> None:
    """A complete body cannot turn an invalid adapter status into a fact."""
    with pytest.raises(ProviderAuditError):
        _capture_bytes(b"{}", status)


def test_bound_hashes_use_complete_original_bytes_and_actual_identity() -> None:
    """Receipt, usage, audit and report each bind their fixed ordered inputs."""
    raw = _wire(_envelope())
    report = _capture_bytes(raw)
    audit, usage = _audit(report), report.usage
    assert usage is not None
    assert audit.audit_hash == _digest(
        "jobforge.run.provider-audit.v1",
        "1",
        "deepseek",
        "1",
        "200",
        hashlib.sha256(raw).hexdigest(),
        "compatible",
        "synthetic-chat-1",
        MODEL,
        "fp_synthetic",
        "1",
        "complete",
        "absent",
        "",
        "nonthinking",
    )
    assert usage.receipt_hash == _digest(
        "jobforge.deepseek.receipt.v1",
        CALL_ID,
        hashlib.sha256(raw).hexdigest(),
        "synthetic-chat-1",
        MODEL,
        "fp_synthetic",
        "1",
    )
    assert usage.usage_hash == _digest(
        "jobforge.run.usage.v1", "10", "5", "2", usage.receipt_hash
    )
    binding = _binding()
    expected = _digest(
        "jobforge.run.call-report.v1",
        "a" * 64,
        CALL_ID,
        "b" * 64,
        usage.usage_hash,
        audit.audit_hash,
    )
    assert report.hash(binding) == expected
    report.verify(binding, expected)
    with pytest.raises(ProviderAuditError):
        report.verify(binding, "0" * 64)
    assert _capture_bytes(raw + b" ").usage != usage
    assert decode_call_report(encode_call_report(report)) == report


def test_safe_incompatible_model_preserves_complete_observed_counts() -> None:
    """An actual different model affects hashes without losing valid metering."""
    value = _envelope()
    value["model"] = "different-model"
    report = _capture(value)
    audit = _audit(report)
    assert (
        audit.identity_state == "incompatible"
        and audit.response_model == "different-model"
    )
    assert audit.usage_evidence == "complete" and report.usage is not None
    assert report.usage.input_tokens == 10
    report.validate(_binding())
    with pytest.raises(ProviderAuditError):
        report.validate(_binding(expected_response_model="different-model"))
    assert report.usage.receipt_hash == audit.receipt_hash(CALL_ID)
    assert "price" not in repr(report) and not hasattr(report, "price_eligible")


@pytest.mark.parametrize(
    "field", ["id", "model", "system_fingerprint", "created", "object"]
)
@pytest.mark.parametrize(
    "bad", [None, True, [], "", "unsafe identifier", "é", "x" * 129]
)
def test_invalid_identity_retains_other_individually_safe_fields(
    field: str, bad: Any
) -> None:
    """Invalid raw identity values become null; valid siblings remain evidence."""
    value = _envelope()
    value[field] = bad
    report = _capture(value)
    audit = _audit(report)
    assert audit.identity_state == "invalid" and audit.usage_evidence == "unavailable"
    assert report.usage is None
    mapping = {
        "id": "response_id",
        "model": "response_model",
        "system_fingerprint": "system_fingerprint",
        "created": "created",
    }
    for raw_name, audit_name in mapping.items():
        assert getattr(audit, audit_name) == (
            None if raw_name == field else value[raw_name]
        )
    assert audit.reasoning_state == "unavailable"


@pytest.mark.parametrize(
    "raw",
    [
        b"{",
        b"[]",
        b"null",
        b"{}{}",
        b'{"id":"a","id":"b"}',
        b"\xff",
        b'{"id":"\\ud800"}',
        b'{"x":NaN}',
    ],
)
def test_ambiguous_or_nonobject_body_keeps_digest_without_identity(raw: bytes) -> None:
    """Strict decoding failure does not expose values from an ambiguous body."""
    audit = _audit(_capture_bytes(raw))
    assert audit.response_sha256 == hashlib.sha256(raw).hexdigest()
    assert audit.identity_state == "invalid"
    assert (
        audit.response_id
        is audit.response_model
        is audit.system_fingerprint
        is audit.created
        is None
    )
    assert (
        audit.usage_evidence
        == audit.reasoning_state
        == audit.mode_state
        == "unavailable"
    )


@pytest.mark.parametrize(
    "field", ["id", "model", "system_fingerprint", "created", "object"]
)
def test_missing_identity_is_unavailable_usage(field: str) -> None:
    """An absent identity cannot support the required chat receipt."""
    value = _envelope()
    del value[field]
    assert _audit(_capture(value)).usage_evidence == "unavailable"


@pytest.mark.parametrize(
    "details, state, count, evidence, mode",
    [
        ({}, "absent", None, "complete", "nonthinking"),
        ({"reasoning_tokens": 0}, "observed", 0, "complete", "nonthinking"),
        ({"reasoning_tokens": 3}, "observed", 3, "complete", "unexpected"),
        ({"reasoning_tokens": 5}, "observed", 5, "complete", "unexpected"),
        ({"reasoning_tokens": None}, "invalid", None, "invalid", "invalid"),
        ({"reasoning_tokens": -1}, "invalid", None, "invalid", "invalid"),
        ({"reasoning_tokens": True}, "invalid", None, "invalid", "invalid"),
        ({"reasoning_tokens": "0"}, "invalid", None, "invalid", "invalid"),
        ({"reasoning_tokens": 0.0}, "invalid", None, "invalid", "invalid"),
        ({"reasoning_tokens": 6}, "invalid", None, "invalid", "invalid"),
        ({"reasoning_tokens": MAX_INTEGER + 1}, "invalid", None, "invalid", "invalid"),
        ({"other": 0}, "invalid", None, "invalid", "invalid"),
        (None, "invalid", None, "invalid", "invalid"),
        ([], "invalid", None, "invalid", "invalid"),
    ],
)
def test_reasoning_details_have_explicit_absent_zero_positive_and_invalid_states(
    details: Any, state: str, count: int | None, evidence: str, mode: str
) -> None:
    """Reasoning is never fabricated as zero and is not added to output tokens."""
    value = _envelope()
    value["usage"]["completion_tokens_details"] = details
    report = _capture(value)
    audit = _audit(report)
    assert (
        audit.reasoning_state,
        audit.reasoning_tokens,
        audit.usage_evidence,
        audit.mode_state,
    ) == (state, count, evidence, mode)
    if report.usage is not None:
        assert report.usage.output_tokens == 5


@pytest.mark.parametrize(
    "reasoning, expected",
    [
        (0, "unavailable"),
        (3, "unavailable"),
        (-1, "invalid"),
        (6, "invalid"),
        (None, "invalid"),
    ],
)
def test_incomplete_aggregate_never_claims_reasoning_observed(
    reasoning: Any, expected: str
) -> None:
    """An isolated safe integer cannot establish verified output bounds."""
    value = _envelope()
    del value["usage"]["total_tokens"]
    value["usage"]["completion_tokens_details"] = {"reasoning_tokens": reasoning}
    audit = _audit(_capture(value))
    assert audit.usage_evidence == "invalid" and audit.reasoning_state == expected
    assert audit.reasoning_tokens is None


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
@pytest.mark.parametrize("bad", [-1, True, 1.0, "1", None, MAX_INTEGER + 1])
def test_usage_requires_exact_safe_counters(field: str, bad: Any) -> None:
    """Unsafe counters remain unknown, rather than clamped or zero-valued."""
    value = _envelope()
    value["usage"][field] = bad
    report = _capture(value)
    assert report.usage is None and _audit(report).usage_evidence == "invalid"


@pytest.mark.parametrize(
    "change",
    [
        {"prompt_cache_miss_tokens": 7},
        {"total_tokens": 16},
        {"prompt_tokens_details": {"cached_tokens": 3}},
        {"prompt_tokens_details": {"cached_tokens": True}},
        {"prompt_tokens_details": {"other": 2}},
        {"prompt_tokens_details": None},
        {"other": 0},
    ],
)
def test_aggregate_and_optional_cached_details_must_agree(
    change: dict[str, Any],
) -> None:
    """No partial or contradictory aggregate can generate canonical usage."""
    value = _envelope()
    value["usage"].update(change)
    assert _capture(value).usage is None


@pytest.mark.parametrize("usage", [None, [], {}, "missing"])
def test_present_invalid_usage_is_distinct_from_absent(usage: Any) -> None:
    """Only the missing usage key has absent evidence."""
    value = _envelope()
    value["usage"] = usage
    assert _audit(_capture(value)).usage_evidence == "invalid"
    del value["usage"]
    audit = _audit(_capture(value))
    assert (
        audit.usage_evidence == "absent"
        and audit.reasoning_state == audit.mode_state == "unavailable"
    )


@pytest.mark.parametrize(
    "field, value, expected",
    [
        ("reasoning_content", None, "nonthinking"),
        ("reasoning_content", "", "nonthinking"),
        ("reasoning_content", SENTINEL, "unexpected"),
        ("reasoning_content", 0, "invalid"),
        ("tool_calls", None, "nonthinking"),
        ("tool_calls", [], "nonthinking"),
        ("tool_calls", [{"function": SENTINEL}], "unexpected"),
        ("tool_calls", "", "invalid"),
    ],
)
def test_mode_facts_keep_usage_and_never_tool_or_reasoning_contents(
    field: str, value: Any, expected: str
) -> None:
    """Explicit mode violations preserve structurally complete metering."""
    envelope = _envelope()
    envelope["choices"][0]["message"][field] = value
    report = _capture(envelope)
    assert report.usage is not None and _audit(report).mode_state == expected
    assert SENTINEL not in repr(report) and SENTINEL.encode() not in encode_call_report(
        report
    )
    del envelope["usage"]
    missing = _audit(_capture(envelope))
    assert missing.mode_state == (
        "unavailable" if expected == "nonthinking" else expected
    )


@pytest.mark.parametrize(
    "choices",
    [
        None,
        [],
        [{}, {}],
        [{}],
        [{"index": True, "message": {}, "finish_reason": "stop"}],
    ],
)
def test_invalid_choice_cannot_destroy_independent_usage(choices: Any) -> None:
    """Envelope mode rejection is independent from complete provider counting."""
    value = _envelope()
    value["choices"] = choices
    report = _capture(value)
    assert report.usage is not None and _audit(report).mode_state == "invalid"


@pytest.mark.parametrize("finish", ["length", "other", None])
@pytest.mark.parametrize(
    "content", ["not JSON", "[]", '{"wrong_schema":true}', None, "x" * 16385]
)
def test_business_output_failures_do_not_invalidate_identity_usage_or_mode(
    finish: Any, content: Any
) -> None:
    """Neither business validation nor finish reasons control metering facts."""
    value = _envelope()
    value["choices"][0].update(finish_reason=finish)
    value["choices"][0]["message"]["content"] = content
    report = _capture(value)
    audit = _audit(report)
    assert report.usage is not None
    assert (audit.identity_state, audit.usage_evidence, audit.mode_state) == (
        "compatible",
        "complete",
        "nonthinking",
    )


def test_exact_body_and_safeint_boundaries_preserve_unclamped_counters() -> None:
    """The upper response/count bounds are inclusive and never reservation caps."""
    value = _envelope()
    value.update(
        id="r" * 128, model="m" * 128, system_fingerprint="f" * 128, created=MAX_INTEGER
    )
    value["usage"].update(
        completion_tokens=MAX_INTEGER - 10,
        total_tokens=MAX_INTEGER,
        completion_tokens_details={"reasoning_tokens": MAX_INTEGER - 10},
    )
    raw = _wire(value)
    boundary = raw + b" " * (MAX_RESPONSE_BYTES - len(raw))
    report = _capture_bytes(boundary)
    audit = _audit(report)
    assert report.usage is not None and report.usage.output_tokens == MAX_INTEGER - 10
    assert audit.created == MAX_INTEGER and len(audit.response_model or "") == 128
    assert audit.reasoning_tokens == MAX_INTEGER - 10
    assert len(encode_provider_audit(audit)) <= MAX_AUDIT_BYTES
    assert not _audit(_capture_bytes(boundary + b" ")).response_complete
    value["usage"]["total_tokens"] = MAX_INTEGER + 1
    assert _capture(value).usage is None


def test_zero_is_valid_for_created_and_all_usage_counts() -> None:
    """Explicit zero is distinct from nullable or unavailable fields."""
    value = _envelope()
    value["created"] = 0
    value["usage"] = {
        key: 0
        for key in (
            "prompt_tokens",
            "completion_tokens",
            "total_tokens",
            "prompt_cache_hit_tokens",
            "prompt_cache_miss_tokens",
        )
    }
    value["usage"]["completion_tokens_details"] = {"reasoning_tokens": 0}
    report = _capture(value)
    audit = _audit(report)
    assert (
        report.usage is not None
        and report.usage.input_tokens == report.usage.output_tokens == 0
    )
    assert (
        audit.created == audit.reasoning_tokens == 0
        and audit.reasoning_state == "observed"
    )


def test_captured_facts_are_immutable_and_detached() -> None:
    """Mutable business JSON and detached dictionaries cannot rewrite facts."""
    value = _envelope()
    report = _capture(value)
    saved = encode_call_report(report)
    value["model"] = "changed"
    report.to_dict()["provider_audit"]["response_model"] = "changed"
    assert encode_call_report(report) == saved
    with pytest.raises(FrozenInstanceError):
        # Deliberately exercise the frozen dataclass assignment guard.
        report.usage = None  # type: ignore[misc]


@pytest.mark.parametrize(
    "field",
    [
        "schema_version",
        "provider",
        "response_complete",
        "http_status",
        "identity_state",
        "usage_evidence",
        "reasoning_state",
        "mode_state",
        "audit_hash",
    ],
)
def test_audit_scalar_null_missing_unknown_and_duplicate_keys_are_rejected(
    field: str,
) -> None:
    """Nullable fields alone permit null; every declared key remains required."""
    value = _audit(_capture(_envelope())).to_dict()
    for changed in (
        {**value, field: None},
        {key: child for key, child in value.items() if key != field},
        {**value, "unknown": 0},
    ):
        with pytest.raises(ProviderAuditError):
            decode_provider_audit(_wire(changed))
    raw = _wire(value)
    with pytest.raises(ProviderAuditError):
        decode_provider_audit(
            b"{" + _wire(field) + b":" + _wire(value[field]) + b"," + raw[1:]
        )


@pytest.mark.parametrize(
    "field, bad",
    [
        ("schema_version", 1.0),
        ("schema_version", "1"),
        ("response_complete", 1),
        ("http_status", 200.0),
        ("created", True),
        ("created", "1"),
        ("created", MAX_INTEGER + 1),
        ("response_id", ""),
        ("response_model", "é"),
        ("system_fingerprint", "x" * 129),
        ("response_sha256", "A" * 64),
        ("reasoning_tokens", 0.0),
        ("audit_hash", ""),
        ("identity_state", []),
    ],
)
def test_audit_wire_types_and_boundaries_are_exact(field: str, bad: Any) -> None:
    """JSON strings, bools, floats and unsafe identifiers cannot alias facts."""
    value = _audit(_capture(_envelope())).to_dict()
    value[field] = bad
    with pytest.raises(ProviderAuditError):
        decode_provider_audit(_wire(value))


@pytest.mark.parametrize("suffix", [b"x", b"{}", b"\xff"])
def test_strict_decoder_errors_never_keep_sensitive_body_in_exception(
    suffix: bytes,
) -> None:
    """Fixed exceptions drop parser doc/context as well as public error text."""
    with pytest.raises(ProviderAuditError) as caught:
        decode_provider_audit(b'{"private":"' + SENTINEL.encode() + b'"}' + suffix)
    assert SENTINEL not in repr(caught.value)
    assert caught.value.__context__ is None and caught.value.__cause__ is None


def test_raw_audit_2048_bound_is_independent_inside_call_report() -> None:
    """Excess whitespace must not disappear before the raw audit cap is checked."""
    report = _capture(_envelope())
    compact = encode_provider_audit(_audit(report))
    padded = compact[:-1] + b" " * (MAX_AUDIT_BYTES - len(compact)) + b"}"
    assert (
        len(padded) == 2048 and decode_provider_audit(padded) == report.provider_audit
    )
    with pytest.raises(ProviderAuditError):
        decode_provider_audit(padded + b" ")
    prefix = (
        b'{"usage":'
        + _wire(report.usage.to_dict() if report.usage else None)
        + b',"provider_audit":'
    )
    assert decode_call_report(prefix + padded + b"}") == report
    with pytest.raises(ProviderAuditError):
        decode_call_report(prefix + padded[:-1] + b" }" + b"}")


@pytest.mark.parametrize(
    "changes",
    [
        {"reasoning_state": "observed", "reasoning_tokens": None},
        {"reasoning_state": "absent", "reasoning_tokens": 0},
        {"reasoning_state": "unavailable"},
        {"reasoning_state": "observed", "reasoning_tokens": 1},
        {
            "mode_state": "nonthinking",
            "usage_evidence": "absent",
            "reasoning_state": "unavailable",
        },
        {"identity_state": "invalid", "usage_evidence": "complete"},
        {"response_complete": False},
        {"http_status": 500},
    ],
)
def test_self_consistent_hash_does_not_override_audit_cross_field_rules(
    changes: dict[str, Any],
) -> None:
    """Hash equality alone never validates contradictory typed audit facts."""
    audit = _signed(_audit(_capture(_envelope())), **changes)
    with pytest.raises(ProviderAuditError):
        audit.validate()


def test_report_cross_checks_reasoning_receipt_model_and_binding() -> None:
    """Report validation rejects a coherent hash attached to the wrong facts."""
    report = _capture(_envelope())
    assert report.usage is not None
    audit = _audit(report)
    with pytest.raises(ProviderAuditError):
        CallReport(
            report.usage,
            _signed(
                audit,
                reasoning_state="observed",
                reasoning_tokens=6,
                mode_state="unexpected",
            ),
        ).validate()
    with pytest.raises(ProviderAuditError):
        CallReport(None, audit).validate()
    for binding in (
        _binding(physical_call_id=CALL_ID[:-1] + "7"),
        _binding(expected_response_model="other"),
        _binding(subcall="query_embedding"),
    ):
        with pytest.raises(ProviderAuditError):
            report.validate(binding)
    for binding in (
        _binding(execution_binding_hash="c" * 64),
        _binding(parameter_hash="c" * 64),
    ):
        with pytest.raises(ProviderAuditError):
            report.verify(binding, report.hash(_binding()))
    altered = replace(report.usage, receipt_hash="0" * 64)
    altered = replace(altered, usage_hash=altered.hash())
    with pytest.raises(ProviderAuditError):
        CallReport(altered, audit).validate(_binding())


def test_usage_only_embedding_retains_old_canonical_validation() -> None:
    """Canonical counters remain observable for later reservation-anomaly checks."""
    usage = UsageReport(10, 5, 2, "c" * 64, "")
    usage = replace(usage, usage_hash=usage.hash())
    report = CallReport(usage, None)
    report.validate(_binding(subcall="query_embedding", expected_response_model=""))
    assert decode_call_report(encode_call_report(report)) == report
    with pytest.raises(ProviderAuditError):
        report.validate(_binding())
    with pytest.raises(ProviderAuditError):
        CallReport(None, None).validate()


def _execution_binding() -> dict[str, Any]:
    return {
        "tenant_id": "tenant-a",
        "run_id": CALL_ID,
        "worker_id": "worker-a",
        "session_id": CALL_ID[:-1] + "1",
        "attempt_no": 1,
        "fencing_token": 2,
        "step_id": CALL_ID[:-1] + "2",
        "step_sequence": 5,
        "step_kind": "model_proposal",
        "cursor_version": 4,
        "input_hash": "c" * 64,
        "profile_id": "profile-a",
        "profile_hash": "d" * 64,
        "snapshot_id": CALL_ID[:-1] + "3",
        "snapshot_hash": "e" * 64,
    }


def test_execution_hash_binds_every_original_field_and_rejects_bad_shape() -> None:
    """Changing any original execution field changes the persistent binding."""
    binding = _execution_binding()
    expected = _digest(
        "jobforge.run.call-binding.v1", *(str(value) for value in binding.values())
    )
    assert execution_binding_hash(binding) == expected
    for field, value in {
        "attempt_no": 2,
        "fencing_token": 3,
        "tenant_id": "tenant-b",
        "worker_id": "worker-b",
        "profile_id": "profile-b",
        "step_kind": "protocol_correction",
        "input_hash": "f" * 64,
        "profile_hash": "f" * 64,
        "snapshot_hash": "f" * 64,
        "run_id": CALL_ID[:-1] + "7",
        "session_id": CALL_ID[:-1] + "7",
        "step_id": CALL_ID[:-1] + "7",
        "snapshot_id": CALL_ID[:-1] + "7",
    }.items():
        assert execution_binding_hash({**binding, field: value}) != expected
    assert (
        execution_binding_hash({**binding, "step_sequence": 6, "cursor_version": 5})
        != expected
    )
    for changes in (
        {"attempt_no": True},
        {"cursor_version": 32},
        {"step_sequence": 4},
        {"snapshot_id": "other"},
        {"extra": 0},
    ):
        with pytest.raises(ProviderAuditError):
            execution_binding_hash({**binding, **changes})


def test_observation_hash_uses_domain_error_and_nullable_audit() -> None:
    """This new pure function does not reinterpret the existing runtime codec."""
    fields: dict[str, Any] = dict(
        transport_outcome="response",
        http_status=200,
        mapped_domain_error_code="MODEL_PROTOCOL_ERROR",
        business_outcome="rejected",
        usage_hash="a" * 64,
        audit_hash="b" * 64,
    )
    assert observation_hash_v2(**fields) == _digest(
        "jobforge.run.observation.v2",
        "response",
        "200",
        "MODEL_PROTOCOL_ERROR",
        "rejected",
        "a" * 64,
        "b" * 64,
    )
    for changes in (
        {"mapped_domain_error_code": "OUTPUT_INVALID"},
        {"http_status": True},
        {"business_outcome": "accepted"},
        {"usage_hash": ""},
        {"audit_hash": ""},
    ):
        with pytest.raises(ProviderAuditError):
            observation_hash_v2(**{**fields, **changes})
    assert observation_hash_v2(
        transport_outcome="unknown",
        http_status=0,
        mapped_domain_error_code="TIMEOUT",
        business_outcome="unknown",
        usage_hash=None,
        audit_hash=None,
    ) == _digest(
        "jobforge.run.observation.v2", "unknown", "0", "TIMEOUT", "unknown", "", ""
    )


def test_duplicate_nested_fields_fail_before_any_identity_is_trusted() -> None:
    """Ambiguous business or usage JSON cannot supply even otherwise safe IDs."""
    raw = _wire(_envelope()).replace(
        b'"prompt_tokens":10', b'"prompt_tokens":10,"prompt_tokens":10'
    )
    audit = _audit(_capture_bytes(raw))
    assert audit.identity_state == "invalid" and audit.response_id is None
    report = _capture(_envelope())
    value = report.to_dict()
    for invalid in (
        {"usage": None, "provider_audit": None},
        {**value, "extra": 0},
        {"usage": value["usage"]},
        {**value, "usage": {**value["usage"], "extra": 0}},
    ):
        with pytest.raises(ProviderAuditError):
            decode_call_report(_wire(invalid))
    clone = copy.deepcopy(value)
    clone["usage"]["input_tokens"] = True
    with pytest.raises(ProviderAuditError):
        decode_call_report(_wire(clone))
