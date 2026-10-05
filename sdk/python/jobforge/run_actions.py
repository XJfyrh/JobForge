"""Public approval and business-effect views; never execution credentials."""

from __future__ import annotations

import re
from dataclasses import dataclass
from datetime import datetime
from typing import Any

from jobforge.run_models import Run, RunModel, _hash, _uuid

_FIELDS = {
    "ticket.order_id",
    "order.delivery_id",
    "delivery.usable_tracking_events",
    "delivery.delivered_event",
    "ticket.problem_description",
}


def _strings(values: list[str], maximum: int, *, nonempty: bool = False) -> None:
    if (
        len(values) > maximum
        or len(set(values)) != len(values)
        or (nonempty and not values)
        or any(not value or len(value.encode("utf-8")) > 256 for value in values)
    ):
        raise ValueError("invalid proposal strings")


def _claim(value: Any, evidence: list[str]) -> None:
    if not isinstance(value, dict):
        raise ValueError("invalid proposal claim")
    kind = value.get("kind")
    variants = {
        "timing": (
            {"test", "event_id"},
            "test",
            {
                "delivered_not_late",
                "delivered_late",
                "outstanding_not_overdue",
                "outstanding_overdue_lt48",
                "outstanding_overdue_ge48",
            },
        ),
        "dispute": (
            {"type", "delivered_event_id"},
            "type",
            {
                "non_receipt",
                "wrong_address",
                "unauthorized_recipient",
                "unauthorized_safe_place",
            },
        ),
        "critical": (
            {"status", "event_id"},
            "status",
            {"lost", "damaged", "returned_to_sender"},
        ),
        "missing": ({"field"}, "field", _FIELDS),
        "conflict": (
            {"type", "event_ids"},
            "type",
            {
                "pre_handover",
                "same_time",
                "source_key",
                "post_delivery",
                "order_delivery",
            },
        ),
        "correction": ({"recovery_event_id", "corrected_event_id"}, None, set()),
        "ticket_status": (
            {"mode"},
            "mode",
            {"informational_no_action", "preserve_escalated"},
        ),
    }
    if not isinstance(kind, str) or kind not in variants:
        raise ValueError("invalid proposal claim kind")
    keys, enum_key, choices = variants[kind]
    if set(value) != keys | {"kind", "refs"} or (
        enum_key is not None and value[enum_key] not in choices
    ):
        raise ValueError("invalid proposal claim fields")
    for key in keys - {enum_key, "event_ids"}:
        if (
            not isinstance(value[key], str)
            or re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._:-]{0,127}", value[key]) is None
        ):
            raise ValueError("invalid proposal claim identifier")
    if kind == "conflict":
        ids = value["event_ids"]
        if not isinstance(ids, list) or len(ids) != (
            0 if value["type"] == "order_delivery" else 2
        ):
            raise ValueError("invalid conflict identifiers")
        if any(
            not isinstance(item, str)
            or re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._:-]{0,127}", item) is None
            for item in ids
        ) or len(set(ids)) != len(ids):
            raise ValueError("invalid conflict identifiers")
    if (
        kind == "correction"
        and value["recovery_event_id"] == value["corrected_event_id"]
    ):
        raise ValueError("invalid correction identifiers")
    refs = value["refs"]
    if not isinstance(refs, list) or not 1 <= len(refs) <= 10:
        raise ValueError("invalid claim references")
    seen: set[tuple[str, str]] = set()
    for ref in refs:
        if (
            not isinstance(ref, dict)
            or set(ref) != {"evidence_ref", "source_pointer"}
            or ref["evidence_ref"] not in evidence
            or not isinstance(ref["source_pointer"], str)
            or not 1 <= len(ref["source_pointer"].encode("utf-8")) <= 128
        ):
            raise ValueError("invalid claim reference")
        pair = (ref["evidence_ref"], ref["source_pointer"])
        if pair in seen:
            raise ValueError("duplicate claim reference")
        seen.add(pair)


@dataclass(frozen=True)
class ApprovalProposal(RunModel):
    """Original persisted support-v1 proposal, including protected evidence."""

    decision: str
    action: str
    conclusion: str
    requested_fields: list[str]
    target_ticket_status: str
    summary: str
    claims: list[Any]
    evidence_refs: list[str]

    def __post_init__(self) -> None:
        _strings(self.requested_fields, 5)
        _strings(self.evidence_refs, 32, nonempty=True)
        if (
            self.decision != "proposal"
            or self.action
            not in {"record_conclusion", "request_information", "escalate"}
            or self.conclusion
            not in {"on_time", "delayed", "disputed", "insufficient", "conflicting"}
            or not set(self.requested_fields) <= _FIELDS
            or not 1 <= len(self.summary.encode("utf-8")) <= 8192
            or not 1 <= len(self.claims) <= 4
        ):
            raise ValueError("invalid approval proposal")
        if (
            self.target_ticket_status
            not in {"open", "informational_only", "awaiting_information", "escalated"}
            or (
                self.action == "request_information"
                and self.target_ticket_status != "awaiting_information"
            )
            or (self.action == "escalate" and self.target_ticket_status != "escalated")
        ):
            raise ValueError("invalid proposal status")
        for index, claim in enumerate(self.claims):
            _claim(claim, self.evidence_refs)
            if claim in self.claims[:index]:
                raise ValueError("duplicate proposal claim")


@dataclass(frozen=True)
class ApprovalView(RunModel):
    """First decision is stable even when execution later terminates."""

    run_id: str
    status: str
    proposal_hash: str
    proposal_ref: str
    proposal: ApprovalProposal
    permission_expires_at: datetime
    approval_id: str | None
    actor_id: str | None
    decided_at: datetime | None
    available: bool

    def __post_init__(self) -> None:
        _uuid(self.run_id)
        _hash(self.proposal_hash)
        if (
            self.status not in {"pending", "approved", "rejected"}
            or not self.proposal_ref
        ):
            raise ValueError("invalid approval view")
        decided = self.status != "pending"
        if any(
            (item is not None) != decided
            for item in (self.approval_id, self.actor_id, self.decided_at)
        ) or (decided and self.available):
            raise ValueError("invalid approval decision")
        if self.approval_id is not None:
            _uuid(self.approval_id)


@dataclass(frozen=True)
class ApprovalResponse(RunModel):
    """Accepted first approval plus current Run; replay never requeues."""

    approval: ApprovalView
    run: Run
    reused: bool

    def __post_init__(self) -> None:
        if self.approval.run_id != self.run.run_id:
            raise ValueError("invalid approval Run binding")


@dataclass(frozen=True)
class ActionReceipt(RunModel):
    """Immutable first business commit, with UTC Unix microsecond times."""

    schema_version: int
    tenant_id: str
    operation_id: str
    business_request_id: str
    authorization_hash: str
    proposal_hash: str
    parameters_hash: str
    approval_id: str
    ticket_id: str
    before_revision: int
    after_revision: int
    ticket_status: str
    applied_at: int
    retain_until: int
    receipt_hash: str

    def __post_init__(self) -> None:
        for value in (self.operation_id, self.business_request_id, self.approval_id):
            _uuid(value)
        for value in (
            self.authorization_hash,
            self.proposal_hash,
            self.parameters_hash,
            self.receipt_hash,
        ):
            _hash(value)
        if (
            self.schema_version != 1
            or self.before_revision < 1
            or self.after_revision != self.before_revision + 1
            or self.applied_at < 1
            or self.retain_until < self.applied_at + 30 * 86400 * 1_000_000
            or self.ticket_status
            not in {"open", "informational_only", "awaiting_information", "escalated"}
        ):
            raise ValueError("invalid action receipt")


@dataclass(frozen=True)
class EffectView(RunModel):
    """Independent business fact; querying it never changes terminal results."""

    state: str
    operation_id: str | None
    authorization_hash: str | None
    effect_version: int
    receipt_ref: str | None
    receipt: ActionReceipt | None
    source: str | None
    observed_at: datetime | None

    def __post_init__(self) -> None:
        if self.state not in {"none", "unknown", "applied"}:
            raise ValueError("invalid effect state")
        authorized = self.state != "none"
        if any(
            (value is not None) != authorized
            for value in (self.operation_id, self.authorization_hash)
        ):
            raise ValueError("invalid effect identity")
        if self.operation_id is not None:
            _uuid(self.operation_id)
        if self.authorization_hash is not None:
            _hash(self.authorization_hash)
        applied = self.state == "applied"
        if self.effect_version != int(applied) or any(
            (value is not None) != applied
            for value in (self.receipt_ref, self.receipt, self.source, self.observed_at)
        ):
            raise ValueError("invalid effect evidence")
        if self.receipt is not None and (
            self.receipt.operation_id != self.operation_id
            or self.receipt.authorization_hash != self.authorization_hash
            or self.source
            not in {"action_response", "worker_query", "reconcile", "retry"}
        ):
            raise ValueError("invalid effect receipt binding")


@dataclass(frozen=True)
class ActionCall(RunModel):
    """Physical action permission, separate from model usage and fees."""

    physical_call_id: str
    operation_id: str
    authorization_hash: str
    kind: str
    status: str
    reserved_at: datetime
    dispatch_expires_at: datetime
    call_deadline: datetime
    transport_outcome: str | None
    observation_hash: str | None
    observed_at: datetime | None
    provider_metering: str

    def __post_init__(self) -> None:
        _uuid(self.physical_call_id)
        _uuid(self.operation_id)
        _hash(self.authorization_hash)
        if (
            self.kind not in {"action_write", "receipt_query"}
            or self.status not in {"reserved", "observed", "unknown"}
            or self.provider_metering != "not_applicable"
            or not self.reserved_at < self.dispatch_expires_at <= self.call_deadline
            or (self.call_deadline - self.reserved_at).total_seconds() > 10
        ):
            raise ValueError("invalid action call")
        observed = self.status == "observed"
        if any(
            (value is not None) != observed
            for value in (
                self.transport_outcome,
                self.observation_hash,
                self.observed_at,
            )
        ) or self.transport_outcome not in {
            None,
            "response",
            "timeout",
            "network_error",
        }:
            raise ValueError("invalid action observation")
        if self.observation_hash is not None:
            _hash(self.observation_hash)


@dataclass(frozen=True)
class ActionCallsResponse(RunModel):
    """At most four writes plus four receipt queries on the original Run."""

    items: list[ActionCall]

    def __post_init__(self) -> None:
        if (
            len(self.items) > 8
            or len({item.physical_call_id for item in self.items}) != len(self.items)
            or any(
                sum(item.kind == kind for item in self.items) > 4
                for kind in ("action_write", "receipt_query")
            )
        ):
            raise ValueError("invalid action call ledger")
