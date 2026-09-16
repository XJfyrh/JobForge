"""Request-owned real TCP HTTP fault server; no model or control service fake."""

from __future__ import annotations

import asyncio
from collections.abc import Awaitable, Callable, Mapping
from dataclasses import dataclass, field
from types import TracebackType


@dataclass(frozen=True)
class ReceivedRequest:
    """Hold actual received request bytes for local test assertions only."""

    method: str
    path: str
    headers: Mapping[str, str] = field(repr=False)
    body: bytes = field(repr=False)


Handler = Callable[
    [asyncio.StreamReader, asyncio.StreamWriter, ReceivedRequest], Awaitable[None]
]


async def respond(
    writer: asyncio.StreamWriter,
    *,
    status: int = 200,
    body: bytes = b"{}",
    headers: Mapping[str, str] | None = None,
) -> None:
    """Send one complete raw HTTP response without framework normalization."""
    fields = {"Content-Length": str(len(body)), "Connection": "close"}
    fields.update(headers or {})
    head = f"HTTP/1.1 {status} Response\r\n" + "".join(
        f"{name}: {value}\r\n" for name, value in fields.items()
    )
    writer.write(head.encode("ascii") + b"\r\n" + body)
    await writer.drain()


class HTTPFaultServer:
    """Own a loopback listener and join every connection on context exit."""

    def __init__(self, handler: Handler) -> None:
        """Require an explicit response/fault handler for each scenario."""
        self.handler = handler
        self.requests: list[ReceivedRequest] = []
        self.origin = ""
        self._server: asyncio.Server | None = None
        self._tasks: set[asyncio.Task[None]] = set()
        self._writers: set[asyncio.StreamWriter] = set()

    async def __aenter__(self) -> HTTPFaultServer:
        """Listen on an OS-assigned IPv4 loopback port."""
        self._server = await asyncio.start_server(self._connected, "127.0.0.1", 0)
        self.origin = f"http://127.0.0.1:{self._server.sockets[0].getsockname()[1]}"
        return self

    def _connected(
        self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter
    ) -> None:
        task = asyncio.create_task(self._serve(reader, writer))
        self._tasks.add(task)
        self._writers.add(writer)

    async def _serve(
        self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter
    ) -> None:
        try:
            raw = await reader.readuntil(b"\r\n\r\n")
            lines = raw.decode("ascii").split("\r\n")
            method, path, _ = lines[0].split(" ")
            headers = dict(
                (name.lower(), value.strip())
                for name, value in (line.split(":", 1) for line in lines[1:] if line)
            )
            length = int(headers.get("content-length", "0"))
            if not 0 <= length <= 65536:
                raise ValueError("test request limit")
            body = await reader.readexactly(length)
            request = ReceivedRequest(method, path, headers, body)
            self.requests.append(request)
            await self.handler(reader, writer, request)
        except (ConnectionError, asyncio.IncompleteReadError):
            pass
        finally:
            writer.close()
            try:
                await writer.wait_closed()
            except ConnectionError:
                pass
            self._writers.discard(writer)

    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        traceback: TracebackType | None,
    ) -> None:
        """Cancel and retrieve every connection task, including completed tasks."""
        assert self._server is not None
        self._server.close()
        await self._server.wait_closed()
        for writer in self._writers:
            writer.close()
        for task in self._tasks:
            if not task.done():
                task.cancel()
        results = await asyncio.gather(*self._tasks, return_exceptions=True)
        self._tasks.clear()
        # Windows Proactor delivers listener cancellation through queued IOCP
        # callbacks after wait_closed; let those finite callbacks complete.
        await asyncio.sleep(0)
        await asyncio.sleep(0)
        if exc is None:
            for result in results:
                if isinstance(result, Exception):
                    raise result
