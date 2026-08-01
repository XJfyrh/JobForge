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

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> Job:
        """Create a Job from an API response dictionary."""
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
    try:
        return datetime.fromisoformat(value.replace("Z", "+00:00"))
    except (ValueError, AttributeError):
        return None
