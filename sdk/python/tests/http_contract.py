"""Real HTTP contract runner launched by TestPythonHTTPContract (no mocks)."""

from __future__ import annotations

import sys
import time
from datetime import datetime, timedelta, timezone
from uuid import uuid4

import pytest
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider

from jobforge import (
    AlreadyTerminalError,
    ConflictError,
    InvalidArgumentError,
    InvalidTransitionError,
    JobForgeClient,
    NotFoundError,
    QueueOverloadedError,
    UnauthorizedError,
)
from jobforge.models import Job, JobState


def wait_state(client: JobForgeClient, job_id: str, state: JobState) -> Job:
    """Wait with a fixed total deadline, never duplicating job submission."""
    deadline = time.monotonic() + 35
    while time.monotonic() < deadline:
        job = client.get(job_id)
        if job.state == state:
            return job
        time.sleep(0.05)
    raise AssertionError(f"job {job_id} did not reach {state}: {job.state}")


def main() -> None:
    """Exercise the installed SDK against the production HTTP router."""
    url, queue = sys.argv[1:]
    trace.set_tracer_provider(TracerProvider())
    parent = "00-11111111111111111111111111111111-2222222222222222-01"
    past = datetime.now(timezone.utc) - timedelta(minutes=1)
    with (
        JobForgeClient(url, "contract-a", traceparent=parent) as client,
        JobForgeClient(url, "contract-b") as foreign,
        JobForgeClient(url, "invalid-key") as unauthorized,
    ):
        with pytest.raises(UnauthorizedError):
            unauthorized.get(str(uuid4()))
        with pytest.raises(NotFoundError):
            client.get(str(uuid4()))
        with pytest.raises(InvalidArgumentError):
            client.submit(queue, "unregistered.type", {})
        with pytest.raises(InvalidArgumentError):
            client.submit(queue, "demo.echo", {}, timeout_seconds=-1)

        payload = {"message": "synthetic contract"}
        submitted = client.submit(
            queue, "demo.echo", payload, run_at=past, idempotency_key="contract-same"
        )
        repeated = client.submit(
            queue, "demo.echo", payload, run_at=past, idempotency_key="contract-same"
        )
        assert repeated.job_id == submitted.job_id and repeated.deduplicated
        with pytest.raises(ConflictError):
            client.submit(
                queue, "demo.echo", {"different": True}, idempotency_key="contract-same"
            )
        completed = wait_state(client, submitted.job_id, JobState.SUCCEEDED)
        assert completed.result_ref and completed.result_ref.startswith("echo:")
        assert completed.trace_id == "11111111111111111111111111111111"
        assert len(completed.attempts) == 1
        attempt = completed.attempts[0]
        assert attempt.outcome == "succeeded"
        assert (
            attempt.started_at
            and attempt.finished_at
            and attempt.duration_ms is not None
        )
        assert attempt.fencing_token == completed.fencing_token
        for operation in (foreign.get, foreign.cancel, foreign.retry):
            with pytest.raises(NotFoundError):
                operation(submitted.job_id)
        for operation in (client.cancel, client.retry):
            with pytest.raises(AlreadyTerminalError):
                operation(submitted.job_id)

        delayed = client.submit(
            queue,
            "demo.echo",
            {},
            run_at=datetime.now(timezone.utc) + timedelta(hours=1),
        )
        with pytest.raises(InvalidTransitionError):
            client.retry(delayed.job_id)
        client.cancel(delayed.job_id)
        assert client.get(delayed.job_id).state == JobState.CANCELLED
        retried = client.retry(delayed.job_id)
        assert retried.job_id != delayed.job_id
        assert client.get(retried.job_id).retry_of_job_id == delayed.job_id
        wait_state(client, retried.job_id, JobState.SUCCEEDED)
        assert client.get(delayed.job_id).state == JobState.CANCELLED

        sleeping = client.submit(
            queue, "demo.sleep", {"duration_ms": 30000}, run_at=past
        )
        wait_state(client, sleeping.job_id, JobState.RUNNING)
        client.cancel(sleeping.job_id)
        cancelled = wait_state(client, sleeping.job_id, JobState.CANCELLED)
        assert cancelled.result_ref is None
        assert cancelled.attempts[0].outcome == "cancelled"

        failed = client.submit(
            queue, "demo.fail", {"retryable": False}, run_at=past, max_attempts=1
        )
        dead = wait_state(client, failed.job_id, JobState.DEAD)
        assert dead.attempts[0].outcome == "failed_dead"
        dead_retry = client.retry(dead.id)
        assert client.get(dead_retry.job_id).retry_of_job_id == dead.id
        wait_state(client, dead_retry.job_id, JobState.DEAD)

        # No Worker consumes this dedicated queue: backpressure is deterministic.
        full = queue + "-overloaded"
        for _ in range(2):
            client.submit(full, "demo.echo", {})
        with pytest.raises(QueueOverloadedError) as captured:
            client.submit(full, "demo.echo", {})
        assert captured.value.status_code == 429 and captured.value.retryable
    print(
        "PASS real HTTP SDK: lifecycle, result, attempts, idempotency, "
        "isolation, errors, trace"
    )


if __name__ == "__main__":
    main()
