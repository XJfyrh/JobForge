"""Bounded JSON HTTP with explicit deadlines and no implicit dispatches."""

from __future__ import annotations

import asyncio
import json
import math
import time
from collections.abc import Callable
from typing import Any
from urllib.parse import urlsplit

import httpx

from jobforge_agent.errors import ToolError

ERROR_CODES = {
    "INVALID_ARGUMENT",
    "UNAUTHORIZED",
    "FORBIDDEN",
    "NOT_FOUND",
    "CONFLICT",
    "PROFILE_UNAVAILABLE",
    "RATE_LIMITED",
    "INTERNAL",
    "DEPENDENCY_UNAVAILABLE",
}


def strict_json(raw: bytes) -> Any:
    """Reject duplicate keys, non-finite constants, and trailing JSON."""

    def pairs(items: list[tuple[str, Any]]) -> dict[str, Any]:
        result: dict[str, Any] = {}
        for key, value in items:
            if key in result:
                raise ValueError("duplicate field")
            result[key] = value
        return result

    def constant(_value: str) -> None:
        raise ValueError("non-finite JSON")

    def finite_float(value: str) -> float:
        result = float(value)
        if not math.isfinite(result):
            raise ValueError("non-finite JSON")
        return result

    try:
        return json.loads(
            raw.decode("utf-8"),
            object_pairs_hook=pairs,
            parse_constant=constant,
            parse_float=finite_float,
        )
    except (ValueError, UnicodeError, RecursionError) as exc:
        raise ToolError("INVALID_JSON") from exc


def origin(value: str) -> str:
    """Validate a trusted deployment origin without URL paths or credentials."""
    try:
        parsed = urlsplit(value)
        port = parsed.port
        if (
            parsed.scheme not in {"http", "https"}
            or not parsed.hostname
            or parsed.username is not None
            or parsed.password is not None
            or parsed.path not in {"", "/"}
            or parsed.query
            or parsed.fragment
            or port == 0
            or any(ord(char) <= 32 for char in value)
        ):
            raise ValueError("invalid origin")
    except ValueError as exc:
        raise ToolError("INVALID_CONFIGURATION") from exc
    return value.rstrip("/")


class BoundedHTTP:
    """Own one HTTP client with explicit authentication and finite resources."""

    def __init__(
        self,
        base_origin: str,
        *,
        bearer_key: str | None = None,
        transport: httpx.AsyncBaseTransport | None = None,
        before_send: Callable[[str], None] | None = None,
    ) -> None:
        """Create the client; transport injection is for deterministic tests only."""
        headers = {"Accept": "application/json", "Accept-Encoding": "identity"}
        if bearer_key is not None:
            if not bearer_key or len(bearer_key) > 4096 or not bearer_key.isascii():
                raise ToolError("INVALID_CONFIGURATION")
            if any(ord(char) <= 32 or ord(char) == 127 for char in bearer_key):
                raise ToolError("INVALID_CONFIGURATION")
            headers["Authorization"] = f"Bearer {bearer_key}"
        self._origin = origin(base_origin)
        self._before_send = before_send
        self._client = httpx.AsyncClient(
            headers=headers,
            follow_redirects=False,
            trust_env=False,
            timeout=httpx.Timeout(60, connect=5),
            limits=httpx.Limits(max_connections=1, max_keepalive_connections=1),
            transport=transport or httpx.AsyncHTTPTransport(retries=0, trust_env=False),
        )
        self._busy = False

    async def aclose(self) -> None:
        """Close the client's connection pool on every ownership exit."""
        await self._client.aclose()

    async def request(
        self,
        method: str,
        path: str,
        *,
        deadline: float,
        max_response: int,
        body: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Attempt at most one request, rejecting concurrent waiting queues."""
        if self._busy:
            raise ToolError("RATE_LIMITED")
        started = time.monotonic()
        remaining = deadline - started
        if not math.isfinite(remaining) or remaining <= 0:
            raise ToolError("DEADLINE_EXCEEDED")
        if not path.startswith("/") or path.startswith("//"):
            raise ToolError("INVALID_CONFIGURATION")
        request_deadline = min(deadline, started + 60)
        self._busy = True
        try:
            async with asyncio.timeout(min(60, remaining)):
                if self._before_send is not None:
                    self._before_send(path)
                # Persistence can block without giving the asyncio timer a turn.
                # A recorded intent must not permit a send after its deadline.
                if time.monotonic() >= request_deadline:
                    raise ToolError("DEADLINE_EXCEEDED")
                async with self._client.stream(
                    method, self._origin + path, json=body
                ) as response:
                    if 300 <= response.status_code < 400:
                        raise ToolError("REDIRECT_REJECTED")
                    if (
                        response.headers.get("content-encoding", "identity")
                        != "identity"
                    ):
                        raise ToolError("INVALID_RESPONSE")
                    length = response.headers.get("content-length")
                    if length is not None:
                        try:
                            if int(length) > max_response or int(length) < 0:
                                raise ToolError("RESPONSE_TOO_LARGE")
                        except ValueError as exc:
                            raise ToolError("INVALID_RESPONSE") from exc
                    data = bytearray()
                    async for chunk in response.aiter_bytes(chunk_size=4096):
                        if len(data) + len(chunk) > max_response:
                            raise ToolError("RESPONSE_TOO_LARGE")
                        data.extend(chunk)
                    result = strict_json(bytes(data))
                    if time.monotonic() >= request_deadline:
                        raise ToolError("DEADLINE_EXCEEDED")
                    if not isinstance(result, dict):
                        raise ToolError("INVALID_RESPONSE")
                    if response.status_code != 200:
                        error = result.get("error")
                        code = error.get("code") if isinstance(error, dict) else None
                        raise ToolError(
                            code
                            if isinstance(code, str) and code in ERROR_CODES
                            else "DEPENDENCY_UNAVAILABLE"
                        )
                # Include response cleanup, which may also complete synchronously.
                if time.monotonic() >= request_deadline:
                    raise ToolError("DEADLINE_EXCEEDED")
                return result
        except TimeoutError as exc:
            raise ToolError("DEADLINE_EXCEEDED") from exc
        except httpx.HTTPError as exc:
            raise ToolError("DEPENDENCY_UNAVAILABLE") from exc
        finally:
            self._busy = False
