"""Strict Run v2 wire models from api/run/v2/openapi.yaml.

Counts and money are JSON-safe integers. Budget usage is conservative exposure,
not a provider invoice. Parsing never echoes rejected business content.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, fields
from datetime import datetime
from enum import Enum
from types import UnionType
from typing import Any, TypeVar, Union, get_args, get_origin, get_type_hints
from uuid import UUID

MAX_SAFE_INTEGER = (1 << 53) - 1
_T = TypeVar("_T", bound="RunModel")
_TIMESTAMP = re.compile(
    r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}"
    r"(?:\.[0-9]+)?(?:Z|[+-][0-9]{2}:[0-9]{2})\Z"
)


class RunState(str, Enum):
    """Durable execution states; stopping does not grant more execution time."""

    READY = "ready"
    RUNNING = "running"
    STOPPING = "stopping"
    RETRY_WAIT = "retry_wait"
    AWAITING_APPROVAL = "awaiting_approval"
    SUCCEEDED = "succeeded"
    FAILED = "failed"
    CANCELLED = "cancelled"

    @property
    def is_terminal(self) -> bool:
        """Whether execution has ended, independently of late usage reports."""
        return self in (self.SUCCEEDED, self.FAILED, self.CANCELLED)


@dataclass(frozen=True)
class RunModel:
    """Base for complete typed v2 responses; unknown fields fail closed."""

    @classmethod
    def from_dict(cls: type[_T], data: object) -> _T:
        """Parse the complete wire object without type coercion or defaults."""
        if not isinstance(data, dict) or set(data) != {f.name for f in fields(cls)}:
            raise ValueError("invalid run response fields")
        hints = get_type_hints(cls)
        return cls(**{key: _decode(hints[key], value) for key, value in data.items()})


@dataclass(frozen=True)
class RunError(RunModel):
    """Execution failure on an otherwise successful detail query."""

    code: str
    message: str


@dataclass(frozen=True)
class TicketVersion(RunModel):
    """Authoritative ticket revision from the immutable snapshot."""

    id: str
    revision: int


@dataclass(frozen=True)
class OrderVersion(RunModel):
    """A missing order preserves its expected relationship identity."""

    id: str | None
    exists: bool
    revision: int | None

    def __post_init__(self) -> None:
        if self.exists != (self.revision is not None) or (
            self.exists and self.id is None
        ):
            raise ValueError("invalid order version")


@dataclass(frozen=True)
class DeliveryVersion(RunModel):
    """The whole delivery event aggregate, including an explicit missing fact."""

    id: str | None
    exists: bool
    aggregate_revision: int | None

    def __post_init__(self) -> None:
        if self.exists != (self.aggregate_revision is not None) or (
            self.exists and self.id is None
        ):
            raise ValueError("invalid delivery version")


@dataclass(frozen=True)
class PolicyVersion(RunModel):
    """Immutable policy corpus identity."""

    version: str
    revision: int
    corpus_sha256: str

    def __post_init__(self) -> None:
        _hash(self.corpus_sha256)


@dataclass(frozen=True)
class IndexVersion(RunModel):
    """Immutable retrieval index identity."""

    id: str
    profile_hash: str
    content_hash: str

    def __post_init__(self) -> None:
        _uuid(self.id)
        _hash(self.profile_hash)
        _hash(self.content_hash)


@dataclass(frozen=True)
class VersionVector(RunModel):
    """Trusted complete snapshot version vector, never model-provided."""

    schema_version: int
    ticket: TicketVersion
    order: OrderVersion
    delivery: DeliveryVersion
    policy: PolicyVersion
    index: IndexVersion

    def __post_init__(self) -> None:
        if self.schema_version != 1 or (
            not self.order.exists and self.delivery.id is not None
        ):
            raise ValueError("invalid snapshot version vector")


@dataclass(frozen=True)
class RunUsage(RunModel):
    """Authorization counts and token/cost exposure, including unknown holds."""

    chat: int
    logical_tools: int
    query_embedding: int
    profile_metadata_http: int
    business_tool_http: int
    physical_http: int
    protocol_corrections: int
    tokens: int
    cost_microyuan: int


@dataclass(frozen=True)
class BudgetAccount(RunModel):
    """An account with confirmed usage and unrefunded reserved/unknown holds."""

    scope: str
    id: str
    limits: RunUsage
    used: RunUsage
    frozen: bool
    known_tokens: int
    known_cost_microyuan: int
    held_tokens: int
    held_cost_microyuan: int

    def __post_init__(self) -> None:
        if self.scope not in ("family", "tenant", "batch"):
            raise ValueError("invalid budget scope")
        if (
            self.used.tokens != self.known_tokens + self.held_tokens
            or self.used.cost_microyuan
            != self.known_cost_microyuan + self.held_cost_microyuan
        ):
            raise ValueError("invalid budget exposure")


@dataclass(frozen=True)
class RunBudget(RunModel):
    """All three shared accounts plus this Run's conservative usage."""

    family: BudgetAccount
    tenant: BudgetAccount
    batch: BudgetAccount
    run_usage: RunUsage

    def __post_init__(self) -> None:
        if (self.family.scope, self.tenant.scope, self.batch.scope) != (
            "family",
            "tenant",
            "batch",
        ):
            raise ValueError("invalid budget account binding")


@dataclass(frozen=True)
class Run(RunModel):
    """Tenant-authorized Run detail without Worker credentials or raw inputs."""

    run_id: str
    tenant_id: str
    business_request_id: str
    business_request_key: str
    ticket_id: str
    retry_of_run_id: str | None
    profile_id: str
    profile_hash: str
    budget_batch_id: str
    snapshot_id: str
    snapshot_hash: str
    version_vector: VersionVector
    state: RunState
    outcome: str | None
    error: RunError | None
    attempt_no: int
    recovery_count: int
    cursor_version: int
    run_timeout_seconds: int
    run_deadline: datetime
    attempt_deadline: datetime | None
    lease_until: datetime | None
    next_attempt_at: datetime | None
    permission_expires_at: datetime | None
    proposal_ref: str | None
    stop_reason: str | None
    cancel_requested_at: datetime | None
    created_at: datetime
    updated_at: datetime
    budget: RunBudget

    def __post_init__(self) -> None:
        for value in (self.run_id, self.business_request_id, self.snapshot_id):
            _uuid(value)
        if self.retry_of_run_id is not None:
            _uuid(self.retry_of_run_id)
        _hash(self.profile_hash)
        _hash(self.snapshot_hash)
        if not 1 <= self.run_timeout_seconds <= 86400 or self.recovery_count > 3:
            raise ValueError("invalid run bounds")
        if self.outcome not in (None, "no_action", "applied", "rejected"):
            raise ValueError("invalid run outcome")


@dataclass(frozen=True)
class RunSubmission(RunModel):
    """Submission/retry returns the first accepted Run and a reuse flag."""

    run: Run
    reused: bool


@dataclass(frozen=True)
class RunCancellation(RunModel):
    """Accepted cancellation operation and current execution state."""

    operation_id: str
    run: Run
    reused: bool

    def __post_init__(self) -> None:
        _uuid(self.operation_id)


@dataclass(frozen=True)
class RunPage(RunModel):
    """A single descending creation-time/ID page; cursors are opaque."""

    items: list[Run]
    next_cursor: str | None


@dataclass(frozen=True)
class RunStep(RunModel):
    """Protected accepted step content, fetched only on an explicit SDK call."""

    step_id: str
    sequence: int
    kind: str
    input_hash: str
    profile_hash: str
    snapshot_hash: str
    commit_hash: str
    output_ref: str
    output: Any
    cursor_version: int
    created_at: datetime

    def __post_init__(self) -> None:
        _uuid(self.step_id)
        for value in (
            self.input_hash,
            self.profile_hash,
            self.snapshot_hash,
            self.commit_hash,
        ):
            _hash(value)
        if not 1 <= self.sequence <= 32:
            raise ValueError("invalid step sequence")


@dataclass(frozen=True)
class RunStepPage(RunModel):
    """One ascending accepted-step page, never an automatic polling loop."""

    items: list[RunStep]
    next_after: int | None


@dataclass(frozen=True)
class RunEvent(RunModel):
    """Metadata-only event; business/model/tool content is deliberately absent."""

    sequence: int
    event_type: str
    state: RunState
    attempt_no: int
    cursor_version: int
    created_at: datetime

    def __post_init__(self) -> None:
        if self.sequence < 1:
            raise ValueError("invalid event sequence")


@dataclass(frozen=True)
class RunEventPage(RunModel):
    """One ascending event page."""

    items: list[RunEvent]
    next_after: int | None


@dataclass(frozen=True)
class RunResult(RunModel):
    """An explicit availability/ref view; a proposal is never an applied action."""

    available: bool
    kind: str | None
    ref: str | None

    def __post_init__(self) -> None:
        if self.kind not in (None, "proposal", "no_action", "final"):
            raise ValueError("invalid result kind")
        if self.available != (self.kind is not None and self.ref is not None):
            raise ValueError("invalid result availability")
        if not self.available and (self.kind is not None or self.ref is not None):
            raise ValueError("invalid absent result")


def _uuid(value: str) -> None:
    if str(UUID(value)) != value:
        raise ValueError("invalid UUID")


def _hash(value: str) -> None:
    if re.fullmatch("[0-9a-f]{64}", value) is None:
        raise ValueError("invalid SHA-256")


def _decode(expected: Any, value: Any) -> Any:
    """Apply the small source-contract type vocabulary without a framework."""
    if expected is Any:
        return value
    origin = get_origin(expected)
    if origin in (Union, UnionType):
        for option in get_args(expected):
            try:
                return _decode(option, value)
            except (TypeError, ValueError):
                continue
        raise ValueError("invalid nullable response field")
    if expected is type(None):
        if value is not None:
            raise ValueError("expected null")
        return None
    if origin is list:
        if not isinstance(value, list) or len(value) > 100:
            raise ValueError("invalid response page")
        return [_decode(get_args(expected)[0], entry) for entry in value]
    if expected in (str, bool, int):
        if type(value) is not expected:
            raise ValueError("invalid response field type")
        if expected is int and not 0 <= value <= MAX_SAFE_INTEGER:
            raise ValueError("invalid response integer")
        return value
    if expected is datetime:
        if not isinstance(value, str) or _TIMESTAMP.fullmatch(value) is None:
            raise ValueError("invalid response timestamp")
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
        if parsed.tzinfo is None:
            raise ValueError("invalid response timezone")
        return parsed
    if issubclass(expected, RunState):
        if not isinstance(value, str):
            raise ValueError("invalid run state")
        return expected(value)
    if issubclass(expected, RunModel):
        return expected.from_dict(value)
    raise TypeError("unsupported run response type")
