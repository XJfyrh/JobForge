"""JobForge SDK client.

Provides a synchronous HTTP client for the JobForge API.
"""

from __future__ import annotations

from datetime import datetime
from typing import Any

import httpx
from opentelemetry import trace
from opentelemetry.trace import StatusCode
from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator

from jobforge.errors import (
    InternalError,
    RequestTimeoutError,
    TransportError,
    from_response,
)
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
        >>> payload = {"version": 1, "business_key": "handbook-v1",
        ...            "corpus_version": "handbook-v1"}
        >>> job = client.submit("default", "rag.index", payload)
        >>> print(job.job_id, job.state)
    """

    def __init__(
        self,
        base_url: str,
        api_key: str,
        timeout: float = 30.0,
        trace_id: str | None = None,
        *,
        traceparent: str | None = None,
        transport: httpx.BaseTransport | None = None,
    ) -> None:
        self._base_url = base_url.rstrip("/")
        self._api_key = api_key
        self._trace_id = trace_id
        self._traceparent = traceparent
        self._client = httpx.Client(
            base_url=self._base_url,
            timeout=timeout,
            headers=self._build_headers(),
            transport=transport,
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
        if not 200 <= response.status_code < 300:
            try:
                data = response.json()
            except ValueError:
                data = None
            error = data.get("error") if isinstance(data, dict) else None
            if (
                isinstance(error, dict)
                and isinstance(error.get("code"), str)
                and isinstance(error.get("message"), str)
            ):
                exc = from_response(error["code"], error["message"][:2048])
            else:
                exc = InternalError("invalid server error response")
            exc.status_code = response.status_code
            raise exc

    def _request(
        self, operation: str, method: str, path: str, **kwargs: Any
    ) -> httpx.Response:
        """Perform exactly one HTTP exchange with bounded, content-free tracing."""
        parent = (
            TraceContextTextMapPropagator().extract({"traceparent": self._traceparent})
            if self._traceparent
            else None
        )
        with trace.get_tracer("jobforge.sdk").start_as_current_span(
            f"sdk.{operation}",
            context=parent,
            record_exception=False,
            set_status_on_exception=False,
        ) as span:
            headers: dict[str, str] = {}
            TraceContextTextMapPropagator().inject(headers)
            try:
                response = self._client.request(method, path, headers=headers, **kwargs)
            except httpx.TimeoutException as exc:
                span.set_status(StatusCode.ERROR, "request timeout")
                raise RequestTimeoutError() from exc
            except httpx.RequestError as exc:
                span.set_status(StatusCode.ERROR, "transport failure")
                raise TransportError() from exc
            span.set_attribute("http.response.status_code", response.status_code)
            if not 200 <= response.status_code < 300:
                span.set_status(StatusCode.ERROR, "server rejected request")
            self._handle_error(response)
            return response

    @staticmethod
    def _json(response: httpx.Response) -> dict[str, Any]:
        """Reject malformed success envelopes without echoing response content."""
        try:
            data = response.json()
        except ValueError as exc:
            raise InternalError("invalid server success response") from exc
        if not isinstance(data, dict):
            raise InternalError("invalid server success response")
        return data

    @classmethod
    def _submit_response(cls, response: httpx.Response) -> SubmitResponse:
        data = cls._json(response)
        if not isinstance(data.get("job_id"), str) or not isinstance(
            data.get("state"), str
        ):
            raise InternalError("invalid server success response")
        return SubmitResponse(
            job_id=data["job_id"],
            state=data["state"],
            deduplicated=data.get("deduplicated", False),
        )

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

        response = self._request("submit", "POST", "/v1/jobs", json=body)
        return self._submit_response(response)

    def get(self, job_id: str) -> Job:
        """Get job details by ID.

        Args:
            job_id: Job UUID.

        Returns:
            Job object with full details.

        Raises:
            NotFoundError: If job does not exist or belongs to another tenant.
        """
        response = self._request("get", "GET", f"/v1/jobs/{job_id}")
        try:
            return Job.from_dict(self._json(response))
        except (KeyError, TypeError, ValueError, AttributeError) as exc:
            raise InternalError("invalid server job response") from exc

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
        self._request("cancel", "POST", f"/v1/jobs/{job_id}:cancel")

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
        response = self._request("retry", "POST", f"/v1/jobs/{job_id}:retry")
        return self._submit_response(response)

    def close(self) -> None:
        """Close the underlying HTTP client."""
        self._client.close()

    def __enter__(self) -> JobForgeClient:
        return self

    def __exit__(self, *args: object) -> None:
        self.close()
