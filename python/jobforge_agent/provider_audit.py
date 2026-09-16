"""Capture and verify bounded provider facts without transport or authority.

These pure contracts are intentionally separate from the current runtime wire
bridge. A coherent report proves metadata binding, never permission or pricing.
"""

from __future__ import annotations

import hashlib
import json
import re
from dataclasses import asdict, dataclass, replace
from typing import Any

from jobforge_agent.errors import ToolError
from jobforge_agent.http import strict_json

MAX_INTEGER = (1 << 53) - 1
MAX_AUDIT_BYTES = 2048
MAX_RESPONSE_BYTES = 65536
MAX_REPORT_BYTES = 8192
_IDENTIFIER = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:-]{0,127}")
_HASH = re.compile(r"[0-9a-f]{64}")
_UUID = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")
_IDENTITY_FIELDS = {"id", "model", "system_fingerprint", "created", "object"}
_USAGE_FIELDS = {
    "prompt_tokens",
    "completion_tokens",
    "total_tokens",
    "prompt_cache_hit_tokens",
    "prompt_cache_miss_tokens",
}
_STEPS = {
    "read_ticket",
    "get_order",
    "get_delivery",
    "search_policy",
    "model_proposal",
    "protocol_correction",
    "submit_proposal",
}
_OBSERVATION_ERRORS = {
    "",
    "MODEL_PROTOCOL_ERROR",
    "INVALID_ARGUMENT",
    "EXECUTOR_PROTOCOL_ERROR",
    "TIMEOUT",
    "DEPENDENCY_UNAVAILABLE",
    "PROFILE_UNAVAILABLE",
    "BUDGET_EXHAUSTED",
}


class ProviderAuditError(ValueError):
    """Expose only a fixed diagnostic, never supplied response or JSON text."""

    def __init__(self) -> None:
        """Use one content-free error for this pure contract boundary."""
        super().__init__("invalid provider audit report")


def _require(valid: bool) -> None:
    if not valid:
        raise ProviderAuditError()


def _integer(value: Any, low: int = 0, high: int = MAX_INTEGER) -> bool:
    return type(value) is int and low <= value <= high


def _match(value: Any, pattern: re.Pattern[str] = _IDENTIFIER) -> bool:
    return isinstance(value, str) and pattern.fullmatch(value) is not None


def _member(value: Any, choices: set[str]) -> bool:
    return isinstance(value, str) and value in choices


def _fingerprint(domain: str, *fields: str) -> str:
    digest = hashlib.sha256()
    for value in (domain, *fields):
        raw = value.encode("utf-8")
        digest.update(len(raw).to_bytes(8, "big"))
        digest.update(raw)
    return digest.hexdigest()


def _nullable(value: str | int | None) -> str:
    return "" if value is None else str(value)


def _json_bytes(value: Any) -> bytes:
    return json.dumps(
        value, ensure_ascii=False, allow_nan=False, separators=(",", ":")
    ).encode("utf-8")


def _decode(raw: bytes, limit: int) -> Any:
    _require(type(raw) is bytes and len(raw) <= limit)
    try:
        return strict_json(raw)
    except ToolError:
        pass
    # Do not retain JSONDecodeError.doc or provider text in exception context.
    raise ProviderAuditError()


def _object(value: Any, fields: set[str]) -> dict[str, Any]:
    _require(isinstance(value, dict) and value.keys() == fields)
    return value


@dataclass(frozen=True)
class ProviderAudit:
    """Retain only the ADR-0020 v1 closed set of provider metadata."""

    schema_version: int
    provider: str
    response_complete: bool
    http_status: int
    response_sha256: str | None
    identity_state: str
    response_id: str | None
    response_model: str | None
    system_fingerprint: str | None
    created: int | None
    usage_evidence: str
    reasoning_state: str
    reasoning_tokens: int | None
    mode_state: str
    audit_hash: str

    def hash(self) -> str:
        """Compute the exact length-prefixed hash, excluding its own field."""
        return _fingerprint(
            "jobforge.run.provider-audit.v1",
            str(self.schema_version),
            self.provider,
            "1" if self.response_complete else "0",
            str(self.http_status),
            _nullable(self.response_sha256),
            self.identity_state,
            _nullable(self.response_id),
            _nullable(self.response_model),
            _nullable(self.system_fingerprint),
            _nullable(self.created),
            self.usage_evidence,
            self.reasoning_state,
            _nullable(self.reasoning_tokens),
            self.mode_state,
        )

    def to_dict(self) -> dict[str, Any]:
        """Return a detached closed JSON object with explicit nullable keys."""
        return asdict(self)

    def validate(self) -> None:
        """Check intrinsic facts and hash without guessing the original profile."""
        _require(_integer(self.schema_version, 1, 1) and self.provider == "deepseek")
        _require(type(self.response_complete) is bool)
        _require(_integer(self.http_status, 0, 599))
        _require(self.response_sha256 is None or _match(self.response_sha256, _HASH))
        for value in (self.response_id, self.response_model, self.system_fingerprint):
            _require(value is None or _match(value))
        _require(self.created is None or _integer(self.created))
        _require(self.reasoning_tokens is None or _integer(self.reasoning_tokens))
        _require(
            _member(
                self.identity_state,
                {"compatible", "incompatible", "invalid", "unavailable"},
            )
        )
        _require(
            _member(
                self.usage_evidence, {"complete", "absent", "invalid", "unavailable"}
            )
        )
        _require(
            _member(
                self.reasoning_state, {"observed", "absent", "invalid", "unavailable"}
            )
        )
        _require(
            _member(
                self.mode_state, {"nonthinking", "unexpected", "invalid", "unavailable"}
            )
        )
        identity = (
            self.response_id,
            self.response_model,
            self.system_fingerprint,
            self.created,
        )
        if self.response_complete:
            _require(self.http_status >= 100 and self.response_sha256 is not None)
        else:
            _require(self.http_status == 0 and self.response_sha256 is None)
        if not self.response_complete or self.http_status != 200:
            _require(all(value is None for value in identity))
            _require(
                self.identity_state
                == self.usage_evidence
                == self.reasoning_state
                == self.mode_state
                == "unavailable"
            )
        else:
            _require(self.identity_state != "unavailable")
            if self.identity_state in {"compatible", "incompatible"}:
                _require(all(value is not None for value in identity))
        if self.usage_evidence == "complete":
            _require(self.identity_state in {"compatible", "incompatible"})
            _require(self.reasoning_state in {"observed", "absent"})
            _require(self.mode_state != "unavailable")
        else:
            _require(self.reasoning_state in {"invalid", "unavailable"})
        _require(
            (self.reasoning_state == "observed") == (self.reasoning_tokens is not None)
        )
        if self.reasoning_state == "invalid":
            _require(self.mode_state == "invalid")
        if self.mode_state == "nonthinking":
            _require(self.reasoning_state == "absent" or self.reasoning_tokens == 0)
        if self.reasoning_tokens is not None and self.reasoning_tokens > 0:
            _require(self.mode_state in {"unexpected", "invalid"})
        _require(_match(self.audit_hash, _HASH) and self.audit_hash == self.hash())
        _require(len(_json_bytes(self.to_dict())) <= MAX_AUDIT_BYTES)

    def receipt_hash(self, physical_call_id: str) -> str:
        """Bind a complete actual identity, including an incompatible model."""
        self.validate()
        _require(_match(physical_call_id, _UUID))
        _require(self.identity_state in {"compatible", "incompatible"})
        return _fingerprint(
            "jobforge.deepseek.receipt.v1",
            physical_call_id,
            _nullable(self.response_sha256),
            _nullable(self.response_id),
            _nullable(self.response_model),
            _nullable(self.system_fingerprint),
            _nullable(self.created),
        )


@dataclass(frozen=True)
class UsageReport:
    """Hold canonical counters and receipt, without reservation or pricing."""

    input_tokens: int
    output_tokens: int
    cached_input_tokens: int
    receipt_hash: str
    usage_hash: str

    def hash(self) -> str:
        """Keep the existing jobforge.run.usage.v1 fingerprint unchanged."""
        return _fingerprint(
            "jobforge.run.usage.v1",
            str(self.input_tokens),
            str(self.output_tokens),
            str(self.cached_input_tokens),
            self.receipt_hash,
        )

    def validate(self) -> None:
        """Reject lossy counters and inconsistent canonical hash values."""
        _require(_integer(self.input_tokens) and _integer(self.output_tokens))
        _require(_integer(self.cached_input_tokens, 0, self.input_tokens))
        _require(_match(self.receipt_hash, _HASH) and _match(self.usage_hash, _HASH))
        _require(self.usage_hash == self.hash())

    def to_dict(self) -> dict[str, Any]:
        """Return the unchanged five-field usage shape."""
        return asdict(self)


@dataclass(frozen=True)
class ReportBinding:
    """Carry original reservation identity and the frozen expected model."""

    execution_binding_hash: str
    physical_call_id: str
    parameter_hash: str
    subcall: str
    expected_response_model: str

    def validate(self) -> None:
        """Validate shape only; callers must obtain actual reservation values."""
        _require(_match(self.execution_binding_hash, _HASH))
        _require(_match(self.physical_call_id, _UUID))
        _require(_match(self.parameter_hash, _HASH))
        _require(_member(self.subcall, {"chat", "query_embedding"}))
        if self.subcall == "chat":
            _require(_match(self.expected_response_model))


@dataclass(frozen=True)
class CallReport:
    """Freeze one metering/audit fact; this object grants no continuation."""

    usage: UsageReport | None
    provider_audit: ProviderAudit | None

    def to_dict(self) -> dict[str, Any]:
        """Return both required keys even for an audit-only report."""
        return asdict(self)

    def validate(self, binding: ReportBinding | None = None) -> None:
        """Validate structural coherence and optionally original call binding."""
        _require(self.usage is not None or self.provider_audit is not None)
        if self.usage is not None:
            _require(isinstance(self.usage, UsageReport))
            self.usage.validate()
        audit = self.provider_audit
        if audit is not None:
            _require(isinstance(audit, ProviderAudit))
            audit.validate()
            _require((audit.usage_evidence == "complete") == (self.usage is not None))
            if self.usage is not None:
                _require(
                    self.usage.input_tokens + self.usage.output_tokens <= MAX_INTEGER
                )
                if audit.reasoning_tokens is not None:
                    _require(audit.reasoning_tokens <= self.usage.output_tokens)
        if binding is None:
            return
        binding.validate()
        if binding.subcall == "chat":
            _require(audit is not None)
            assert audit is not None
            if audit.identity_state in {"compatible", "incompatible"}:
                _require(
                    (audit.response_model == binding.expected_response_model)
                    == (audit.identity_state == "compatible")
                )
            if self.usage is not None:
                _require(
                    self.usage.receipt_hash
                    == audit.receipt_hash(binding.physical_call_id)
                )
        else:
            _require(audit is None and self.usage is not None)

    def hash(self, binding: ReportBinding) -> str:
        """Fingerprint the frozen report and original execution/call/parameters."""
        self.validate(binding)
        return _fingerprint(
            "jobforge.run.call-report.v1",
            binding.execution_binding_hash,
            binding.physical_call_id,
            binding.parameter_hash,
            self.usage.usage_hash if self.usage is not None else "",
            self.provider_audit.audit_hash if self.provider_audit is not None else "",
        )

    def verify(self, binding: ReportBinding, report_hash: str) -> None:
        """Require an exact submitted hash after full report binding checks."""
        _require(_match(report_hash, _HASH) and report_hash == self.hash(binding))


def decode_provider_audit(raw: bytes) -> ProviderAudit:
    """Decode the complete closed object and enforce its raw 2048-byte bound."""
    value = _object(
        _decode(raw, MAX_AUDIT_BYTES), set(ProviderAudit.__dataclass_fields__)
    )
    audit = ProviderAudit(**value)
    audit.validate()
    return audit


def encode_provider_audit(audit: ProviderAudit) -> bytes:
    """Encode explicit nulls only after full metadata validation."""
    audit.validate()
    return _json_bytes(audit.to_dict())


def _member_raw(raw: bytes, member: str) -> bytes:
    # The enclosing object was strictly decoded first. Locate exact member bytes
    # so whitespace cannot evade the independent raw audit size limit.
    source = raw.decode("utf-8")
    decoder = json.JSONDecoder()
    index = source.index("{") + 1
    while True:
        while source[index] in " \t\r\n,":
            index += 1
        key, index = decoder.raw_decode(source, index)
        while source[index] in " \t\r\n:":
            index += 1
        start = index
        _, index = decoder.raw_decode(source, index)
        if key == member:
            return source[start:index].encode("utf-8")


def decode_call_report(raw: bytes) -> CallReport:
    """Strictly decode both report parts without accepting a protocol frame."""
    value = _object(_decode(raw, MAX_REPORT_BYTES), {"usage", "provider_audit"})
    usage = None
    if value["usage"] is not None:
        usage = UsageReport(
            **_object(value["usage"], set(UsageReport.__dataclass_fields__))
        )
    audit = None
    if value["provider_audit"] is not None:
        audit = decode_provider_audit(_member_raw(raw, "provider_audit"))
    result = CallReport(usage, audit)
    result.validate()
    return result


def encode_call_report(report: CallReport) -> bytes:
    """Encode a coherent report while keeping the separate audit cap."""
    report.validate()
    raw = _json_bytes(report.to_dict())
    _require(len(raw) <= MAX_REPORT_BYTES)
    return raw


def execution_binding_hash(binding: dict[str, Any]) -> str:
    """Hash validated original Lease/Step fields, never current cursor guesses."""
    fields = (
        "tenant_id",
        "run_id",
        "worker_id",
        "session_id",
        "attempt_no",
        "fencing_token",
        "step_id",
        "step_sequence",
        "step_kind",
        "cursor_version",
        "input_hash",
        "profile_id",
        "profile_hash",
        "snapshot_id",
        "snapshot_hash",
    )
    _object(binding, set(fields))
    for name in ("tenant_id", "worker_id", "profile_id"):
        _require(_match(binding[name]))
    for name in ("run_id", "session_id", "step_id", "snapshot_id"):
        _require(_match(binding[name], _UUID))
    for name in ("input_hash", "profile_hash", "snapshot_hash"):
        _require(_match(binding[name], _HASH))
    for name in ("attempt_no", "fencing_token"):
        _require(_integer(binding[name], 1))
    _require(_integer(binding["step_sequence"], 1, 32))
    _require(_integer(binding["cursor_version"], 0, 31))
    _require(binding["step_sequence"] == binding["cursor_version"] + 1)
    _require(_member(binding["step_kind"], _STEPS))
    return _fingerprint(
        "jobforge.run.call-binding.v1", *(str(binding[key]) for key in fields)
    )


def observation_hash_v2(
    *,
    transport_outcome: str,
    http_status: int,
    mapped_domain_error_code: str,
    business_outcome: str,
    usage_hash: str | None,
    audit_hash: str | None,
) -> str:
    """Hash already-mapped observation fields without modifying the old codec."""
    _require(_member(mapped_domain_error_code, _OBSERVATION_ERRORS))
    _require(usage_hash is None or _match(usage_hash, _HASH))
    _require(audit_hash is None or _match(audit_hash, _HASH))
    if transport_outcome == "unknown":
        _require(
            _integer(http_status, 0, 0)
            and business_outcome == "unknown"
            and usage_hash is None
        )
    else:
        _require(transport_outcome == "response" and _integer(http_status, 100, 599))
        _require(_member(business_outcome, {"accepted", "rejected"}))
        _require(business_outcome != "accepted" or mapped_domain_error_code == "")
    return _fingerprint(
        "jobforge.run.observation.v2",
        transport_outcome,
        str(http_status),
        mapped_domain_error_code,
        business_outcome,
        _nullable(usage_hash),
        _nullable(audit_hash),
    )


def _reasoning_invalid(usage: Any) -> bool:
    if not isinstance(usage, dict) or "completion_tokens_details" not in usage:
        return False
    details = usage["completion_tokens_details"]
    if not isinstance(details, dict) or details.keys() - {"reasoning_tokens"}:
        return True
    if "reasoning_tokens" not in details:
        return False
    count = details["reasoning_tokens"]
    output = usage.get("completion_tokens")
    return not _integer(count) or (_integer(output) and count > output)


def _usage_facts(
    envelope: dict[str, Any], identity_valid: bool
) -> tuple[str, str, int | None]:
    usage = envelope.get("usage")
    invalid_reasoning = _reasoning_invalid(usage)
    missing_reasoning = "invalid" if invalid_reasoning else "unavailable"
    if not identity_valid:
        return "unavailable", missing_reasoning, None
    if "usage" not in envelope:
        return "absent", "unavailable", None
    if (
        not isinstance(usage, dict)
        or not _USAGE_FIELDS <= usage.keys()
        or usage.keys()
        - (_USAGE_FIELDS | {"prompt_tokens_details", "completion_tokens_details"})
        or any(not _integer(usage[field]) for field in _USAGE_FIELDS)
        or usage["prompt_tokens"]
        != usage["prompt_cache_hit_tokens"] + usage["prompt_cache_miss_tokens"]
        or usage["total_tokens"] != usage["prompt_tokens"] + usage["completion_tokens"]
        or invalid_reasoning
    ):
        return "invalid", missing_reasoning, None
    prompt = usage.get("prompt_tokens_details", {})
    if (
        not isinstance(prompt, dict)
        or prompt.keys() - {"cached_tokens"}
        or (
            "cached_tokens" in prompt
            and (
                not _integer(prompt["cached_tokens"])
                or prompt["cached_tokens"] != usage["prompt_cache_hit_tokens"]
            )
        )
    ):
        return "invalid", missing_reasoning, None
    details = usage.get("completion_tokens_details", {})
    if "reasoning_tokens" in details:
        return "complete", "observed", details["reasoning_tokens"]
    return "complete", "absent", None


def _mode(
    envelope: dict[str, Any], reasoning_state: str, reasoning_tokens: int | None
) -> str:
    if reasoning_state == "invalid":
        return "invalid"
    choices = envelope.get("choices")
    if not isinstance(choices, list) or len(choices) != 1:
        return "invalid"
    choice = choices[0]
    if (
        not isinstance(choice, dict)
        or not {"index", "message", "finish_reason"} <= choice.keys()
        or choice.keys() - {"index", "message", "finish_reason", "logprobs"}
        or not _integer(choice["index"], 0, 0)
        or choice.get("logprobs") is not None
    ):
        return "invalid"
    message = choice["message"]
    if (
        not isinstance(message, dict)
        or not {"role", "content"} <= message.keys()
        or message.keys() - {"role", "content", "reasoning_content", "tool_calls"}
        or message["role"] != "assistant"
        or (message["content"] is not None and not isinstance(message["content"], str))
    ):
        return "invalid"
    reasoning, tools = message.get("reasoning_content"), message.get("tool_calls")
    if (reasoning is not None and not isinstance(reasoning, str)) or (
        tools is not None and not isinstance(tools, list)
    ):
        return "invalid"
    if reasoning or tools or (reasoning_tokens is not None and reasoning_tokens > 0):
        return "unexpected"
    return "nonthinking" if reasoning_state in {"observed", "absent"} else "unavailable"


def capture_chat_report(
    body: bytes | None,
    *,
    http_status: int,
    physical_call_id: str,
    expected_response_model: str,
) -> CallReport:
    """Freeze facts from one already-read bounded body before asynchronous hooks.

    Args:
        body: Complete original bytes, or None for any incomplete read. Passing
            an over-limit body also records unavailable, never a partial digest.
        http_status: Received status; ignored when no bounded complete body exists.
        physical_call_id: The original canonical physical-call UUID.
        expected_response_model: Trusted frozen profile value, never body-derived.

    Returns:
        A content-free immutable report, including safe incompatible-model usage.
        No settlement, stop decision, continuation or provider request occurs.
    """
    _require(_match(physical_call_id, _UUID) and _match(expected_response_model))
    _require(body is None or type(body) is bytes)
    complete = body is not None and len(body) <= MAX_RESPONSE_BYTES
    if complete:
        _require(_integer(http_status, 100, 599))
    audit = ProviderAudit(
        1,
        "deepseek",
        complete,
        http_status if complete else 0,
        hashlib.sha256(body).hexdigest() if complete and body is not None else None,
        "unavailable",
        None,
        None,
        None,
        None,
        "unavailable",
        "unavailable",
        None,
        "unavailable",
        "",
    )
    envelope = None
    if complete and http_status == 200:
        assert body is not None
        try:
            envelope = strict_json(body)
        except ToolError:
            pass
        audit = replace(audit, identity_state="invalid")
    if isinstance(envelope, dict):
        response_id = envelope.get("id")
        model = envelope.get("model")
        fingerprint = envelope.get("system_fingerprint")
        created = envelope.get("created")
        identity_valid = (
            _IDENTITY_FIELDS <= envelope.keys()
            and not envelope.keys() - (_IDENTITY_FIELDS | {"choices", "usage"})
            and envelope["object"] == "chat.completion"
            and all(_match(value) for value in (response_id, model, fingerprint))
            and _integer(created)
        )
        evidence, reasoning_state, reasoning_tokens = _usage_facts(
            envelope, identity_valid
        )
        audit = replace(
            audit,
            identity_state=(
                "compatible" if model == expected_response_model else "incompatible"
            )
            if identity_valid
            else "invalid",
            response_id=response_id if _match(response_id) else None,
            response_model=model if _match(model) else None,
            system_fingerprint=fingerprint if _match(fingerprint) else None,
            created=created if _integer(created) else None,
            usage_evidence=evidence,
            reasoning_state=reasoning_state,
            reasoning_tokens=reasoning_tokens,
            mode_state=_mode(envelope, reasoning_state, reasoning_tokens),
        )
    audit = replace(audit, audit_hash=audit.hash())
    usage = None
    if audit.usage_evidence == "complete":
        assert isinstance(envelope, dict)
        counts = envelope["usage"]
        usage = UsageReport(
            counts["prompt_tokens"],
            counts["completion_tokens"],
            counts["prompt_cache_hit_tokens"],
            audit.receipt_hash(physical_call_id),
            "",
        )
        usage = replace(usage, usage_hash=usage.hash())
    report = CallReport(usage, audit)
    report.validate()
    return report
