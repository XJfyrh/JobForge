"""JobForge SDK client.

Provides a synchronous HTTP client for the JobForge API.
"""

from __future__ import annotations

from datetime import datetime
from typing import Any

import httpx

from jobforge.errors import from_response
from jobforge.models import Job, SubmitResponse


class JobForgeClient:
    """Synchronous client for the JobForge HTTP API.

    Args:
        base_url: Base URL of the JobForge API (e.g., "http://localhost:8080").
        api_key: API key for authentication.
        timeout: Request timeout in seconds (default 30).
        trace_id: Optional trace ID to propagate to all requests.

    Example:
        >>> client = JobForgeClient("http://localhost:8080", "dev-api-key")
        >>> job = client.submit(queue="default", type="demo.echo", payload={"msg": "hi"})
        >>> print(job.job_id, job.state)
    """

    def __init__(
        self,
        base_url: str,
        api_key: str,
        timeout: float = 30.0,
        trace_id: str | None = None,
    ) -> None:
        self._base_url = base_url.rstrip("/")
        self._api_key = api_key
        self._trace_id = trace_id
        self._client = httpx.Client(
            base_url=self._base_url,
            timeout=timeout,
            headers=self._build_headers(),
        )

    def _build_headers(self) -> dict[str, str]:
        """Build common headers for all requests."""
        headers = {
            "Authorization": f"Bearer {self._api_key}",
            "Content-Type": "application/json",
        }
        if self._trace_id:
            headers["X-Trace-ID"] = self._trace_id
        return headers

    def _handle_error(self, response: httpx.Response) -> None:
        """Raise appropriate exception for error responses."""
        if response.status_code >= 400:
            try:
                data = response.json()
                code = data.get("code", "INTERNAL")
                message = data.get("message", response.text)
            except Exception:
                code = "INTERNAL"
                message = response.text
            raise from_response(code, message)

    def submit(
        self,
        queue: str,
        type: str,
        payload: dict[str, Any],
        *,
        priority: int = 0,
        run_at: datetime | None = None,
        max_attempts: int = 3,
        timeout_seconds: int = 300,
        idempotency_key: str | None = None,
    ) -> SubmitResponse:
        """Submit a new job.

        Args:
            queue: Target queue name.
            type: Registered task type (e.g., "demo.echo").
            payload: Task parameters.
            priority: Priority (higher = more urgent).
            run_at: Earliest execution time (None = immediate).
            max_attempts: Maximum execution attempts (1-10).
            timeout_seconds: Per-attempt timeout.
            idempotency_key: Optional dedup key.

        Returns:
            SubmitResponse with job_id, state, and deduplicated flag.

        Raises:
            InvalidArgumentError: If parameters are invalid.
            QueueOverloadedError: If queue is at capacity.
        """
        body: dict[str, Any] = {
            "queue": queue,
            "type": type,
            "payload": payload,
            "priority": priority,
            "max_attempts": max_attempts,
            "timeout_seconds": timeout_seconds,
        }
        if run_at is not None:
            body["run_at"] = run_at.isoformat()
        if idempotency_key is not None:
            body["idempotency_key"] = idempotency_key

        response = self._client.post("/v1/jobs", json=body)
        self._handle_error(response)

        data = response.json()
        return SubmitResponse(
            job_id=data["job_id"],
            state=data["state"],
            deduplicated=data.get("deduplicated", False),
        )

    def get(self, job_id: str) -> Job:
        """Get job details by ID.

        Args:
            job_id: Job UUID.

        Returns:
            Job object with full details.

        Raises:
            NotFoundError: If job does not exist or belongs to another tenant.
        """
        response = self._client.get(f"/v1/jobs/{job_id}")
        self._handle_error(response)
        return Job.from_dict(response.json())

    def cancel(self, job_id: str) -> None:
        """Request job cancellation.

        Waiting-state jobs are cancelled immediately.
        Running jobs enter cancelling state.

        Args:
            job_id: Job UUID.

        Raises:
            NotFoundError: If job does not exist.
            AlreadyTerminalError: If job is already in a terminal state.
        """
        response = self._client.post(f"/v1/jobs/{job_id}:cancel")
        self._handle_error(response)

    def retry(self, job_id: str) -> SubmitResponse:
        """Manually retry a dead or cancelled job.

        Creates a clone of the original job with retry_of_job_id set.

        Args:
            job_id: Job UUID (must be dead or cancelled).

        Returns:
            SubmitResponse for the new clone job.

        Raises:
            NotFoundError: If job does not exist.
            AlreadyTerminalError: If job is succeeded (cannot retry).
        """
        response = self._client.post(f"/v1/jobs/{job_id}:retry")
        self._handle_error(response)

        data = response.json()
        return SubmitResponse(
            job_id=data["job_id"],
            state=data["state"],
            deduplicated=data.get("deduplicated", False),
        )

    def close(self) -> None:
        """Close the underlying HTTP client."""
        self._client.close()

    def __enter__(self) -> JobForgeClient:
        return self

    def __exit__(self, *args: object) -> None:
        self.close()
