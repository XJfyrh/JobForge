"""Error envelopes are identical to real ADR-0002 HTTP responses."""

from __future__ import annotations

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
            client.get("id")
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
            client.get("id")
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
            client.get("id")
        with pytest.raises(InternalError):
            client.submit("default", "demo.echo", {})


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
