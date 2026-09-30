"""Public call evidence uses Go-generated fixtures; no real provider calls."""

from __future__ import annotations

import copy
import json
from pathlib import Path
from typing import Any

import httpx
import pytest
import yaml  # type: ignore[import-untyped]
from jsonschema import Draft202012Validator  # type: ignore[import-untyped]

from jobforge import InternalError, InvalidArgumentError, NotFoundError, RunClient
from jobforge.run_calls import MAX_CALL_RESPONSE_BYTES, RunCalls

ROOT = Path(__file__).resolve().parents[3]
FIXTURES: dict[str, Any] = json.loads(
    (ROOT / "api/run/v2/calls-fixtures.json").read_text(encoding="utf-8")
)


@pytest.mark.parametrize("name", list(FIXTURES))
def test_go_call_view_parses_and_makes_one_query(name: str) -> None:
    value = FIXTURES[name]
    requests = []

    def serve(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        assert request.method == "GET"
        assert request.url.path == f"/v2/runs/{value['run_id']}/calls"
        assert not request.url.query
        assert not request.content
        assert request.headers["Authorization"] == "Bearer synthetic-reader"
        return httpx.Response(200, json=value)

    with RunClient(
        "http://synthetic.invalid",
        "synthetic-reader",
        transport=httpx.MockTransport(serve),
    ) as client:
        result = client.calls(value["run_id"])
    assert len(requests) == 1
    assert result == RunCalls.from_dict(value)
    if name == "incompatible":
        assert result.items[0].observed_usage is not None
        assert result.items[0].settled_usage is None
        assert not result.items[0].usage_known
        assert result.items[0].known_cost_microyuan == 0
        assert result.items[0].held_cost_microyuan == 1000
    if name == "unavailable":
        audit = result.items[0].provider_audit
        assert audit is not None and audit.reasoning_tokens is None
    if name == "recorded":
        audit = result.items[0].provider_audit
        assert audit is not None and audit.reasoning_tokens == 0


def test_public_source_schema_accepts_all_go_fixtures() -> None:
    source = yaml.safe_load(
        (ROOT / "api/run/v2/openapi.yaml").read_text(encoding="utf-8")
    )
    validator = Draft202012Validator(
        {
            "$schema": "https://json-schema.org/draft/2020-12/schema",
            "$ref": "#/components/schemas/RunCalls",
            "components": source["components"],
        }
    )
    for value in FIXTURES.values():
        validator.validate(value)


@pytest.mark.parametrize(
    ("field", "invalid"),
    [
        ("ordinal", True),
        ("ordinal", 45),
        ("attempt_no", "1"),
        ("known_tokens", -1),
        ("held_tokens", (1 << 53)),
        ("report_hash", "secret body is not a hash"),
        ("audit_status", "assumed_not_sent"),
        ("usage_known", False),
        ("http_status", 700),
        ("physical_call_id", "invalid"),
        ("transport_outcome", "paid"),
    ],
)
def test_invalid_call_facts_fail_closed(field: str, invalid: Any) -> None:
    value = copy.deepcopy(FIXTURES["recorded"])
    value["items"][0][field] = invalid
    with pytest.raises((ValueError, TypeError)):
        RunCalls.from_dict(value)


@pytest.mark.parametrize("mutation", ["missing", "extra", "null", "unknown-priced"])
def test_call_fields_are_complete_and_observed_usage_is_not_known(
    mutation: str,
) -> None:
    value = copy.deepcopy(FIXTURES["incompatible"])
    item = value["items"][0]
    if mutation == "missing":
        del item["settled_usage"]
    elif mutation == "extra":
        item["worker_control_token"] = "must-not-appear"
    elif mutation == "null":
        item["usage_known"] = None
    else:
        item["known_cost_microyuan"] = 1
    with pytest.raises((ValueError, TypeError)):
        RunCalls.from_dict(value)


def test_call_rows_have_bounded_unique_ascending_positions() -> None:
    value = copy.deepcopy(FIXTURES["recorded"])
    value["items"] *= 45
    with pytest.raises(ValueError):
        RunCalls.from_dict(value)
    value["items"] = value["items"][:2]
    with pytest.raises(ValueError):
        RunCalls.from_dict(value)


def test_calls_invalid_id_is_rejected_before_http() -> None:
    def unexpected(request: httpx.Request) -> httpx.Response:
        raise AssertionError("invalid id must not cause a request")

    with RunClient(
        "http://synthetic.invalid",
        "synthetic-reader",
        transport=httpx.MockTransport(unexpected),
    ) as client:
        with pytest.raises(InvalidArgumentError):
            client.calls("invalid")


@pytest.mark.parametrize("variant", ["oversize", "duplicate", "foreign"])
def test_calls_error_size_and_duplicate_handling(variant: str) -> None:
    count = 0

    def serve(request: httpx.Request) -> httpx.Response:
        nonlocal count
        count += 1
        if variant == "foreign":
            return httpx.Response(
                404, json={"error": {"code": "NOT_FOUND", "message": "not found"}}
            )
        if variant == "oversize":
            return httpx.Response(200, content=b" " * (MAX_CALL_RESPONSE_BYTES + 1))
        return httpx.Response(200, content=b'{"items":[],"items":[]}')

    with RunClient(
        "http://synthetic.invalid",
        "synthetic-reader",
        transport=httpx.MockTransport(serve),
    ) as client:
        expected = NotFoundError if variant == "foreign" else InternalError
        with pytest.raises(expected):
            client.calls(FIXTURES["empty"]["run_id"])
    assert count == 1


@pytest.mark.parametrize("status", [200, 503])
def test_calls_stops_reading_oversized_stream_and_closes(status: int) -> None:
    """The limit must stop the download, not inspect an already buffered body."""

    class EndlessBody(httpx.SyncByteStream):
        reads = 0
        closed = False

        def __iter__(self):
            while self.reads < 1000:
                self.reads += 1
                yield b" " * 8192
            pytest.fail("the SDK consumed the oversized response")

        def close(self) -> None:
            self.closed = True

    stream = EndlessBody()
    requests = []

    def serve(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        return httpx.Response(status, stream=stream)

    with RunClient(
        "http://synthetic.invalid",
        "synthetic-reader",
        transport=httpx.MockTransport(serve),
    ) as client:
        with pytest.raises(InternalError, match="invalid run server response"):
            client.calls(FIXTURES["empty"]["run_id"])
    assert stream.closed
    assert stream.reads == MAX_CALL_RESPONSE_BYTES // 8192 + 1
    assert len(requests) == 1


def test_calls_accepts_exact_decoded_limit() -> None:
    value = FIXTURES["empty"]
    raw = json.dumps(value).encode()
    raw += b" " * (MAX_CALL_RESPONSE_BYTES - len(raw))
    with RunClient(
        "http://synthetic.invalid",
        "synthetic-reader",
        transport=httpx.MockTransport(lambda _: httpx.Response(200, content=raw)),
    ) as client:
        assert client.calls(value["run_id"]) == RunCalls.from_dict(value)


def test_calls_closes_midstream_timeout_without_retry() -> None:
    class InterruptedBody(httpx.SyncByteStream):
        closed = False

        def __iter__(self):
            yield b"{"
            raise httpx.ReadTimeout("synthetic interrupted read")

        def close(self) -> None:
            self.closed = True

    from jobforge import RequestTimeoutError

    stream = InterruptedBody()
    requests = []

    def serve(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        return httpx.Response(200, stream=stream)

    with RunClient(
        "http://synthetic.invalid",
        "synthetic-reader",
        transport=httpx.MockTransport(serve),
    ) as client:
        with pytest.raises(RequestTimeoutError):
            client.calls(FIXTURES["empty"]["run_id"])
    assert stream.closed
    assert len(requests) == 1
