"""Tests for the JobForge Python SDK client."""

from __future__ import annotations

from collections.abc import Iterator

import httpx
import pytest

from jobforge import (
    AlreadyTerminalError,
    JobForgeClient,
    NotFoundError,
)
from jobforge.models import Job, JobState


@pytest.fixture
def mock_transport() -> httpx.MockTransport:
    """Create a mock transport for testing."""

    def handler(request: httpx.Request) -> httpx.Response:
        # Route based on path
        path = request.url.path

        if path == "/v1/jobs" and request.method == "POST":
            return httpx.Response(
                202,
                json={
                    "job_id": "11111111-1111-4111-8111-111111111111",
                    "state": "ready",
                    "deduplicated": False,
                },
            )

        if (
            path == "/v1/jobs/11111111-1111-4111-8111-111111111111"
            and request.method == "GET"
        ):
            return httpx.Response(
                200,
                json={
                    "id": "11111111-1111-4111-8111-111111111111",
                    "tenant_id": "test-tenant",
                    "queue": "default",
                    "type": "demo.echo",
                    "payload": {"message": "hello"},
                    "state": "running",
                    "attempt": 1,
                    "max_attempts": 3,
                    "fencing_token": 1,
                },
            )

        if (
            path == "/v1/jobs/33333333-3333-4333-8333-333333333333"
            and request.method == "GET"
        ):
            return httpx.Response(
                404,
                json={"error": {"code": "NOT_FOUND", "message": "job not found"}},
            )

        if (
            path == "/v1/jobs/11111111-1111-4111-8111-111111111111:cancel"
            and request.method == "POST"
        ):
            return httpx.Response(200, json={})

        if (
            path == "/v1/jobs/44444444-4444-4444-8444-444444444444:cancel"
            and request.method == "POST"
        ):
            return httpx.Response(
                409,
                json={
                    "error": {
                        "code": "ALREADY_TERMINAL",
                        "message": "job already succeeded",
                    }
                },
            )

        if (
            path == "/v1/jobs/11111111-1111-4111-8111-111111111111:retry"
            and request.method == "POST"
        ):
            return httpx.Response(
                202,
                json={
                    "job_id": "22222222-2222-4222-8222-222222222222",
                    "state": "ready",
                    "deduplicated": False,
                },
            )

        return httpx.Response(
            404, json={"error": {"code": "NOT_FOUND", "message": "unknown route"}}
        )

    return httpx.MockTransport(handler)


@pytest.fixture
def client(mock_transport: httpx.MockTransport) -> Iterator[JobForgeClient]:
    """Create a test client with mock transport."""
    with JobForgeClient(
        base_url="http://testserver",
        api_key="test-key",
        transport=mock_transport,
    ) as client:
        yield client


class TestSubmit:
    def test_submit_success(self, client: JobForgeClient) -> None:
        result = client.submit(
            queue="default",
            type="demo.echo",
            payload={"message": "hello"},
        )
        assert result.job_id == "11111111-1111-4111-8111-111111111111"
        assert result.state == "ready"
        assert result.deduplicated is False

    def test_submit_with_options(self, client: JobForgeClient) -> None:
        result = client.submit(
            queue="default",
            type="demo.echo",
            payload={},
            priority=10,
            max_attempts=5,
            timeout_seconds=600,
            idempotency_key="unique-key",
        )
        assert result.job_id == "11111111-1111-4111-8111-111111111111"


class TestGet:
    def test_get_success(self, client: JobForgeClient) -> None:
        job = client.get("11111111-1111-4111-8111-111111111111")
        assert job.id == "11111111-1111-4111-8111-111111111111"
        assert job.state == JobState.RUNNING
        assert job.attempt == 1

    def test_get_not_found(self, client: JobForgeClient) -> None:
        with pytest.raises(NotFoundError) as exc_info:
            client.get("33333333-3333-4333-8333-333333333333")
        assert exc_info.value.code == "NOT_FOUND"


class TestCancel:
    def test_cancel_success(self, client: JobForgeClient) -> None:
        # Should not raise
        client.cancel("11111111-1111-4111-8111-111111111111")

    def test_cancel_terminal(self, client: JobForgeClient) -> None:
        with pytest.raises(AlreadyTerminalError) as exc_info:
            client.cancel("44444444-4444-4444-8444-444444444444")
        assert exc_info.value.code == "ALREADY_TERMINAL"


class TestRetry:
    def test_retry_success(self, client: JobForgeClient) -> None:
        result = client.retry("11111111-1111-4111-8111-111111111111")
        assert result.job_id == "22222222-2222-4222-8222-222222222222"
        assert result.state == "ready"


class TestJobModel:
    def test_job_state_terminal(self) -> None:
        assert JobState.SUCCEEDED.is_terminal
        assert JobState.DEAD.is_terminal
        assert JobState.CANCELLED.is_terminal
        assert not JobState.RUNNING.is_terminal
        assert not JobState.READY.is_terminal

    def test_job_from_dict(self) -> None:
        data = {
            "id": "job-1",
            "tenant_id": "tenant-1",
            "queue": "q1",
            "type": "demo.echo",
            "state": "ready",
        }
        job = Job.from_dict(data)
        assert job.id == "job-1"
        assert job.state == JobState.READY

    @pytest.mark.parametrize("result_ref", [None, "", "artifact:v1:index-1"])
    def test_optional_fields_and_unknown_extensions(
        self, result_ref: str | None
    ) -> None:
        data = {
            "id": "11111111-1111-4111-8111-111111111111",
            "result_ref": result_ref,
            "attempts": None,
            "future_field": {"version": 2},
        }
        job = Job.from_dict(data)
        assert job.result_ref == (result_ref or None)
        assert job.attempts == []
        assert job.created_at is None

    def test_attempt_timeline(self) -> None:
        job = Job.from_dict(
            {
                "id": "11111111-1111-4111-8111-111111111111",
                "created_at": "2026-09-15T08:00:00Z",
                "attempts": [
                    {
                        "attempt_no": 1,
                        "worker_id": "worker-1",
                        "fencing_token": 1,
                        "started_at": "2026-09-15T08:00:01Z",
                        "finished_at": None,
                        "duration_ms": None,
                        "error_code": None,
                        "error_message": None,
                    }
                ],
            }
        )
        assert job.created_at is not None
        assert job.created_at.utcoffset() is not None
        assert job.attempts[0].attempt_no == 1
        assert job.attempts[0].started_at is not None
        assert job.attempts[0].finished_at is None
