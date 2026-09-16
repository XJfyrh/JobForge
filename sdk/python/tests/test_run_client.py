"""Deterministic SDK checks; real service coverage is run_http_contract.py."""

from __future__ import annotations

import copy
import json
from collections.abc import Callable
from pathlib import Path
from typing import Any

import httpx
import pytest

from jobforge import (
    DependencyUnavailableError,
    InternalError,
    InvalidArgumentError,
    JobForgeError,
    RequestTimeoutError,
    RunClient,
    RunState,
    TransportError,
)
from jobforge.run_models import MAX_SAFE_INTEGER, Run

FIXTURES = json.loads(
    (Path(__file__).resolve().parents[3] / "api/run/v2/fixtures.json").read_text(
        encoding="utf-8"
    )
)
RUN_ID: str = FIXTURES["run"]["run_id"]


def invoke_submit(client: RunClient, **kwargs: Any) -> Any:
    """Submit shared fixture values, allowing one invalid override per test."""
    values = {
        key: value
        for key, value in FIXTURES["submit_explicit_default"].items()
        if key != "schema_version"
    }
    values["idempotency_key"] = "submit-key"
    values.update(kwargs)
    return client.submit(**values)


@pytest.mark.parametrize(
    ("call", "path", "method", "fixture"),
    [
        (invoke_submit, "/v2/runs", "POST", "submission"),
        (lambda c: c.get(RUN_ID), f"/v2/runs/{RUN_ID}", "GET", "run"),
        (lambda c: c.list(), "/v2/runs", "GET", "page"),
        (lambda c: c.steps(RUN_ID), f"/v2/runs/{RUN_ID}/steps", "GET", "steps"),
        (lambda c: c.events(RUN_ID), f"/v2/runs/{RUN_ID}/events", "GET", "events"),
        (lambda c: c.result(RUN_ID), f"/v2/runs/{RUN_ID}/result", "GET", "result"),
        (
            lambda c: c.cancel(RUN_ID, idempotency_key="cancel-key"),
            f"/v2/runs/{RUN_ID}/cancel",
            "POST",
            "cancellation",
        ),
        (
            lambda c: c.retry(RUN_ID, idempotency_key="retry-key"),
            f"/v2/runs/{RUN_ID}/retry",
            "POST",
            "submission",
        ),
    ],
)
def test_one_exchange_per_operation(
    call: Callable[[RunClient], Any], path: str, method: str, fixture: str
) -> None:
    """Public operations expose the shared source-contract response models."""
    requests: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        return httpx.Response(200, json=FIXTURES[fixture])

    with RunClient(
        "https://unit.invalid", "test-only", transport=httpx.MockTransport(handler)
    ) as client:
        assert call(client) is not None
    assert len(requests) == 1
    request = requests[0]
    assert request.method == method and request.url.path == path
    if method == "POST":
        assert request.headers["Idempotency-Key"]
        body = json.loads(request.content)
        assert body["schema_version"] == 1
        assert "tenant_id" not in body
        if path == "/v2/runs":
            assert body == FIXTURES["submit_explicit_default"]


def test_page_parameters_and_no_automatic_next_page() -> None:
    """Opaque cursor and explicit after/limit are sent once without iteration."""
    requests: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        if request.url.path.endswith("events"):
            return httpx.Response(200, json={"items": [], "next_after": 900})
        return httpx.Response(200, json={"items": [], "next_cursor": "opaque-v1"})

    with RunClient(
        "https://unit.invalid", "test-only", transport=httpx.MockTransport(handler)
    ) as client:
        assert (
            client.list(state=RunState.FAILED, cursor="opaque", limit=7).next_cursor
            == "opaque-v1"
        )
        assert client.events(RUN_ID, after=33, limit=5).next_after == 900
    assert len(requests) == 2
    assert dict(requests[0].url.params) == {
        "state": "failed",
        "cursor": "opaque",
        "limit": "7",
    }
    assert dict(requests[1].url.params) == {"after": "33", "limit": "5"}


@pytest.mark.parametrize("entry", FIXTURES["errors"])
def test_stable_nested_errors_without_retry(entry: dict[str, Any]) -> None:
    """Error code, never English message or status alone, chooses the exception."""
    requests: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        return httpx.Response(entry["status"], json=entry["body"])

    with RunClient(
        "https://unit.invalid", "test-only", transport=httpx.MockTransport(handler)
    ) as client:
        with pytest.raises(JobForgeError) as captured:
            invoke_submit(client)
    assert len(requests) == 1
    assert captured.value.code == entry["body"]["error"]["code"]
    assert captured.value.status_code == entry["status"]
    assert type(captured.value) is not JobForgeError


@pytest.mark.parametrize("status", [301, 302, 307, 308, 503])
def test_redirect_and_dependency_error_never_resend(status: int) -> None:
    """A transport redirect must not send the operation to another location."""
    count = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal count
        count += 1
        return httpx.Response(
            status,
            headers={"Location": "https://other.invalid"},
            json={
                "error": {"code": "DEPENDENCY_UNAVAILABLE", "message": "unavailable"}
            },
        )

    with RunClient(
        "https://unit.invalid", "test-only", transport=httpx.MockTransport(handler)
    ) as client:
        with pytest.raises(DependencyUnavailableError):
            invoke_submit(client)
    assert count == 1


@pytest.mark.parametrize("timeout", [False, True])
def test_transport_failure_is_once_and_has_no_body_in_public_error(
    timeout: bool,
) -> None:
    """Network uncertainty is returned to the caller without replaying a key."""
    count = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal count
        count += 1
        error = httpx.ReadTimeout if timeout else httpx.ConnectError
        raise error("sensitive transport detail", request=request)

    with RunClient(
        "https://unit.invalid", "test-only", transport=httpx.MockTransport(handler)
    ) as client:
        with pytest.raises(
            RequestTimeoutError if timeout else TransportError
        ) as captured:
            invoke_submit(client)
    assert count == 1 and "sensitive" not in str(captured.value)


@pytest.mark.parametrize(
    "call",
    [
        lambda c: invoke_submit(c, run_timeout_seconds=True),
        lambda c: invoke_submit(c, run_timeout_seconds=1.0),
        lambda c: invoke_submit(c, run_timeout_seconds=0),
        lambda c: invoke_submit(c, run_timeout_seconds=86401),
        lambda c: invoke_submit(c, ticket_id=" bad"),
        lambda c: invoke_submit(c, business_request_key="中文"),
        lambda c: c.cancel(RUN_ID, idempotency_key="key\n"),
        lambda c: c.cancel(RUN_ID, idempotency_key=""),
        lambda c: invoke_submit(c, idempotency_key=None),
        lambda c: c.get("../other-tenant"),
        lambda c: c.list(state="dead"),
        lambda c: c.list(limit=True),
        lambda c: c.list(limit=101),
        lambda c: c.list(cursor=""),
        lambda c: c.events(RUN_ID, after=-1),
        lambda c: c.events(RUN_ID, after=MAX_SAFE_INTEGER + 1),
    ],
)
def test_invalid_public_input_sends_nothing(call: Callable[[RunClient], Any]) -> None:
    """The thin SDK rejects malformed syntax without deciding service policy."""

    def handler(request: httpx.Request) -> httpx.Response:
        raise AssertionError("invalid input must not be sent")

    with RunClient(
        "https://unit.invalid", "test-only", transport=httpx.MockTransport(handler)
    ) as client:
        with pytest.raises(InvalidArgumentError):
            call(client)


@pytest.mark.parametrize(
    "body",
    [
        b'{"available":false,"available":true,"kind":null,"ref":null}',
        b'{"available":false,"kind":null,"ref":null,"unexpected":1}',
        b'{"available":false,"kind":null}',
        b'{"available":false,"kind":null,"ref":null} {}',
        b'{"available":0,"kind":null,"ref":null}',
        b'{"available":true,"kind":"proposal","ref":null}',
        b'{"available":false,"kind":null,"ref":"sensitive"}',
        b'{"available":false,"kind":null,"ref":NaN}',
        b'{"available":false,"kind":null,"ref":"\xff"}',
        b'["sensitive"]',
    ],
)
def test_malformed_success_is_rejected_without_content(body: bytes) -> None:
    """Unknown, missing, duplicate, non-JSON and invalid UTF-8 fail closed."""
    transport = httpx.MockTransport(lambda request: httpx.Response(200, content=body))
    with RunClient("https://unit.invalid", "test-only", transport=transport) as client:
        with pytest.raises(InternalError) as captured:
            client.result(RUN_ID)
    assert "sensitive" not in str(captured.value)
    assert captured.value.__suppress_context__


@pytest.mark.parametrize("value", [True, 1.0, -1, MAX_SAFE_INTEGER + 1, "1", None])
def test_response_identity_and_amounts_are_safe_integers(value: Any) -> None:
    """Financial exposure never accepts bool, float, strings or overflow."""
    for path in (
        ("attempt_no",),
        ("budget", "family", "used", "cost_microyuan"),
        ("budget", "tenant", "known_tokens"),
        ("version_vector", "ticket", "revision"),
    ):
        body = copy.deepcopy(FIXTURES["run"])
        target = body
        for key in path[:-1]:
            target = target[key]
        target[path[-1]] = value
        with pytest.raises((ValueError, TypeError)):
            Run.from_dict(body)


def test_execution_failure_and_held_exposure_remain_visible_data() -> None:
    """A failed Run is a successful query; unknown holds never disappear."""
    body = copy.deepcopy(FIXTURES["run"])
    body["state"] = "failed"
    body["error"] = {"code": "BUDGET_EXHAUSTED", "message": "budget exhausted"}
    family = body["budget"]["family"]
    family.update(
        known_tokens=3, held_tokens=7, known_cost_microyuan=2, held_cost_microyuan=9
    )
    family["used"].update(tokens=10, cost_microyuan=11)
    transport = httpx.MockTransport(lambda request: httpx.Response(200, json=body))
    with RunClient("https://unit.invalid", "test-only", transport=transport) as client:
        run = client.get(RUN_ID)
    assert run.state == RunState.FAILED and run.state.is_terminal
    assert run.error and run.error.code == "BUDGET_EXHAUSTED"
    assert run.budget.family.held_tokens == 7


@pytest.mark.parametrize(
    "timestamp",
    ["2026-09-16", "2026-09-16T08:00:00", "2026-09-16 08:00:00Z", "private"],
)
def test_timestamps_require_rfc3339_timezone(timestamp: str) -> None:
    """Response dates cannot silently lose timezone or accept private content."""
    body = copy.deepcopy(FIXTURES["run"])
    body["created_at"] = timestamp
    with pytest.raises(ValueError):
        Run.from_dict(body)


def test_invalid_nested_bindings_are_rejected() -> None:
    """Missing relationships and account exposure must remain internally sound."""
    body = copy.deepcopy(FIXTURES["run"])
    body["budget"]["family"]["held_tokens"] = 1
    with pytest.raises(ValueError):
        Run.from_dict(body)
    body = copy.deepcopy(FIXTURES["run"])
    body["version_vector"]["order"]["id"] = "expected-order"
    body["version_vector"]["delivery"]["id"] = "unauthorized-delivery"
    with pytest.raises(ValueError):
        Run.from_dict(body)


def test_nonfinite_protected_output_is_rejected() -> None:
    """Even untyped protected JSON output cannot carry non-finite numbers."""
    body = json.dumps(FIXTURES["steps"]).replace(
        '"output": {', '"output": {"invalid": 1e999,'
    )
    transport = httpx.MockTransport(lambda request: httpx.Response(200, content=body))
    with RunClient("https://unit.invalid", "test-only", transport=transport) as client:
        with pytest.raises(InternalError):
            client.steps(RUN_ID)


def test_trace_context_is_forwarded_without_payload_attributes() -> None:
    """Explicit trace parent/state flow through a content-free SDK span."""
    requests: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        return httpx.Response(200, json=FIXTURES["run"])

    with RunClient(
        "https://unit.invalid",
        "test-only",
        traceparent="00-11111111111111111111111111111111-2222222222222222-01",
        tracestate="vendor=value",
        transport=httpx.MockTransport(handler),
    ) as client:
        client.get(RUN_ID)
    assert "11111111111111111111111111111111" in requests[0].headers["traceparent"]
    assert requests[0].headers["tracestate"] == "vendor=value"
