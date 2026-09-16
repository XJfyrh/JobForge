"""Synchronous, single-exchange client for the public Run v2 source contract."""

from __future__ import annotations

import json
import math
import re
from typing import Any, TypeVar
from uuid import UUID

import httpx
from opentelemetry import trace
from opentelemetry.trace import StatusCode
from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator

from jobforge.errors import (
    InternalError,
    InvalidArgumentError,
    JobForgeError,
    RequestTimeoutError,
    TransportError,
    from_response,
)
from jobforge.run_calls import MAX_CALL_RESPONSE_BYTES, RunCalls
from jobforge.run_models import (
    MAX_SAFE_INTEGER,
    Run,
    RunCancellation,
    RunEventPage,
    RunModel,
    RunPage,
    RunResult,
    RunState,
    RunStepPage,
    RunSubmission,
)

_T = TypeVar("_T", bound=RunModel)
_KEY = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:-]{0,127}\Z")


class RunClient:
    """Thin authenticated Run API wrapper with no retries or background work.

    Each method makes at most one HTTP request, including failures and redirects.
    A timeout can leave acceptance unknown: callers retain their operation key.
    Budget provisioning, Worker RPCs and approval/write actions are not exposed.

    Args:
        base_url: Root URL of the Run control service.
        api_key: Public reader/operator credential; never recorded in tracing.
        timeout: Per-exchange timeout in seconds.
        traceparent: Optional explicit W3C parent; otherwise uses current context.
        tracestate: Optional W3C state accompanying the explicit parent.
        transport: Optional test/custom transport; callers must disable its retries.
    """

    def __init__(
        self,
        base_url: str,
        api_key: str,
        timeout: float = 30.0,
        *,
        traceparent: str | None = None,
        tracestate: str | None = None,
        transport: httpx.BaseTransport | None = None,
    ) -> None:
        self._parent = (
            {"traceparent": traceparent, "tracestate": tracestate or ""}
            if traceparent
            else None
        )
        self._client = httpx.Client(
            base_url=base_url.rstrip("/"),
            timeout=timeout,
            follow_redirects=False,
            headers={
                "Authorization": f"Bearer {api_key}",
                "Content-Type": "application/json",
                "Accept": "application/json",
            },
            transport=transport,
        )

    def submit(
        self,
        ticket_id: str,
        business_request_key: str,
        profile_id: str,
        budget_batch_id: str,
        *,
        idempotency_key: str,
        run_timeout_seconds: int = 3600,
    ) -> RunSubmission:
        """Submit one stable business intent; the server owns deduplication.

        Reuse the key and exact content when acceptance is unknown. A new submit
        key with unchanged business intent still returns the first root Run.
        """
        _key(idempotency_key)
        for value in (ticket_id, business_request_key, profile_id, budget_batch_id):
            _key(value)
        _integer(run_timeout_seconds, 1, 86400)
        return self._request(
            RunSubmission,
            "submit",
            "POST",
            "/v2/runs",
            key=idempotency_key,
            body={
                "schema_version": 1,
                "ticket_id": ticket_id,
                "business_request_key": business_request_key,
                "profile_id": profile_id,
                "budget_batch_id": budget_batch_id,
                "run_timeout_seconds": run_timeout_seconds,
            },
        )

    def get(self, run_id: str) -> Run:
        """Fetch one tenant-authorized Run; an execution failure remains data."""
        return self._request(Run, "get", "GET", _path(run_id))

    def list(
        self,
        *,
        state: RunState | str | None = None,
        cursor: str | None = None,
        limit: int = 20,
    ) -> RunPage:
        """Fetch one descending page; the cursor binds tenant and state filter."""
        _integer(limit, 1, 100)
        params: dict[str, str | int] = {"limit": limit}
        if state is not None:
            try:
                params["state"] = RunState(state).value
            except (TypeError, ValueError):
                raise InvalidArgumentError("invalid run state") from None
        if cursor is not None:
            if not isinstance(cursor, str) or not cursor or len(cursor) > 4096:
                raise InvalidArgumentError("invalid cursor")
            params["cursor"] = cursor
        return self._request(RunPage, "list", "GET", "/v2/runs", params=params)

    def steps(self, run_id: str, *, after: int = 0, limit: int = 20) -> RunStepPage:
        """Read one page of protected checkpoint content explicitly."""
        return self._request(
            RunStepPage,
            "steps",
            "GET",
            _path(run_id) + "/steps",
            params=_page(after, limit),
        )

    def events(self, run_id: str, *, after: int = 0, limit: int = 20) -> RunEventPage:
        """Read one ascending metadata-only event page without polling."""
        return self._request(
            RunEventPage,
            "events",
            "GET",
            _path(run_id) + "/events",
            params=_page(after, limit),
        )

    def result(self, run_id: str) -> RunResult:
        """Read result availability; this does not dereference protected content."""
        return self._request(RunResult, "result", "GET", _path(run_id) + "/result")

    def calls(self, run_id: str) -> RunCalls:
        """Capture this Run's call audit once; observed usage is not known cost.

        There is no pagination, implicit polling or audit mutation. Missing
        reports do not prove that a provider request was never sent.
        """
        return self._request(RunCalls, "calls", "GET", _path(run_id) + "/calls")

    def cancel(self, run_id: str, *, idempotency_key: str) -> RunCancellation:
        """Accept cancellation; running execution first enters stopping.

        Cancellation prevents later authorization, but cannot retract a request
        that was already authorized and may be computing remotely.
        """
        _key(idempotency_key)
        return self._request(
            RunCancellation,
            "cancel",
            "POST",
            _path(run_id) + "/cancel",
            key=idempotency_key,
            body={"schema_version": 1},
        )

    def retry(
        self,
        run_id: str,
        *,
        idempotency_key: str,
        run_timeout_seconds: int = 3600,
    ) -> RunSubmission:
        """Create/reuse the sole successor while retaining the shared budget.

        Only failed/cancelled sources within the original seven-day business
        window can create successors. Already accepted operations remain readable.
        """
        _key(idempotency_key)
        _integer(run_timeout_seconds, 1, 86400)
        return self._request(
            RunSubmission,
            "retry",
            "POST",
            _path(run_id) + "/retry",
            key=idempotency_key,
            body={"schema_version": 1, "run_timeout_seconds": run_timeout_seconds},
        )

    def _request(
        self,
        model: type[_T],
        operation: str,
        method: str,
        path: str,
        *,
        key: str | None = None,
        body: dict[str, Any] | None = None,
        params: dict[str, str | int] | None = None,
    ) -> _T:
        headers: dict[str, str] = {}
        if key is not None:
            _key(key)
            headers["Idempotency-Key"] = key
        content = None
        if body is not None:
            content = json.dumps(
                body, ensure_ascii=False, separators=(",", ":")
            ).encode("utf-8")
            if len(content) > 4096:
                raise InvalidArgumentError("request exceeds 4096 bytes")
        propagator = TraceContextTextMapPropagator()
        parent = propagator.extract(self._parent) if self._parent else None
        with trace.get_tracer("jobforge.sdk").start_as_current_span(
            f"sdk.run.{operation}",
            context=parent,
            record_exception=False,
            set_status_on_exception=False,
        ) as span:
            propagator.inject(headers)
            try:
                response = self._client.request(
                    method, path, content=content, params=params, headers=headers
                )
            except httpx.TimeoutException as exc:
                span.set_status(StatusCode.ERROR, "request timeout")
                raise RequestTimeoutError() from exc
            except httpx.RequestError as exc:
                span.set_status(StatusCode.ERROR, "transport failure")
                raise TransportError() from exc
            span.set_attribute("http.response.status_code", response.status_code)
            success = 200 <= response.status_code < 300
            if not success:
                span.set_status(StatusCode.ERROR, "server rejected request")
            try:
                if (
                    operation == "calls"
                    and len(response.content) > MAX_CALL_RESPONSE_BYTES
                ):
                    raise ValueError("oversized call evidence")
                data = json.loads(
                    response.content.decode("utf-8"),
                    object_pairs_hook=_unique_object,
                    parse_constant=_invalid_constant,
                    parse_float=_finite_float,
                )
                if not success:
                    return self._raise_error(response.status_code, data)
                return model.from_dict(data)
            except (ValueError, TypeError, KeyError, OverflowError, RecursionError):
                raise InternalError("invalid run server response") from None

    @staticmethod
    def _raise_error(status: int, data: Any) -> Any:
        exc: JobForgeError
        if (
            not isinstance(data, dict)
            or set(data) != {"error"}
            or not isinstance(data["error"], dict)
            or set(data["error"]) != {"code", "message"}
            or not isinstance(data["error"]["code"], str)
            or not isinstance(data["error"]["message"], str)
        ):
            exc = InternalError("invalid server error response")
        else:
            error = data["error"]
            exc = from_response(error["code"], error["message"][:2048])
        exc.status_code = status
        raise exc

    def close(self) -> None:
        """Close the owned HTTP connection pool."""
        self._client.close()

    def __enter__(self) -> RunClient:
        return self

    def __exit__(self, *args: object) -> None:
        self.close()


def _key(value: str) -> None:
    if not isinstance(value, str) or _KEY.fullmatch(value) is None:
        raise InvalidArgumentError("invalid identifier or idempotency key")


def _integer(value: int, low: int, high: int) -> None:
    if type(value) is not int or not low <= value <= high:
        raise InvalidArgumentError("integer outside allowed range")


def _path(run_id: str) -> str:
    try:
        if not isinstance(run_id, str) or str(UUID(run_id)) != run_id:
            raise ValueError("invalid UUID")
    except ValueError:
        raise InvalidArgumentError("invalid run UUID") from None
    return f"/v2/runs/{run_id}"


def _page(after: int, limit: int) -> dict[str, str | int]:
    _integer(after, 0, MAX_SAFE_INTEGER)
    _integer(limit, 1, 100)
    return {"after": after, "limit": limit}


def _unique_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate server response field")
        result[key] = value
    return result


def _invalid_constant(value: str) -> None:
    raise ValueError("non-finite JSON number")


def _finite_float(value: str) -> float:
    result = float(value)
    if not math.isfinite(result):
        raise ValueError("non-finite JSON number")
    return result
