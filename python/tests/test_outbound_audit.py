"""Fixed outbound metadata and real loopback send-boundary regressions."""

from __future__ import annotations

import asyncio
import json
from dataclasses import replace
from datetime import datetime
from pathlib import Path
from typing import Any

import httpx
import pytest
from http_fault_server import HTTPFaultServer, ReceivedRequest, respond
from jobforge_agent import outbound_audit
from jobforge_agent.deepseek import prepare_chat_request
from jobforge_agent.dispatch import (
    AuthorizedDispatcher,
    DispatchError,
    Endpoint,
    PreparedRequest,
)
from test_dispatch import (
    Clock,
    Hooks,
    chat_body,
    context,
    good_response,
    make_dispatcher,
    run_async,
    start_frame,
    valid,
)

CALL = "00000000-0000-4000-8000-000000000020"
FIELDS = {
    "schema_version",
    "event",
    "time",
    "physical_call_id",
    "tenant_id",
    "snapshot_id",
    "profile_hash",
    "subcall",
    "method",
    "endpoint_alias",
    "origin",
    "resource_id",
    "model",
    "thinking",
    "max_tokens",
    "http_status",
    "response_complete",
}


def request() -> PreparedRequest:
    """Use the actual fixed request builder with deliberately private content."""
    return prepare_chat_request(
        [{"role": "user", "content": "PRIVATE-CUSTOMER-BODY"}], context=context()
    )


def records(directory: Path) -> list[dict[str, Any]]:
    """Decode only this test's one physical call trace."""
    raw = (directory / (CALL + ".jsonl")).read_bytes()
    assert b"PRIVATE-CUSTOMER-BODY" not in raw
    assert b"dummy-credential" not in raw
    assert b"Authorization" not in raw
    lines = raw.splitlines(keepends=True)
    assert all(len(line) <= 2048 and line.endswith(b"\n") for line in lines)
    values = [json.loads(line) for line in lines]
    for value in values:
        assert set(value) == FIELDS and value["schema_version"] == 1
        assert datetime.fromisoformat(value["time"]).tzinfo is not None
    return values


@pytest.mark.parametrize(
    "subcall,resource",
    [
        ("chat", "deepseek-flash"),
        ("profile_version", "all-minilm:22m"),
        ("profile_tags", "all-minilm:22m"),
        ("query_embedding", "actual-embedding-model"),
        ("get_order", "order-bound"),
        ("get_delivery", "order-bound"),
        ("search_policy", "index-bound"),
    ],
)
def test_metadata_selects_bound_resources_without_copying_payload(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, subcall: str, resource: str
) -> None:
    """Each fixed capability exposes only its designated resource identity."""
    monkeypatch.setattr(outbound_audit, "_DIRECTORY", tmp_path)
    start = start_frame()
    start["checkpoint"] = {
        "snapshot": {
            "ticket_binding_json": {
                "order_id": "order-bound",
                "description": "PRIVATE-CUSTOMER-BODY",
            },
            "index_id": "index-bound",
        }
    }
    prepared = request()
    if subcall != "chat":
        prepared = replace(
            prepared,
            subcall=subcall,
            endpoint="business"
            if subcall in {"get_order", "get_delivery", "search_policy"}
            else "ollama",
            body=b'{"model":"actual-embedding-model","input":"PRIVATE-CUSTOMER-BODY"}'
            if subcall == "query_embedding"
            else b"",
        )
    audit = outbound_audit.begin(prepared, CALL, start, "http://bound.example")
    assert audit is not None
    value = records(tmp_path)[0]
    assert value["resource_id"] == resource
    assert value["tenant_id"] == start["binding"]["tenant_id"]
    assert value["snapshot_id"] == start["binding"]["snapshot_id"]
    assert value["model"] == ("deepseek-flash" if subcall == "chat" else None)
    assert value["thinking"] == ("disabled" if subcall == "chat" else None)
    assert value["max_tokens"] == (1024 if subcall == "chat" else None)


def test_absent_mount_disables_output_and_records_stay_bounded(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """The optional observer never creates a mount or emits an oversized line."""
    missing = tmp_path / "not-mounted"
    monkeypatch.setattr(outbound_audit, "_DIRECTORY", missing)
    assert (
        outbound_audit.begin(request(), CALL, start_frame(), "https://api.deepseek.com")
        is None
    )
    assert not missing.exists()
    monkeypatch.setattr(outbound_audit, "_DIRECTORY", tmp_path)
    outbound_audit.begin(request(), CALL, start_frame(), "x" * 3000)
    assert not (tmp_path / (CALL + ".jsonl")).exists()


@run_async
@pytest.mark.parametrize("failure", ["none", "http_error", "truncated"])
async def test_real_send_boundary_preserves_response_completeness(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, failure: str
) -> None:
    """An attempt exists before TCP handling; headers do not imply a full body."""
    monkeypatch.setattr(outbound_audit, "_DIRECTORY", tmp_path)

    async def handler(
        reader: asyncio.StreamReader,
        writer: asyncio.StreamWriter,
        incoming: ReceivedRequest,
    ) -> None:
        before_response = records(tmp_path)
        assert [r["event"] for r in before_response] == ["dispatch_attempt"]
        assert before_response[0]["http_status"] is None
        assert incoming.body == request().body
        await respond(
            writer,
            status=503 if failure == "http_error" else 200,
            body=chat_body(),
            headers={"Content-Length": "65536"} if failure == "truncated" else None,
        )

    clock = Clock()
    hooks = Hooks(clock)
    async with HTTPFaultServer(handler) as server:
        dispatcher = make_dispatcher(monkeypatch, server, clock, hooks)
        try:
            if failure == "none":
                assert await dispatcher.execute(
                    request(), context=context(), validate=valid
                ) == {"ok": True}
            else:
                with pytest.raises(DispatchError):
                    await dispatcher.execute(
                        request(), context=context(), validate=valid
                    )
            assert len(server.requests) == 1 and len(dispatcher.recorded_usage()) == 1
        finally:
            await dispatcher.aclose()
    observed = records(tmp_path)
    assert [row["event"] for row in observed] == [
        "dispatch_attempt",
        "http_response",
        "finish",
    ]
    assert observed[1]["response_complete"] is False
    assert observed[-1]["response_complete"] == (failure != "truncated")
    assert observed[-1]["http_status"] == (503 if failure == "http_error" else 200)
    assert observed[0]["origin"] == server.origin


@run_async
async def test_recording_failure_does_not_change_dispatch_or_settlement(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """An unwritable trace becomes missing evidence, not a provider failure."""
    monkeypatch.setattr(outbound_audit, "_DIRECTORY", tmp_path)
    (tmp_path / (CALL + ".jsonl")).mkdir()
    clock = Clock()
    hooks = Hooks(clock)
    async with HTTPFaultServer(good_response) as server:
        dispatcher = make_dispatcher(monkeypatch, server, clock, hooks)
        try:
            assert await dispatcher.execute(
                request(), context=context(), validate=valid
            ) == {"ok": True}
            assert (
                len(server.requests)
                == len(hooks.reports)
                == len(hooks.observations)
                == 1
            )
        finally:
            await dispatcher.aclose()


@run_async
async def test_rejected_permit_has_no_send_metadata(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """An authorization attempt cannot masquerade as an HTTP dispatch."""
    monkeypatch.setattr(outbound_audit, "_DIRECTORY", tmp_path)
    clock = Clock()
    hooks = Hooks(clock)
    hooks.patch = {"dispatch_ms": 0}
    async with HTTPFaultServer(good_response) as server:
        dispatcher = make_dispatcher(monkeypatch, server, clock, hooks)
        try:
            with pytest.raises(DispatchError):
                await dispatcher.execute(request(), context=context(), validate=valid)
            assert server.requests == []
            assert not (tmp_path / (CALL + ".jsonl")).exists()
        finally:
            await dispatcher.aclose()


@run_async
async def test_slow_metadata_cannot_send_after_dispatch_permission_expires(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """An attempt record can remain when final permission expires before HTTP."""
    monkeypatch.setattr(outbound_audit, "_DIRECTORY", tmp_path)
    clock = Clock()
    begin = outbound_audit.begin
    sent_at: list[int] = []

    def slow_begin(*args: Any) -> outbound_audit.OutboundAudit | None:
        audit = begin(*args)
        clock.now = 1600  # Permit's dispatch deadline is 1000 + 500 ms.
        return audit

    def send(outbound: httpx.Request) -> httpx.Response:
        sent_at.append(clock.now)
        return httpx.Response(200, content=chat_body())

    monkeypatch.setattr(outbound_audit, "begin", slow_begin)
    hooks = Hooks(clock)
    dispatcher = AuthorizedDispatcher(
        start_frame(),
        hooks=hooks,
        endpoints={"deepseek": Endpoint("https://api.deepseek.com", "synthetic")},
        clock=clock,
    )
    dispatcher._clients["deepseek"] = httpx.AsyncClient(
        transport=httpx.MockTransport(send)
    )
    try:
        with pytest.raises(DispatchError):
            await dispatcher.execute(request(), context=context(), validate=valid)
        assert sent_at == [] and hooks.reports == []
        observed = records(tmp_path)
        assert [value["event"] for value in observed] == ["dispatch_attempt"]
        assert observed[0]["response_complete"] is False
        assert observed[0]["http_status"] is None
    finally:
        await dispatcher.aclose()
