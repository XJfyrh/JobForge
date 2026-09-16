"""Bounded Linux FD transport; permission remains in the original Conversation."""

from __future__ import annotations

import asyncio
import copy
import os
import sys
from dataclasses import dataclass

from jobforge_agent.dispatch import DispatchError
from jobforge_agent.protocol_v2 import (
    MAX_FRAME_BYTES,
    MAX_METERING_FRAME_BYTES,
    Frame,
    ProtocolError,
    decode,
    decode_metering,
    encode,
    encode_metering,
)


@dataclass
class _Pending:
    source: Frame
    kind: str
    future: asyncio.Future[Frame]


class PipeHooks:
    """Own exactly one nonblocking reader/writer per fixed protocol direction."""

    def __init__(self) -> None:
        """Allocate only bounded routes; open owns descriptor initialization."""
        self._loop = asyncio.get_running_loop()
        self._execute: asyncio.Future[Frame] = self._loop.create_future()
        self._started = False
        self._execute_read = False
        self._result_written = False
        self._closing = False
        self._failure: DispatchError | None = None
        self._failed = asyncio.Event()
        self._pending: dict[bool, _Pending | None] = {False: None, True: None}
        self._queues: dict[bool, asyncio.Queue[tuple[bytes, asyncio.Future[None]]]] = {
            False: asyncio.Queue(1),
            True: asyncio.Queue(1),
        }
        self._tasks: list[asyncio.Task[None]] = []
        self._writing: dict[bool, bool] = {False: False, True: False}
        self._buffers: dict[bool, bytearray] = {False: bytearray(), True: bytearray()}
        self._reports: dict[str, bytes] = {}

    @classmethod
    async def open(cls) -> PipeHooks:
        """Bind only the four inherited descriptors; never accept caller paths."""
        if sys.platform != "linux":
            raise DispatchError("PROFILE_UNAVAILABLE")
        result = cls()
        try:
            for descriptor in (0, 1, 4, 5):
                os.set_blocking(descriptor, False)
            for metering in (False, True):
                result._tasks.append(asyncio.create_task(result._reader(metering)))
                result._tasks.append(asyncio.create_task(result._writer(metering)))
        except OSError:
            await result.aclose()
            raise DispatchError("PROTOCOL_ERROR") from None
        return result

    def _check(self) -> None:
        if self._failure is not None:
            raise DispatchError(self._failure.code, fact=self._failure.fact)
        if self._closing:
            raise DispatchError("STOP_REQUESTED")

    def _fail(self, error: DispatchError) -> None:
        if self._closing or self._failure is not None:
            return
        self._failure = error
        self._failed.set()
        # Wake all routes. The main owner stops HTTP even if no hook is waiting.
        for future in [self._execute] + [
            item.future for item in self._pending.values() if item is not None
        ]:
            if not future.done():
                future.set_exception(DispatchError(error.code, fact=error.fact))
                future.exception()

    async def wait_failed(self) -> None:
        """Wake the step owner on every unsolicited, malformed or extra frame."""
        await self._failed.wait()
        self._check()

    async def _ready(self, descriptor: int, *, write: bool = False) -> None:
        ready: asyncio.Future[None] = self._loop.create_future()

        def wake() -> None:
            if not ready.done():
                ready.set_result(None)

        add = self._loop.add_writer if write else self._loop.add_reader
        remove = self._loop.remove_writer if write else self._loop.remove_reader
        add(descriptor, wake)
        try:
            await ready
        finally:
            remove(descriptor)

    def _route(self, frame: Frame, metering: bool) -> None:
        if not metering and not self._started:
            if frame["kind"] != "execute_step":
                raise ProtocolError()
            self._started = True
            self._execute.set_result(copy.deepcopy(frame))
            return
        pending = self._pending[metering]
        if pending is None or frame["kind"] != pending.kind:
            raise ProtocolError()
        for field in ("request_id", "binding", "call_sequence"):
            if frame[field] != pending.source[field]:
                raise ProtocolError()
        if pending.kind == "call_permit":
            fields: tuple[str, ...] = (
                "subcall",
                "parameter_hash",
                "tool_invocation_id",
            )
        else:
            fields = ("physical_call_id",)
        if any(frame[field] != pending.source[field] for field in fields):
            raise ProtocolError()
        if (
            pending.kind == "metering_ack"
            and frame["report_hash"] != pending.source["report_hash"]
        ):
            raise ProtocolError()
        self._pending[metering] = None
        if pending.future.done():
            raise ProtocolError()
        pending.future.set_result(copy.deepcopy(frame))

    async def _reader(self, metering: bool) -> None:
        descriptor = 4 if metering else 0
        limit = MAX_METERING_FRAME_BYTES if metering else MAX_FRAME_BYTES
        buffer = self._buffers[metering]
        decoder = decode_metering if metering else decode
        try:
            while True:
                await self._ready(descriptor)
                try:
                    chunk = os.read(descriptor, min(4096, limit + 1 - len(buffer)))
                except BlockingIOError:
                    continue
                if not chunk:
                    raise ProtocolError()
                buffer.extend(chunk)
                while b"\n" in buffer:
                    end = buffer.index(10) + 1
                    line = bytes(buffer[:end])
                    del buffer[:end]
                    self._route(decoder(line), metering)
                if len(buffer) >= limit:
                    raise ProtocolError("FRAME_LIMIT")
        except ProtocolError as error:
            self._fail(
                DispatchError(
                    "PROTOCOL_ERROR",
                    fact="size_limit" if error.code == "FRAME_LIMIT" else "",
                )
            )
        except Exception:
            # A route/selector programming failure is also a terminal pipe fact;
            # an unobserved reader task must never silently leave HTTP running.
            self._fail(DispatchError("PROTOCOL_ERROR"))

    async def _writer(self, metering: bool) -> None:
        descriptor = 5 if metering else 1
        completion: asyncio.Future[None] | None = None
        try:
            while True:
                raw, completion = await self._queues[metering].get()
                offset = 0
                while offset < len(raw):
                    if not metering:
                        self._check()
                    await self._ready(descriptor, write=True)
                    if not metering:
                        self._check()
                    try:
                        written = os.write(descriptor, raw[offset:])
                    except BlockingIOError:
                        continue
                    if written <= 0:
                        raise OSError()
                    offset += written
                if not completion.done():
                    completion.set_result(None)
                completion = None
        except Exception:
            self._fail(DispatchError("PROTOCOL_ERROR"))
        finally:
            if completion is not None and not completion.done():
                completion.set_exception(DispatchError("PROTOCOL_ERROR"))

    async def _write(
        self, frame: Frame, metering: bool, *, flush: bool = False
    ) -> None:
        if not flush:
            self._check()
        if self._writing[metering]:
            raise DispatchError("PROTOCOL_ERROR")
        self._writing[metering] = True
        try:
            raw = encode_metering(frame) if metering else encode(frame)
            allowed = (
                {"metering_report"}
                if metering
                else {"call_intent", "call_observation", "step_result"}
            )
            if frame["kind"] not in allowed:
                raise ProtocolError()
            if metering:
                physical = frame["physical_call_id"]
                previous = self._reports.get(physical)
                if previous is not None and previous != raw:
                    raise ProtocolError()
                if previous is None and len(self._reports) >= 44:
                    raise ProtocolError()
                self._reports[physical] = raw
            completion: asyncio.Future[None] = self._loop.create_future()
            self._queues[metering].put_nowait((raw, completion))
            await completion
            if not flush:
                self._check()
        except ProtocolError as error:
            failure = DispatchError(
                "PROTOCOL_ERROR",
                fact="size_limit" if error.code == "FRAME_LIMIT" else "",
            )
            self._fail(failure)
            raise failure from None
        except asyncio.QueueFull:
            self._fail(DispatchError("PROTOCOL_ERROR"))
            raise DispatchError("PROTOCOL_ERROR") from None
        finally:
            self._writing[metering] = False

    async def read_execute(self) -> Frame:
        """Read precisely the one initial standard execute frame."""
        if self._execute_read:
            raise DispatchError("PROTOCOL_ERROR")
        self._execute_read = True
        frame = await self._execute
        self._check()
        return copy.deepcopy(frame)

    async def _exchange(self, source: Frame, kind: str, metering: bool) -> Frame:
        self._check()
        if self._pending[metering] is not None:
            raise DispatchError("PROTOCOL_ERROR")
        future: asyncio.Future[Frame] = self._loop.create_future()
        pending = _Pending(copy.deepcopy(source), kind, future)
        self._pending[metering] = pending
        try:
            await self._write(source, metering)
            answer = await future
            self._check()
            return answer
        finally:
            if self._pending[metering] is pending:
                self._pending[metering] = None
            if not future.done():
                future.cancel()

    async def authorize(self, intent: Frame) -> Frame:
        """Route an exact permit without granting dispatch permission locally."""
        return await self._exchange(intent, "call_permit", False)

    async def observe(self, observation: Frame) -> Frame:
        """Route the ordinary acknowledgement independently of metering."""
        return await self._exchange(observation, "call_observation_ack", False)

    async def settle(self, report: Frame) -> Frame:
        """Route one standard metering acknowledgement."""
        return await self._exchange(report, "metering_ack", True)

    async def write_result(self, result: Frame) -> None:
        """Write once and finish without waiting for a nonexistent Commit ACK."""
        self._check()
        if self._result_written or result.get("kind") != "step_result":
            raise DispatchError("PROTOCOL_ERROR")
        self._result_written = True
        await self._write(result, False)
        await asyncio.sleep(0)
        self._check()

    async def flush_captured(self, reports: tuple[Frame, ...]) -> None:
        """Best-effort resend captured complete reports, without waiting for ACK."""
        for report in reports:
            if self._writing[True] or self._closing:
                return
            await self._write(report, True, flush=True)

    async def aclose(self) -> None:
        """Cancel and join all four owners before closing the inherited pipes."""
        if self._closing:
            return
        # Detect buffered/ready trailing bytes even if the result writer won the
        # last event-loop scheduling turn. Closing must not erase extra frames.
        for metering, descriptor in ((False, 0), (True, 4)):
            try:
                extra = os.read(descriptor, 1)
            except BlockingIOError:
                extra = b""
            except OSError:
                extra = b""
                self._fail(DispatchError("PROTOCOL_ERROR"))
            if extra or self._buffers[metering]:
                self._fail(DispatchError("PROTOCOL_ERROR"))
        self._closing = True
        for task in self._tasks:
            task.cancel()
        await asyncio.gather(*self._tasks, return_exceptions=True)
        for descriptor in (0, 1, 4, 5):
            try:
                os.close(descriptor)
            except OSError:
                pass
        if self._failure is not None:
            raise DispatchError(self._failure.code, fact=self._failure.fact)
