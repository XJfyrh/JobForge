"""JobForge SDK data models."""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime
from enum import Enum
from typing import Any


class JobState(str, Enum):
    """Job lifecycle states."""

    SCHEDULED = "scheduled"
    READY = "ready"
    RUNNING = "running"
    CANCELLING = "cancelling"
    RETRY_WAIT = "retry_wait"
    SUCCEEDED = "succeeded"
    DEAD = "dead"
    CANCELLED = "cancelled"

    @property
    def is_terminal(self) -> bool:
        """Return True if this is a terminal state."""
        return self in (JobState.SUCCEEDED, JobState.DEAD, JobState.CANCELLED)


@dataclass
class Job:
    """Represents a JobForge job.

    Attributes:
        id: Unique job identifier (UUID).
        tenant_id: Owning tenant identifier.
        queue: Logical queue name.
        type: Registered task type (e.g., "demo.echo").
        payload: Task parameters as a dictionary.
        priority: Priority (higher = more urgent).
        state: Current lifecycle state.
        run_at: Earliest execution time.
        attempt: Current attempt number.
        max_attempts: Maximum execution attempts.
        timeout_seconds: Per-attempt timeout.
        idempotency_key: Optional submission dedup key.
        lease_owner: Current worker ID (if running).
        lease_until: Lease expiry time (if running).
        fencing_token: Monotonic token for stale write rejection.
        trace_id: Distributed trace identifier.
        retry_of_job_id: Original job ID (if this is a retry clone).
        created_at: Creation timestamp.
        updated_at: Last update timestamp.
        attempts: Execution timeline (one entry per attempt, FR-002).
        result_ref: Opaque business artifact reference; never fetched automatically.
    """

    id: str
    tenant_id: str
    queue: str
    type: str
    payload: dict[str, Any] = field(default_factory=dict)
    priority: int = 0
    state: JobState = JobState.READY
    run_at: datetime | None = None
    attempt: int = 0
    max_attempts: int = 3
    timeout_seconds: int = 300
    idempotency_key: str | None = None
    lease_owner: str | None = None
    lease_until: datetime | None = None
    fencing_token: int = 0
    trace_id: str | None = None
    retry_of_job_id: str | None = None
    created_at: datetime | None = None
    updated_at: datetime | None = None
    attempts: list[Attempt] = field(default_factory=list)
    result_ref: str | None = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> Job:
        """Create a Job from an API response dictionary."""
        _check_fields(data, str, ("id", "tenant_id", "queue", "type"))
        if not data["id"]:
            raise ValueError("missing job id")
        _check_fields(
            data,
            int,
            ("priority", "attempt", "max_attempts", "timeout_seconds", "fencing_token"),
        )
        _check_fields(
            data,
            str,
            (
                "idempotency_key",
                "lease_owner",
                "trace_id",
                "retry_of_job_id",
                "result_ref",
            ),
            nullable=True,
        )
        attempts = data.get("attempts")
        if attempts is not None and not isinstance(attempts, list):
            raise TypeError("invalid attempts field")
        return cls(
            id=data["id"],
            tenant_id=data.get("tenant_id", ""),
            queue=data.get("queue", ""),
            type=data.get("type", ""),
            payload=data.get("payload") or {},
            priority=data.get("priority", 0),
            state=JobState(data.get("state", "ready")),
            run_at=_parse_datetime(data.get("run_at")),
            attempt=data.get("attempt", 0),
            max_attempts=data.get("max_attempts", 3),
            timeout_seconds=data.get("timeout_seconds", 300),
            idempotency_key=data.get("idempotency_key"),
            lease_owner=data.get("lease_owner"),
            lease_until=_parse_datetime(data.get("lease_until")),
            fencing_token=data.get("fencing_token", 0),
            trace_id=data.get("trace_id"),
            retry_of_job_id=data.get("retry_of_job_id"),
            created_at=_parse_datetime(data.get("created_at")),
            updated_at=_parse_datetime(data.get("updated_at")),
            attempts=[Attempt.from_dict(a) for a in attempts or []],
            result_ref=data.get("result_ref") or None,
        )


@dataclass
class Attempt:
    """One entry of a job's attempt timeline (PRD 6.2 / FR-002).

    Attributes:
        attempt_no: 1-based attempt number.
        worker_id: Worker that executed the attempt.
        fencing_token: Lease fencing token of the attempt.
        started_at: Attempt start timestamp.
        finished_at: Attempt finish timestamp (None while running).
        outcome: succeeded, failed_retry, failed_dead, cancelled or lease_expired.
        error_code: Machine-readable error code (if failed).
        error_message: Human-readable error message (if failed).
        duration_ms: Execution duration in milliseconds (if finished).
    """

    attempt_no: int
    worker_id: str = ""
    fencing_token: int = 0
    started_at: datetime | None = None
    finished_at: datetime | None = None
    outcome: str = ""
    error_code: str | None = None
    error_message: str | None = None
    duration_ms: int | None = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> Attempt:
        """Create an Attempt from an API response dictionary."""
        _check_fields(data, str, ("worker_id", "outcome"))
        _check_fields(data, int, ("attempt_no", "fencing_token"))
        _check_fields(data, str, ("error_code", "error_message"), nullable=True)
        _check_fields(data, int, ("duration_ms",), nullable=True)
        return cls(
            attempt_no=data.get("attempt_no", 0),
            worker_id=data.get("worker_id", ""),
            fencing_token=data.get("fencing_token", 0),
            started_at=_parse_datetime(data.get("started_at")),
            finished_at=_parse_datetime(data.get("finished_at")),
            outcome=data.get("outcome", ""),
            error_code=data.get("error_code"),
            error_message=data.get("error_message"),
            duration_ms=data.get("duration_ms"),
        )


@dataclass
class SubmitResponse:
    """Response from job submission."""

    job_id: str
    state: str
    deduplicated: bool = False


def _parse_datetime(value: str | None) -> datetime | None:
    """Parse an ISO 8601 datetime string."""
    if value is None:
        return None
    if not isinstance(value, str):
        raise TypeError("invalid datetime field")
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


def _check_fields(
    data: dict[str, Any],
    expected: type[str] | type[int],
    names: tuple[str, ...],
    *,
    nullable: bool = False,
) -> None:
    """Validate present fields; keep legacy defaults and ignore unknown fields."""
    if not isinstance(data, dict):
        raise TypeError("invalid response object")
    for name in names:
        if name not in data or (nullable and data[name] is None):
            continue
        # JSON booleans must not pass as integers (bool subclasses int).
        if type(data[name]) is not expected:
            raise TypeError(f"invalid {name} field")
