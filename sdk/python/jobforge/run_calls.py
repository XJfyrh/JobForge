"""Bounded, read-only call evidence; observed tokens are not a provider bill."""

from __future__ import annotations

import re
from dataclasses import dataclass
from datetime import datetime

from jobforge.run_models import RunModel, _hash, _uuid

MAX_CALL_RESPONSE_BYTES = 256 * 1024
_IDENTIFIER = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:-]{0,127}\Z")
_BATCH_STOPS = frozenset(
    {
        "MEASUREMENT_ANOMALY",
        "PROVIDER_HTTP_REJECTED",
        "PROVIDER_IDENTITY_INVALID",
        "PROVIDER_MODE_INVALID",
        "CHAT_USAGE_UNKNOWN",
        "REPORT_CONFLICT",
    }
)


def _identifier(value: str) -> None:
    if _IDENTIFIER.fullmatch(value) is None:
        raise ValueError("invalid call evidence identifier")


@dataclass(frozen=True)
class ProviderAudit(RunModel):
    """Fixed adapter metadata, with absent, zero and unavailable kept distinct."""

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

    def __post_init__(self) -> None:
        if self.schema_version != 1 or self.provider != "deepseek":
            raise ValueError("invalid audit schema")
        for value in (self.response_id, self.response_model, self.system_fingerprint):
            if value is not None:
                _identifier(value)
        for digest in (self.audit_hash, self.response_sha256):
            if digest is not None:
                _hash(digest)
        if (
            self.identity_state
            not in ("compatible", "incompatible", "invalid", "unavailable")
            or self.usage_evidence
            not in ("complete", "absent", "invalid", "unavailable")
            or self.reasoning_state
            not in ("observed", "absent", "invalid", "unavailable")
            or self.mode_state
            not in ("nonthinking", "unexpected", "invalid", "unavailable")
        ):
            raise ValueError("invalid audit state")
        if (self.reasoning_state == "observed") != (self.reasoning_tokens is not None):
            raise ValueError("invalid reasoning presence")
        if self.response_complete:
            if not 100 <= self.http_status <= 599 or self.response_sha256 is None:
                raise ValueError("invalid complete response metadata")
        elif (
            self.http_status != 0
            or self.response_sha256 is not None
            or any(
                value is not None
                for value in (
                    self.response_id,
                    self.response_model,
                    self.system_fingerprint,
                    self.created,
                    self.reasoning_tokens,
                )
            )
            or (
                self.identity_state,
                self.usage_evidence,
                self.reasoning_state,
                self.mode_state,
            )
            != ("unavailable",) * 4
        ):
            raise ValueError("invalid unavailable response metadata")


@dataclass(frozen=True)
class CallUsage(RunModel):
    """Complete observed counters; only settled_usage has known pricing."""

    input_tokens: int
    output_tokens: int
    cached_input_tokens: int
    receipt_hash: str
    usage_hash: str

    def __post_init__(self) -> None:
        _hash(self.receipt_hash)
        _hash(self.usage_hash)
        if self.cached_input_tokens > self.input_tokens:
            raise ValueError("invalid cached input count")


@dataclass(frozen=True)
class CallBudget(RunModel):
    """Original conservative reservation, not a payment or final invoice."""

    input_tokens: int
    output_tokens: int
    total_tokens: int
    cost_microyuan: int

    def __post_init__(self) -> None:
        if self.total_tokens != self.input_tokens + self.output_tokens:
            raise ValueError("invalid call reservation")


@dataclass(frozen=True)
class RunCall(RunModel):
    """One physical request's facts with no execution credential or raw body."""

    physical_call_id: str
    step_id: str
    step_kind: str
    attempt_no: int
    ordinal: int
    subcall: str
    parameter_hash: str
    profile_id: str
    profile_hash: str
    price_hash: str
    reserved_at: datetime
    dispatch_expires_at: datetime
    call_deadline: datetime
    observed_at: datetime | None
    report_recorded_at: datetime | None
    settled_at: datetime | None
    reserved: CallBudget
    known_tokens: int
    known_cost_microyuan: int
    held_tokens: int
    held_cost_microyuan: int
    usage_known: bool
    measurement_anomaly: bool
    report_hash: str | None
    audit_hash: str | None
    provider_audit: ProviderAudit | None
    observed_usage: CallUsage | None
    settled_usage: CallUsage | None
    report_conflict: bool
    audit_status: str
    transport_outcome: str | None
    http_status: int | None
    error_code: str | None
    business_outcome: str | None

    def __post_init__(self) -> None:
        _uuid(self.physical_call_id)
        _uuid(self.step_id)
        _identifier(self.profile_id)
        if self.attempt_no < 1 or not 1 <= self.ordinal <= 44:
            raise ValueError("invalid physical call position")
        if self.step_kind not in (
            "get_order",
            "get_delivery",
            "search_policy",
            "model_proposal",
            "model_decision",
            "protocol_correction",
        ) or self.subcall not in (
            "get_order",
            "get_delivery",
            "profile_version",
            "profile_tags",
            "query_embedding",
            "search_policy",
            "chat",
        ):
            raise ValueError("invalid registered physical call")
        for value in (
            self.parameter_hash,
            self.profile_hash,
            self.price_hash,
            self.report_hash,
            self.audit_hash,
        ):
            if value is not None:
                _hash(value)
        if self.audit_status not in (
            "legacy_not_collected",
            "not_applicable",
            "missing",
            "recorded",
        ):
            raise ValueError("invalid audit presence status")
        if self.usage_known != (self.settled_usage is not None):
            raise ValueError("invalid known usage presence")
        if not self.usage_known and (self.known_tokens or self.known_cost_microyuan):
            raise ValueError("observed usage cannot acquire known pricing")
        if self.provider_audit is not None:
            if self.audit_hash != self.provider_audit.audit_hash:
                raise ValueError("invalid audit digest binding")
        elif self.audit_hash is not None:
            raise ValueError("audit digest without audit")
        if self.transport_outcome not in (None, "response", "unknown"):
            raise ValueError("invalid transport outcome")
        if self.business_outcome not in (None, "accepted", "rejected", "unknown"):
            raise ValueError("invalid business outcome")
        if self.http_status is not None and self.http_status != 0:
            if not 100 <= self.http_status <= 599:
                raise ValueError("invalid observed HTTP status")


@dataclass(frozen=True)
class RunCalls(RunModel):
    """One captured view, at most 44 rows; late reports can change later views."""

    run_id: str
    captured_at: datetime
    batch_frozen: bool
    batch_stop_code: str | None
    items: list[RunCall]

    def __post_init__(self) -> None:
        _uuid(self.run_id)
        if self.batch_stop_code is not None and (
            not self.batch_frozen or self.batch_stop_code not in _BATCH_STOPS
        ):
            raise ValueError("invalid batch stop evidence")
        if len(self.items) > 44:
            raise ValueError("too many physical calls")
        if len({row.physical_call_id for row in self.items}) != len(self.items):
            raise ValueError("duplicate physical call")
        ordinals = [row.ordinal for row in self.items]
        if ordinals != sorted(set(ordinals)):
            raise ValueError("invalid physical call order")
