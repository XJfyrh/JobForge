"""Deterministic transport fixtures verify boundaries, not model acceptance."""

import asyncio
import gzip
import json
import time
from collections.abc import AsyncIterator
from types import SimpleNamespace
from typing import Any

import httpx
import pytest
from jobforge_agent import http as agent_http
from jobforge_agent.business import BusinessTools, SnapshotBinding
from jobforge_agent.embedding import MODEL, MODEL_DIGEST, OllamaEmbedding, vector
from jobforge_agent.errors import ToolError
from jobforge_agent.http import BoundedHTTP, strict_json

SNAPSHOT = "535cf2ed-cdba-4d8c-a576-a41c865535da"
INDEX = "f9313cc0-4ed7-4ce6-8da8-206a75dd65d0"
BINDING = SnapshotBinding(SNAPSHOT, "order-1", "a" * 64, INDEX, "policy-v1")
SYNTHETIC_VECTOR = [1.0] + [0.0] * 383


def model_fixture(request: httpx.Request) -> httpx.Response:
    """Return explicit synthetic protocol data, never real model evidence."""
    if request.url.path == "/api/version":
        return httpx.Response(200, json={"version": "0.32.5"})
    if request.url.path == "/api/tags":
        return httpx.Response(
            200, json={"models": [{"name": MODEL, "digest": MODEL_DIGEST}]}
        )
    body = json.loads(request.content)
    assert request.url.path == "/api/embed"
    assert body["truncate"] is False
    assert body["model"] == MODEL
    assert "Authorization" not in request.headers
    return httpx.Response(
        200,
        json={"model": MODEL, "embeddings": [SYNTHETIC_VECTOR] * len(body["input"])},
    )


@pytest.mark.parametrize(
    "value",
    [
        [],
        [0.0] * 384,
        [1.0] * 383,
        [True] * 384,
        [float("nan")] * 384,
        [float("inf")] * 384,
        [1e100] * 384,
        [10**400] * 384,
        [1e-100] * 384,
    ],
)
def test_invalid_vectors_are_rejected(value: Any) -> None:
    """Reject dimensions, booleans, overflow, NaN and zero vectors."""
    with pytest.raises(ToolError, match="INVALID_VECTOR"):
        vector(value)


@pytest.mark.parametrize(
    "raw",
    [
        b'{"a":1,"a":2}',
        b"{} {}",
        b'{"a":NaN}',
        b'{"a":Infinity}',
        b'{"a":1e1000}',
        "{}".encode("utf-16"),
    ],
)
def test_json_rejects_ambiguous_or_nonfinite_payloads(raw: bytes) -> None:
    """Strict decoding disallows duplicate fields and trailing values."""
    with pytest.raises(ToolError, match="INVALID_JSON"):
        strict_json(raw)


@pytest.mark.parametrize(
    "value",
    [
        "https://api.example.com",
        "http://secret@127.0.0.1:11434",
        "http://127.0.0.1:11434/api",
        "http://127.0.0.1:11434?key=value",
        "http://example.com:11434",
    ],
)
def test_embedding_rejects_unregistered_endpoints(value: str) -> None:
    """Free resource preparation cannot target arbitrary paid endpoints."""
    with pytest.raises(ToolError, match="INVALID_CONFIGURATION"):
        OllamaEmbedding(value)


def test_identity_is_checked_before_every_batch() -> None:
    """A changed model digest blocks dispatch even after a previous success."""
    paths: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        paths.append(request.url.path)
        if len(paths) == 5:
            return httpx.Response(
                200, json={"models": [{"name": MODEL, "digest": "wrong"}]}
            )
        return model_fixture(request)

    async def run() -> None:
        embedding = OllamaEmbedding(transport=httpx.MockTransport(handler))
        try:
            assert await embedding.embed(["first"], deadline=time.monotonic() + 2)
            with pytest.raises(ToolError, match="PROFILE_UNAVAILABLE"):
                await embedding.embed(["second"], deadline=time.monotonic() + 2)
        finally:
            await embedding.aclose()

    asyncio.run(run())
    assert paths == [
        "/api/version",
        "/api/tags",
        "/api/embed",
        "/api/version",
        "/api/tags",
    ]


@pytest.mark.parametrize(
    ("response", "code"),
    [
        (
            httpx.Response(302, headers={"Location": "https://other.invalid"}),
            "REDIRECT_REJECTED",
        ),
        (httpx.Response(200, content=b" " * 8193), "RESPONSE_TOO_LARGE"),
        (
            httpx.Response(
                200, content=gzip.compress(b"{}"), headers={"Content-Encoding": "gzip"}
            ),
            "INVALID_RESPONSE",
        ),
        (
            httpx.Response(500, json={"error": {"code": ["secret"]}}),
            "DEPENDENCY_UNAVAILABLE",
        ),
        (
            httpx.Response(
                403, json={"error": {"code": "FORBIDDEN", "message": "secret"}}
            ),
            "FORBIDDEN",
        ),
    ],
)
def test_http_boundary_is_bounded_and_sanitized(
    response: httpx.Response, code: str
) -> None:
    """One attempt rejects redirects, oversized bodies and unsafe error details."""
    calls = 0

    def handler(_request: httpx.Request) -> httpx.Response:
        nonlocal calls
        calls += 1
        return response

    async def run() -> None:
        client = BoundedHTTP(
            "http://127.0.0.1:8088", transport=httpx.MockTransport(handler)
        )
        try:
            with pytest.raises(ToolError, match=code) as error:
                await client.request(
                    "GET", "/fixed", deadline=time.monotonic() + 2, max_response=8192
                )
            assert "secret" not in str(error.value)
        finally:
            await client.aclose()

    asyncio.run(run())
    assert calls == 1


def test_absolute_deadline_cancels_slow_response() -> None:
    """A stalled asynchronous transport is cancelled without retry."""
    started = asyncio.Event()
    cancelled: list[bool] = []

    async def handler(_request: httpx.Request) -> httpx.Response:
        started.set()
        try:
            await asyncio.Future()
        finally:
            cancelled.append(True)
        raise AssertionError("unreachable")

    async def run() -> None:
        client = BoundedHTTP(
            "http://127.0.0.1:8088", transport=httpx.MockTransport(handler)
        )
        try:
            with pytest.raises(ToolError, match="DEADLINE_EXCEEDED"):
                await client.request(
                    "GET", "/fixed", deadline=time.monotonic() + 0.02, max_response=8192
                )
        finally:
            await client.aclose()

    asyncio.run(run())
    assert started.is_set() and cancelled == [True]


@pytest.mark.parametrize(("budget", "advance"), [(0.02, 0.05), (120, 61)])
def test_synchronous_persistence_expiry_prevents_dispatch(
    monkeypatch: pytest.MonkeyPatch, budget: float, advance: float
) -> None:
    """A persisted send intent cannot bypass caller or per-request deadlines."""
    clock = [100.0]
    monkeypatch.setattr(agent_http, "time", SimpleNamespace(monotonic=lambda: clock[0]))
    intents: list[str] = []
    physical_dispatches: list[str] = []

    def before_send(path: str) -> None:
        intents.append(path)
        clock[0] += advance

    def handler(request: httpx.Request) -> httpx.Response:
        physical_dispatches.append(request.url.path)
        return httpx.Response(200, json={})

    async def run() -> None:
        client = BoundedHTTP(
            "http://127.0.0.1:8088",
            transport=httpx.MockTransport(handler),
            before_send=before_send,
        )
        try:
            with pytest.raises(ToolError, match="DEADLINE_EXCEEDED"):
                await client.request(
                    "GET", "/fixed", deadline=100 + budget, max_response=8192
                )
        finally:
            await client.aclose()

    asyncio.run(run())
    assert intents == ["/fixed"]
    assert physical_dispatches == []


@pytest.mark.parametrize("expiry_stage", ["response", "decode", "close"])
def test_synchronous_response_completion_cannot_return_after_deadline(
    monkeypatch: pytest.MonkeyPatch, expiry_stage: str
) -> None:
    """Even immediate transport, JSON decode and cleanup must respect expiry."""
    clock = [100.0]
    monkeypatch.setattr(agent_http, "time", SimpleNamespace(monotonic=lambda: clock[0]))
    physical_dispatches: list[str] = []

    class ResponseBody(httpx.AsyncByteStream):
        """Provide a synthetic immediate body and controllable close time."""

        async def __aiter__(self) -> AsyncIterator[bytes]:
            """Yield one synthetic JSON object without yielding control."""
            yield b"{}"

        async def aclose(self) -> None:
            """Model a synchronous cleanup delay without sleeping."""
            if expiry_stage == "close":
                clock[0] += 0.05

    def handler(request: httpx.Request) -> httpx.Response:
        physical_dispatches.append(request.url.path)
        if expiry_stage == "response":
            clock[0] += 0.05
        return httpx.Response(200, stream=ResponseBody())

    original_json = agent_http.strict_json

    def decode(raw: bytes) -> Any:
        result = original_json(raw)
        if expiry_stage == "decode":
            clock[0] += 0.05
        return result

    monkeypatch.setattr(agent_http, "strict_json", decode)

    async def run() -> None:
        client = BoundedHTTP(
            "http://127.0.0.1:8088", transport=httpx.MockTransport(handler)
        )
        try:
            with pytest.raises(ToolError, match="DEADLINE_EXCEEDED"):
                await client.request(
                    "GET", "/fixed", deadline=100.02, max_response=8192
                )
        finally:
            await client.aclose()

    asyncio.run(run())
    assert physical_dispatches == ["/fixed"]


@pytest.mark.parametrize(
    ("name", "arguments", "code"),
    [
        ("write_ticket", {}, "UNKNOWN_TOOL"),
        (["get_order"], {}, "UNKNOWN_TOOL"),
        ("get_order", {"order_id": "wrong"}, "OBJECT_NOT_AUTHORIZED"),
        ("get_delivery", {"order_id": 42}, "INVALID_ARGUMENT"),
        ("get_order", {"order_id": "order-1", "tenant": "other"}, "INVALID_ARGUMENT"),
        ("search_policy", {"query": "x", "top_k": 10}, "INVALID_ARGUMENT"),
        ("search_policy", {"query": "中" * 171}, "INVALID_ARGUMENT"),
        ("search_policy", {"query": "\ud800"}, "INVALID_ARGUMENT"),
        ("search_policy", {"query": ""}, "INVALID_ARGUMENT"),
    ],
)
def test_tool_rejection_happens_before_any_http(
    name: Any, arguments: Any, code: str
) -> None:
    """Model content cannot grant capabilities, alter bindings or spend a call."""

    def no_http(_request: httpx.Request) -> httpx.Response:
        raise AssertionError("invalid arguments must not dispatch")

    async def run() -> None:
        embedding = OllamaEmbedding(transport=httpx.MockTransport(no_http))
        tools = BusinessTools(
            "http://127.0.0.1:8088",
            "synthetic-key",
            BINDING,
            embedding,
            transport=httpx.MockTransport(no_http),
        )
        try:
            with pytest.raises(ToolError, match=code):
                await tools.execute(name, arguments, deadline=time.monotonic() + 2)
            with pytest.raises(ToolError, match="INVALID_TOOL_CALL"):
                await tools.execute_call(
                    [{"name": "get_order", "arguments": {}}],
                    deadline=time.monotonic() + 2,
                )
        finally:
            await tools.aclose()
            await embedding.aclose()

    asyncio.run(run())


def test_search_uses_registered_http_contract_and_snapshot_evidence() -> None:
    """Synthetic transport checks the exact wire shape but not actual retrieval."""

    def business(request: httpx.Request) -> httpx.Response:
        assert request.url.path == f"/business/v1/snapshots/{SNAPSHOT}/policies/search"
        assert request.headers["Authorization"] == "Bearer synthetic-key"
        body = json.loads(request.content)
        assert set(body) == {"embedding_model", "embedding_digest", "query_vector"}
        assert body["query_vector"] == SYNTHETIC_VECTOR
        return httpx.Response(
            200,
            json={
                "snapshot_id": SNAPSHOT,
                "matches": [
                    {
                        "index_id": INDEX,
                        "chunk_id": "P01.1",
                        "policy_version": "policy-v1",
                        "evidence_ref": f"business-policy:{INDEX}:P01.1",
                        "text": "synthetic paragraph",
                        "source": "P01.md",
                        "distance": 0.0,
                    }
                ],
            },
        )

    async def run() -> None:
        embedding = OllamaEmbedding(transport=httpx.MockTransport(model_fixture))
        tools = BusinessTools(
            "http://127.0.0.1:8088",
            "synthetic-key",
            BINDING,
            embedding,
            transport=httpx.MockTransport(business),
        )
        try:
            result = await tools.execute(
                "search_policy",
                {"query": "delivery delay"},
                deadline=time.monotonic() + 2,
            )
            assert result["matches"][0]["chunk_id"] == "P01.1"
        finally:
            await tools.aclose()
            await embedding.aclose()

    asyncio.run(run())
