"""Error envelopes are identical to real ADR-0002 HTTP responses."""

from __future__ import annotations

import traceback

import httpx
import pytest

from jobforge import (
    AlreadyTerminalError,
    CancelRequestedError,
    ConflictError,
    ForbiddenError,
    InternalError,
    InvalidArgumentError,
    InvalidTransitionError,
    JobForgeClient,
    JobForgeError,
    NotFoundError,
    QueueOverloadedError,
    RequestTimeoutError,
    StaleLeaseError,
    TransportError,
    UnauthorizedError,
)


@pytest.mark.parametrize(
    "code,status,exception,retryable",
    [
        ("INVALID_ARGUMENT", 400, InvalidArgumentError, False),
        ("UNAUTHORIZED", 401, UnauthorizedError, False),
        ("FORBIDDEN", 403, ForbiddenError, False),
        ("NOT_FOUND", 404, NotFoundError, False),
        ("CONFLICT", 409, ConflictError, False),
        ("ALREADY_TERMINAL", 409, AlreadyTerminalError, False),
        ("STALE_LEASE", 409, StaleLeaseError, False),
        ("CANCEL_REQUESTED", 409, CancelRequestedError, False),
        ("INVALID_TRANSITION", 409, InvalidTransitionError, False),
        ("QUEUE_OVERLOADED", 429, QueueOverloadedError, True),
        ("INTERNAL", 500, InternalError, True),
    ],
)
def test_error_contract(
    code: str, status: int, exception: type[JobForgeError], retryable: bool
) -> None:
    """Status alone must not conflate terminal, cancel and conflict errors."""
    transport = httpx.MockTransport(
        lambda _: httpx.Response(
            status, json={"error": {"code": code, "message": "safe message"}}
        )
    )
    with JobForgeClient(
        "http://testserver", "synthetic", transport=transport
    ) as client:
        with pytest.raises(exception) as captured:
            client.get("11111111-1111-4111-8111-111111111111")
    assert captured.value.code == code
    assert captured.value.status_code == status
    assert captured.value.message == "safe message"
    assert captured.value.retryable is retryable


@pytest.mark.parametrize(
    "body",
    [
        b"upstream secret response",
        b"null",
        b"[]",
        b'{"error":null}',
        b'{"error":{"code":{},"message":[]}}',
        b'{"code":"NOT_FOUND","message":"obsolete fixture"}',
        b'{"error":{"code":"FUTURE_CODE","message":"upstream secret"}}',
    ],
)
def test_unexpected_response_is_safe_internal_error(body: bytes) -> None:
    """Unexpected envelopes never leak response bodies or parsing exceptions."""
    with JobForgeClient(
        "http://testserver",
        "synthetic",
        transport=httpx.MockTransport(lambda _: httpx.Response(502, content=body)),
    ) as client:
        with pytest.raises(InternalError) as captured:
            client.get("11111111-1111-4111-8111-111111111111")
    assert "secret" not in str(captured.value)
    assert captured.value.retryable


@pytest.mark.parametrize("body", [b"garbage", b"null", b"[]", b"{}", b'{"state":42}'])
def test_malformed_success_is_internal_error(body: bytes) -> None:
    """A broken successful response must not escape as KeyError/ValueError."""
    with JobForgeClient(
        "http://testserver",
        "synthetic",
        transport=httpx.MockTransport(lambda _: httpx.Response(200, content=body)),
    ) as client:
        with pytest.raises(InternalError):
            client.get("11111111-1111-4111-8111-111111111111")
        with pytest.raises(InternalError):
            client.submit("default", "demo.echo", {})


@pytest.mark.parametrize(
    "operation,body",
    [
        ("get", {"id": 42, "state": "ready"}),
        ("get", {"id": "job", "result_ref": {"private": "marker"}}),
        ("get", {"id": "job", "result_ref": False}),
        ("get", {"id": "job", "attempt": "1"}),
        ("get", {"id": "job", "fencing_token": True}),
        ("get", {"id": "job", "created_at": "invalid-date"}),
        ("get", {"id": "job", "created_at": "private-marker"}),
        ("get", {"id": "job", "state": "private-marker"}),
        ("get", {"id": "job", "attempts": {}}),
        ("get", {"id": "job", "attempts": [{"attempt_no": "1"}]}),
        ("submit", {"job_id": "job", "state": "ready", "deduplicated": "false"}),
        ("submit", {"job_id": "job", "state": "unknown"}),
        ("retry", {"job_id": "job", "state": "ready", "deduplicated": 1}),
    ],
)
def test_malformed_success_fields_are_rejected(
    operation: str, body: dict[str, object]
) -> None:
    """Malformed fields must not escape the SDK's declared result types."""
    with JobForgeClient(
        "http://testserver",
        "synthetic",
        transport=httpx.MockTransport(lambda _: httpx.Response(200, json=body)),
    ) as client:
        with pytest.raises(InternalError) as captured:
            if operation == "get":
                client.get("11111111-1111-4111-8111-111111111111")
            elif operation == "retry":
                client.retry("11111111-1111-4111-8111-111111111111")
            else:
                client.submit("default", "demo.echo", {})
        assert "private" not in str(captured.value)
        assert "private-marker" not in "".join(
            traceback.format_exception(captured.value)
        )


@pytest.mark.parametrize(
    "transport_error,expected",
    [(httpx.ReadTimeout, RequestTimeoutError), (httpx.ConnectError, TransportError)],
)
def test_transport_errors_do_not_retry(
    transport_error: type[httpx.RequestError], expected: type[JobForgeError]
) -> None:
    """Ambiguous submissions must remain under caller idempotency control."""
    calls = 0

    def fail(request: httpx.Request) -> httpx.Response:
        nonlocal calls
        calls += 1
        raise transport_error("synthetic", request=request)

    with JobForgeClient(
        "http://testserver", "synthetic", transport=httpx.MockTransport(fail)
    ) as client:
        with pytest.raises(expected):
            client.submit("default", "demo.echo", {})
    assert calls == 1
