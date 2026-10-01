"""Scoped proxy configuration and real CONNECT refusal without provider traffic."""

from __future__ import annotations

import asyncio
import ssl
from pathlib import Path

import certifi
import httpx
import pytest
from jobforge_agent.dispatch import AuthorizedDispatcher, DispatchError, Endpoint
from test_dispatch import Clock, Hooks, run_async, start_frame


def dispatcher(endpoint: Endpoint, alias="deepseek") -> AuthorizedDispatcher:
    """Construct the production authority with fixture hooks and no paid I/O."""
    clock = Clock()
    return AuthorizedDispatcher(
        start_frame(), hooks=Hooks(clock), endpoints={alias: endpoint}, clock=clock
    )


@pytest.mark.parametrize(
    "proxy,ca",
    [
        ("http://proxy:8080", ""),
        ("", "/etc/ca.pem"),
        ("http://user:password@proxy:8080", "/etc/ca.pem"),
        ("http://proxy:8080/path", "/etc/ca.pem"),
        ("http://proxy:8080?secret=x", "/etc/ca.pem"),
        ("socks5://proxy:8080", "/etc/ca.pem"),
        ("http://proxy:8080", "relative.pem"),
        ("http://proxy:8080", "/etc/../ca.pem"),
        ("http://proxy:8080", "/etc/ca.pem\nINJECT=value"),
    ],
)
def test_rejects_ambiguous_proxy_and_ca_before_dispatch(proxy: str, ca: str) -> None:
    """Reject unsafe deployment inputs before any permission or network operation."""
    with pytest.raises(DispatchError, match="^INPUT_INVALID$"):
        dispatcher(Endpoint("https://api.deepseek.com", "fixture-key", proxy, ca))


@pytest.mark.parametrize(
    "alias,origin",
    [("business", "http://business:8090"), ("ollama", "http://ollama:11434")],
)
def test_nonprovider_endpoint_cannot_use_provider_proxy(
    alias: str, origin: str
) -> None:
    """Keep proxy trust scoped to the fixed provider endpoint."""
    with pytest.raises(DispatchError, match="^INPUT_INVALID$"):
        dispatcher(Endpoint(origin, None, "http://proxy:8080", "/etc/ca.pem"), alias)


def test_missing_or_invalid_ca_fails_without_client_or_secret_diagnostic(
    tmp_path: Path,
) -> None:
    """Fail closed without copying certificate or credential content into errors."""
    for path in (tmp_path / "absent.pem", tmp_path / "invalid.pem"):
        if path.name == "invalid.pem":
            path.write_text("synthetic invalid certificate")
        value = dispatcher(
            Endpoint(
                "https://api.deepseek.com",
                "fixture-key",
                "http://127.0.0.1:9",
                str(path),
            )
        )
        with pytest.raises(DispatchError, match="^PROFILE_UNAVAILABLE$"):
            value._client("deepseek")
        assert not value._clients


@run_async
async def test_proxy_connect_refusal_does_not_leak_provider_key_or_retry(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Exercise a real refusing CONNECT proxy without reaching the provider."""
    requests: list[bytes] = []

    async def refuse(
        reader: asyncio.StreamReader, writer: asyncio.StreamWriter
    ) -> None:
        requests.append(await reader.readuntil(b"\r\n\r\n"))
        writer.write(
            b"HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n"
        )
        await writer.drain()
        writer.close()
        await writer.wait_closed()

    server = await asyncio.start_server(refuse, "127.0.0.1", 0)
    proxy = f"http://127.0.0.1:{server.sockets[0].getsockname()[1]}"
    monkeypatch.setenv("HTTPS_PROXY", "http://untrusted.invalid:1")
    monkeypatch.setenv("SSL_CERT_FILE", "/untrusted/ca.pem")
    value = dispatcher(
        Endpoint("https://api.deepseek.com", "fixture-key", proxy, certifi.where())
    )
    try:
        with pytest.raises(httpx.ProxyError):
            await value._client("deepseek").get("https://api.deepseek.com/models")
        assert len(requests) == 1
        assert requests[0].startswith(b"CONNECT api.deepseek.com:443 HTTP/1.1\r\n")
        assert (
            b"fixture-key" not in requests[0] and b"Authorization:" not in requests[0]
        )
    finally:
        await value.aclose()
        server.close()
        await server.wait_closed()


@pytest.mark.parametrize("scheme", ["http", "https"])
def test_explicit_ca_keeps_hostname_and_certificate_verification(
    monkeypatch: pytest.MonkeyPatch,
    scheme: str,
) -> None:
    """Custom trust anchors must not disable hostname or chain verification."""
    captured = []
    original = ssl.create_default_context

    def create(*args, **kwargs):
        context = original(*args, **kwargs)
        captured.append(context)
        return context

    monkeypatch.setattr(ssl, "create_default_context", create)
    value = dispatcher(
        Endpoint(
            "https://api.deepseek.com",
            "fixture-key",
            f"{scheme}://127.0.0.1:9",
            certifi.where(),
        )
    )
    client = value._client("deepseek")
    assert len(captured) >= 1
    assert all(
        c.check_hostname and c.verify_mode == ssl.CERT_REQUIRED for c in captured
    )
    asyncio.run(client.aclose())


@run_async
async def test_business_ignores_ambient_proxy_and_ca(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Unrelated endpoints remain direct despite ambient routing configuration."""
    requests = []

    async def respond(
        reader: asyncio.StreamReader, writer: asyncio.StreamWriter
    ) -> None:
        requests.append(await reader.readuntil(b"\r\n\r\n"))
        writer.write(
            b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}"
        )
        await writer.drain()
        writer.close()
        await writer.wait_closed()

    server = await asyncio.start_server(respond, "127.0.0.1", 0)
    url = f"http://127.0.0.1:{server.sockets[0].getsockname()[1]}"
    monkeypatch.setenv("HTTP_PROXY", "http://127.0.0.1:9")
    monkeypatch.setenv("NO_PROXY", "")
    monkeypatch.setenv("SSL_CERT_FILE", "/untrusted/ca.pem")
    value = dispatcher(Endpoint(url, "synthetic-business"), "business")
    try:
        response = await value._client("business").get(url)
        assert response.status_code == 200 and len(requests) == 1
    finally:
        await value.aclose()
        server.close()
        await server.wait_closed()


@run_async
@pytest.mark.parametrize(
    "mode,reason",
    [
        ("tls_stall", "connect_timeout"),
        ("connect_stall", "read_timeout"),
        ("refuse", "proxy_error"),
    ],
)
async def test_real_proxy_failure_retains_safe_phase_and_unknown_usage(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, mode: str, reason: str
) -> None:
    """Use only loopback: even the TLS-stall case never reaches a provider."""
    import json
    import time

    from jobforge_agent import outbound_audit
    from test_dispatch import context, valid
    from test_outbound_audit import request

    monkeypatch.setattr(outbound_audit, "_DIRECTORY", tmp_path)
    requests: list[bytes] = []
    finished = asyncio.Event()

    async def proxy(reader, writer) -> None:
        try:
            requests.append(await reader.readuntil(b"\r\n\r\n"))
            if mode == "refuse":
                writer.write(b"HTTP/1.1 407 Refused\r\nContent-Length: 0\r\n\r\n")
                await writer.drain()
                return
            if mode == "tls_stall":
                writer.write(b"HTTP/1.1 200 Connection established\r\n\r\n")
                await writer.drain()
            # Consume only the TLS ClientHello, never acknowledge the handshake.
            while await reader.read(4096):
                pass
        finally:
            writer.close()
            await writer.wait_closed()
            finished.set()

    server = await asyncio.start_server(proxy, "127.0.0.1", 0)
    value = dispatcher(
        Endpoint(
            "https://api.deepseek.com",
            "fixture-key",
            f"http://127.0.0.1:{server.sockets[0].getsockname()[1]}",
            certifi.where(),
        )
    )
    client = value._client("deepseek")
    assert client.timeout.connect == 5 and client.timeout.read == 60
    if mode == "connect_stall":
        # Shorten only this read reproduction; keep production TLS timeout below.
        client.timeout = httpx.Timeout(0.1, connect=5)
    started = time.monotonic()
    try:
        with pytest.raises(DispatchError):
            await value.execute(request(), context=context(), validate=valid)
        elapsed = time.monotonic() - started
        if mode == "tls_stall":
            assert 4.5 <= elapsed < 10
        reports = value.recorded_usage()
        assert len(reports) == 1
        assert reports[0]["usage"] is None
        assert reports[0]["provider_audit"]["http_status"] == 0
        assert reports[0]["provider_audit"]["response_complete"] is False
        diagnostics = list(tmp_path.glob("*.failure.json"))
        assert len(diagnostics) == 1
        diagnostic = json.loads(diagnostics[0].read_text())
        assert diagnostic["reason"] == reason
        assert diagnostic["stage"] == "send"
        assert diagnostic["buffered_bytes"] == 0
        stages = [
            json.loads(line)
            for line in next(tmp_path.glob("*.transport.jsonl"))
            .read_text()
            .splitlines()
        ]
        assert any(
            row["event"] == "http11.send_request_headers.complete"
            and row["hop"] == "proxy"
            for row in stages
        )
        assert not any(
            row["event"] == "http11.send_request_headers.started"
            and row["hop"] == "origin"
            for row in stages
        )
        if mode == "tls_stall":
            assert stages[-1]["event"] == "proxy.start_tls.failed"
            assert stages[-1]["hop"] == "origin"
        assert len(requests) == 1  # No automatic retry.
        assert b"fixture-key" not in requests[0]
        assert b"Authorization" not in requests[0]
        assert b"PRIVATE-CUSTOMER-BODY" not in requests[0]
        assert "fixture-key" not in diagnostics[0].read_text()
    finally:
        await value.aclose()
        await asyncio.wait_for(finished.wait(), 2)
        server.close()
        await server.wait_closed()
