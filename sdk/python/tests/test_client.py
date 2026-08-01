"""Tests for the JobForge Python SDK client."""

from __future__ import annotations

import httpx
import pytest

from jobforge import (
    AlreadyTerminalError,
    JobForgeClient,
    NotFoundError,
    QueueOverloadedError,
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
                    "job_id": "test-job-123",
                    "state": "ready",
                    "deduplicated": False,
                },
            )

        if path == "/v1/jobs/test-job-123" and request.method == "GET":
            return httpx.Response(
                200,
                json={
                    "id": "test-job-123",
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

        if path == "/v1/jobs/not-found" and request.method == "GET":
            return httpx.Response(
                404,
                json={"code": "NOT_FOUND", "message": "job not found"},
            )

        if path == "/v1/jobs/test-job-123:cancel" and request.method == "POST":
            return httpx.Response(200, json={})

        if path == "/v1/jobs/terminal-job:cancel" and request.method == "POST":
            return httpx.Response(
                409,
                json={"code": "ALREADY_TERMINAL", "message": "job already succeeded"},
            )

        if path == "/v1/jobs/test-job-123:retry" and request.method == "POST":
            return httpx.Response(
                202,
                json={
                    "job_id": "retry-job-456",
                    "state": "ready",
                    "deduplicated": False,
                },
            )

        if path == "/v1/jobs" and request.method == "POST":
            # Check for queue overloaded
            return httpx.Response(
                429,
                json={"code": "QUEUE_OVERLOADED", "message": "queue at capacity"},
            )

        return httpx.Response(404, json={"code": "NOT_FOUND", "message": "unknown route"})

    return httpx.MockTransport(handler)


@pytest.fixture
def client(mock_transport: httpx.MockTransport) -> JobForgeClient:
    """Create a test client with mock transport."""
    client = JobForgeClient(
        base_url="http://testserver",
        api_key="test-key",
    )
    # Replace the internal client with mock transport
    client._client = httpx.Client(
        base_url="http://testserver",
        transport=mock_transport,
        headers=client._build_headers(),
    )
    return client


class TestSubmit:
    def test_submit_success(self, client: JobForgeClient) -> None:
        result = client.submit(
            queue="default",
            type="demo.echo",
            payload={"message": "hello"},
        )
        assert result.job_id == "test-job-123"
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
        assert result.job_id == "test-job-123"


class TestGet:
    def test_get_success(self, client: JobForgeClient) -> None:
        job = client.get("test-job-123")
        assert job.id == "test-job-123"
        assert job.state == JobState.RUNNING
        assert job.attempt == 1

    def test_get_not_found(self, client: JobForgeClient) -> None:
        with pytest.raises(NotFoundError) as exc_info:
            client.get("not-found")
        assert exc_info.value.code == "NOT_FOUND"


class TestCancel:
    def test_cancel_success(self, client: JobForgeClient) -> None:
        # Should not raise
        client.cancel("test-job-123")

    def test_cancel_terminal(self, client: JobForgeClient) -> None:
        with pytest.raises(AlreadyTerminalError) as exc_info:
            client.cancel("terminal-job")
        assert exc_info.value.code == "ALREADY_TERMINAL"


class TestRetry:
    def test_retry_success(self, client: JobForgeClient) -> None:
        result = client.retry("test-job-123")
        assert result.job_id == "retry-job-456"
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
