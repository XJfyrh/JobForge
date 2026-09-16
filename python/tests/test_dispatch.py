"""Real TCP tests of authorization, cancellation and independent usage capture."""

from __future__ import annotations

import asyncio
import copy
import hashlib
import json
from collections.abc import Callable, Coroutine
from dataclasses import replace
from functools import wraps
from pathlib import Path
from typing import Any, ParamSpec, TypeVar

import httpx
import pytest
from http_fault_server import HTTPFaultServer, ReceivedRequest, respond
from jobforge_agent import dispatch
from jobforge_agent.dispatch import (
    AuthorizedDispatcher,
    CompleteResponse,
    DispatchError,
    Endpoint,
    RunCallContext,
    UsageEvidence,
    prepare_request,
)
from jobforge_agent.errors import ToolError
from jobforge_agent.http import strict_json
from jobforge_agent.protocol_v2 import MAX_INTEGER, Frame, ProtocolError

FIXTURES = json.loads(
    (
        Path(__file__).resolve().parents[2] / "api/executor/v2/fixtures/frames.json"
    ).read_text(encoding="utf-8")
)


P = ParamSpec("P")
R = TypeVar("R")


def run_async(function: Callable[P, Coroutine[Any, Any, R]]) -> Callable[P, R]:
    """Own the full event loop for each synchronous pytest entry point."""

    @wraps(function)
    def wrapped(*args: P.args, **kwargs: P.kwargs) -> R:
        """Wrapped."""
        return asyncio.run(function(*args, **kwargs))

    return wrapped


class Clock:
    """Inject suspend-like BOOTTIME jumps without waiting for wall time."""

    def __init__(self) -> None:
        """Init."""
        self.now = 1000

    def __call__(self) -> int:
        """Call."""
        return self.now


def start_frame(step: str = "model_proposal") -> Frame:
    """Start frame."""
    frame = copy.deepcopy(FIXTURES["valid_frames"][0])
    frame["emitted_mono_ms"] = 1000
    frame["remaining_ms"] = 10000
    frame["binding"]["step_kind"] = step
    return frame


def context(step: str = "model_proposal") -> RunCallContext:
    """Context."""
    return RunCallContext(
        "a" * 64,
        "b" * 64,
        "" if step == "model_proposal" else "00000000-0000-4000-8000-000000000099",
    )


def prepared() -> dispatch.PreparedRequest:
    """Prepared."""
    return prepare_request(
        context=context(),
        endpoint="deepseek",
        subcall="chat",
        method="POST",
        path="/chat/completions",
        body={"value": "private-body"},
        max_response_bytes=65536,
    )


class Hooks:
    """Explicit synthetic control acknowledgements; no persistence claim."""

    def __init__(self, clock: Clock) -> None:
        """Init."""
        self.clock = clock
        self.intents: list[Frame] = []
        self.observations: list[Frame] = []
        self.reports: list[Frame] = []
        self.settlement = "settled"
        self.authorize_entered = asyncio.Event()
        self.settle_entered = asyncio.Event()
        self.observe_entered = asyncio.Event()
        self.authorize_gate: asyncio.Event | None = None
        self.settle_gate: asyncio.Event | None = None
        self.observe_gate: asyncio.Event | None = None
        self.patch: dict[str, Any] = {}

    async def authorize(self, intent: Frame) -> Frame:
        """Authorize."""
        self.intents.append(copy.deepcopy(intent))
        self.authorize_entered.set()
        if self.authorize_gate is not None:
            await self.authorize_gate.wait()
        permit = copy.deepcopy(intent)
        permit.update(
            kind="call_permit",
            emitted_mono_ms=1000,
            physical_call_id="00000000-0000-4000-8000-000000000020",
            granted=True,
            error_code="",
            dispatch_ms=500,
            call_ms=2000,
            input_token_limit=2000,
            output_token_limit=1024,
        )
        permit.update(self.patch)
        return permit

    async def observe(self, observation: Frame) -> None:
        """Observe."""
        self.observations.append(copy.deepcopy(observation))
        self.observe_entered.set()
        if self.observe_gate is not None:
            await self.observe_gate.wait()

    async def settle(self, report: Frame) -> Frame:
        """Settle."""
        self.reports.append(copy.deepcopy(report))
        self.settle_entered.set()
        if self.settle_gate is not None:
            await self.settle_gate.wait()
        ack = {
            key: copy.deepcopy(report[key])
            for key in (
                "version",
                "request_id",
                "binding",
                "call_sequence",
                "physical_call_id",
            )
        }
        ack.update(
            kind="metering_ack",
            emitted_mono_ms=self.clock.now,
            usage_hash=report["usage"]["usage_hash"],
            settlement=self.settlement,
        )
        return ack


def make_dispatcher(
    monkeypatch: pytest.MonkeyPatch,
    server: HTTPFaultServer,
    clock: Clock,
    hooks: Hooks,
) -> AuthorizedDispatcher:
    """Make dispatcher."""
    monkeypatch.setattr(dispatch, "DEEPSEEK_ORIGIN", server.origin)
    return AuthorizedDispatcher(
        start_frame(),
        hooks=hooks,
        endpoints={"deepseek": Endpoint(server.origin, "dummy-credential")},
        clock=clock,
    )


async def good_response(
    reader: asyncio.StreamReader,
    writer: asyncio.StreamWriter,
    request: ReceivedRequest,
) -> None:
    """Good response."""
    await respond(writer, body=b'{"ok":true}')


def usage(
    response: CompleteResponse, output: int = 10, input_: int = 20
) -> UsageEvidence:
    """Usage."""
    return UsageEvidence(input_, output, 0, hashlib.sha256(response.body).hexdigest())


def valid(response: CompleteResponse) -> dict[str, Any]:
    """Valid."""
    return strict_json(response.body)


@run_async
async def test_exact_bytes_once_capture_and_confirm(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Exact bytes once capture and confirm."""
    clock, body = Clock(), {"value": "秘密"}
    hooks = Hooks(clock)
    request = prepare_request(
        context=context(),
        endpoint="deepseek",
        subcall="chat",
        method="POST",
        path="/chat/completions",
        body=body,
        max_response_bytes=65536,
    )
    body["value"] = "changed"
    async with HTTPFaultServer(good_response) as server:
        dispatcher = make_dispatcher(monkeypatch, server, clock, hooks)
        try:
            assert await dispatcher.execute(
                request, context=context(), validate=valid, extract_usage=usage
            ) == {"ok": True}
            assert len(server.requests) == 1
            assert server.requests[0].body == request.body
            assert (
                server.requests[0].headers["authorization"] == "Bearer dummy-credential"
            )
            assert hooks.intents[0]["parameter_hash"] == request.parameter_hash
            assert hooks.observations[0]["usage_disposition"] == "reported"
            assert hooks.observations[0]["business_outcome"] == "accepted"
            assert len(dispatcher.recorded_usage()) == 1
            with pytest.raises(DispatchError):
                await dispatcher.execute(request, context=context(), validate=valid)
            assert len(server.requests) == 1
        finally:
            await dispatcher.aclose()


@run_async
@pytest.mark.parametrize(
    "change",
    [
        {"emitted_mono_ms": 999},
        {"emitted_mono_ms": 1001},
        {"parameter_hash": "d" * 64},
        {"call_sequence": 2},
        {"physical_call_id": ""},
        {"dispatch_ms": 0},
    ],
)
async def test_bad_permit_never_sends(
    monkeypatch: pytest.MonkeyPatch,
    change: dict[str, Any],
) -> None:
    """Bad permit never sends."""
    clock = Clock()
    hooks = Hooks(clock)
    hooks.patch = change
    async with HTTPFaultServer(good_response) as server:
        dispatcher = make_dispatcher(monkeypatch, server, clock, hooks)
        try:
            with pytest.raises(DispatchError):
                await dispatcher.execute(prepared(), context=context(), validate=valid)
            assert server.requests == [] and dispatcher.closed
        finally:
            await dispatcher.aclose()


@run_async
@pytest.mark.parametrize("action", ["stop", "cancel", "expired_permit", "deadline"])
async def test_async_authorization_cannot_refresh_deadline_or_survive_stop(
    monkeypatch: pytest.MonkeyPatch,
    action: str,
) -> None:
    """Async authorization cannot refresh deadline or survive stop."""
    clock = Clock()
    hooks = Hooks(clock)
    hooks.authorize_gate = asyncio.Event()
    async with HTTPFaultServer(good_response) as server:
        dispatcher = make_dispatcher(monkeypatch, server, clock, hooks)
        task = asyncio.create_task(
            dispatcher.execute(prepared(), context=context(), validate=valid)
        )
        await hooks.authorize_entered.wait()
        if action == "stop":
            dispatcher.stop()
        elif action == "cancel":
            task.cancel()
        else:
            clock.now = 1500 if action == "expired_permit" else 11000
            hooks.authorize_gate.set()
        try:
            with pytest.raises((DispatchError, asyncio.CancelledError)):
                await asyncio.wait_for(task, 1)
            assert server.requests == [] and dispatcher.closed
        finally:
            await dispatcher.aclose()


@run_async
@pytest.mark.parametrize("status", [302, 429, 500])
async def test_complete_error_response_has_status_and_no_retry_or_redirect(
    monkeypatch: pytest.MonkeyPatch,
    status: int,
) -> None:
    """Complete error response has status and no retry or redirect."""

    async def handler(
        reader: asyncio.StreamReader,
        writer: asyncio.StreamWriter,
        request: ReceivedRequest,
    ) -> None:
        """Handler."""
        await respond(writer, status=status, headers={"Location": "/chat/completions"})

    clock = Clock()
    hooks = Hooks(clock)
    async with HTTPFaultServer(handler) as server:
        dispatcher = make_dispatcher(monkeypatch, server, clock, hooks)
        try:
            with pytest.raises(DispatchError):
                await dispatcher.execute(prepared(), context=context(), validate=valid)
            assert len(server.requests) == 1
            observation = hooks.observations[0]
            assert observation["transport_outcome"] == "response"
            assert observation["http_status"] == status
            assert observation["business_outcome"] == "rejected"
            assert observation["usage_disposition"] == "unknown"
        finally:
            await dispatcher.aclose()


@run_async
async def test_incomplete_body_preserves_unknown(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Incomplete body preserves unknown."""

    async def handler(
        reader: asyncio.StreamReader,
        writer: asyncio.StreamWriter,
        request: ReceivedRequest,
    ) -> None:
        """Handler."""
        writer.write(b'HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n{"ok":true}')
        await writer.drain()

    clock = Clock()
    hooks = Hooks(clock)
    async with HTTPFaultServer(handler) as server:
        dispatcher = make_dispatcher(monkeypatch, server, clock, hooks)
        try:
            with pytest.raises(DispatchError):
                await dispatcher.execute(
                    prepared(), context=context(), validate=valid, extract_usage=usage
                )
            assert dispatcher.recorded_usage() == ()
            assert hooks.observations[0]["transport_outcome"] == "unknown"
            assert hooks.observations[0]["http_status"] == 0
        finally:
            await dispatcher.aclose()


@run_async
@pytest.mark.parametrize("fact", ["", "size_limit"])
async def test_usage_survives_rejected_business_validation(
    monkeypatch: pytest.MonkeyPatch,
    fact: str,
) -> None:
    """Usage survives rejected business validation."""

    def rejected(response: CompleteResponse) -> None:
        """Rejected."""
        raise DispatchError("OUTPUT_INVALID", fact=fact)

    clock = Clock()
    hooks = Hooks(clock)
    async with HTTPFaultServer(good_response) as server:
        dispatcher = make_dispatcher(monkeypatch, server, clock, hooks)
        try:
            with pytest.raises(DispatchError) as error:
                await dispatcher.execute(
                    prepared(),
                    context=context(),
                    validate=rejected,
                    extract_usage=usage,
                )
            assert error.value.fact == fact
            assert len(hooks.reports) == len(dispatcher.recorded_usage()) == 1
            if fact == "size_limit":
                assert hooks.observations == []
            else:
                assert hooks.observations[0]["business_outcome"] == "rejected"
                assert hooks.observations[0]["usage_disposition"] == "reported"
        finally:
            await dispatcher.aclose()


@run_async
@pytest.mark.parametrize("counts", [(20, 1025), (MAX_INTEGER, 1)])
@pytest.mark.parametrize("ack", ["anomaly", "settled", "unconfirmed"])
async def test_out_of_reservation_usage_is_preserved_and_never_reopens(
    monkeypatch: pytest.MonkeyPatch,
    counts: tuple[int, int],
    ack: str,
) -> None:
    """Out of reservation usage is preserved and never reopens."""
    clock = Clock()
    hooks = Hooks(clock)
    hooks.settlement = ack
    async with HTTPFaultServer(good_response) as server:
        dispatcher = make_dispatcher(monkeypatch, server, clock, hooks)
        try:
            with pytest.raises(DispatchError):
                await dispatcher.execute(
                    prepared(),
                    context=context(),
                    validate=valid,
                    extract_usage=lambda r: usage(r, counts[1], counts[0]),
                )
            assert dispatcher.closed
            assert hooks.observations == []
            assert hooks.reports[0]["usage"]["input_tokens"] == counts[0]
            assert hooks.reports[0]["usage"]["output_tokens"] == counts[1]
            assert len(dispatcher.recorded_usage()) == 1
            with pytest.raises(DispatchError):
                await dispatcher.execute(prepared(), context=context(), validate=valid)
            assert len(server.requests) == 1
        finally:
            await dispatcher.aclose()


@run_async
@pytest.mark.parametrize("phase", ["read", "settle", "observe"])
@pytest.mark.parametrize("action", ["stop", "cancel", "deadline"])
async def test_cancel_and_deadline_join_owned_work_preserving_captured_usage(
    monkeypatch: pytest.MonkeyPatch,
    phase: str,
    action: str,
) -> None:
    """Cancel and deadline join owned work preserving captured usage."""
    read_entered, disconnected = asyncio.Event(), asyncio.Event()

    async def handler(
        reader: asyncio.StreamReader,
        writer: asyncio.StreamWriter,
        request: ReceivedRequest,
    ) -> None:
        """Handler."""
        if phase == "read":
            writer.write(b"HTTP/1.1 200 OK\r\nContent-Length: 999\r\n\r\n{")
            await writer.drain()
            read_entered.set()
            await reader.read()
            disconnected.set()
        else:
            await respond(writer)

    clock = Clock()
    hooks = Hooks(clock)
    hooks.settle_gate = asyncio.Event() if phase == "settle" else None
    hooks.observe_gate = asyncio.Event() if phase == "observe" else None
    baseline = set(asyncio.all_tasks())
    async with HTTPFaultServer(handler) as server:
        dispatcher = make_dispatcher(monkeypatch, server, clock, hooks)
        task = asyncio.create_task(
            dispatcher.execute(
                prepared(), context=context(), validate=valid, extract_usage=usage
            )
        )
        barrier = {
            "read": read_entered,
            "settle": hooks.settle_entered,
            "observe": hooks.observe_entered,
        }[phase]
        await asyncio.wait_for(barrier.wait(), 2)
        if action == "stop":
            dispatcher.stop()
        elif action == "cancel":
            task.cancel()
        else:
            clock.now = 3000
        with pytest.raises((DispatchError, asyncio.CancelledError)):
            await asyncio.wait_for(task, 1)
        assert dispatcher.closed
        assert len(dispatcher.recorded_usage()) == (0 if phase == "read" else 1)
        if phase == "read":
            await asyncio.wait_for(disconnected.wait(), 1)
        await dispatcher.aclose()
    assert not set(asyncio.all_tasks()) - baseline


@run_async
async def test_concurrency_rejects_without_queue(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Concurrency rejects without queue."""
    clock = Clock()
    hooks = Hooks(clock)
    hooks.authorize_gate = asyncio.Event()
    async with HTTPFaultServer(good_response) as server:
        dispatcher = make_dispatcher(monkeypatch, server, clock, hooks)
        task = asyncio.create_task(
            dispatcher.execute(prepared(), context=context(), validate=valid)
        )
        await hooks.authorize_entered.wait()
        with pytest.raises(DispatchError, match="CALL_CONFLICT"):
            await dispatcher.execute(prepared(), context=context(), validate=valid)
        assert len(hooks.intents) == 1
        hooks.authorize_gate.set()
        await task
        await dispatcher.aclose()
        assert len(server.requests) == 1


@run_async
@pytest.mark.parametrize("stage", ["validator", "extractor"])
async def test_sync_clock_jump_preserves_usage_but_never_accepts(
    monkeypatch: pytest.MonkeyPatch,
    stage: str,
) -> None:
    """Sync clock jump preserves usage but never accepts."""
    clock = Clock()
    hooks = Hooks(clock)

    def extract(response: CompleteResponse) -> UsageEvidence:
        """Extract."""
        if stage == "extractor":
            clock.now = 3000
        return usage(response)

    def validate(response: CompleteResponse) -> dict[str, Any]:
        """Validate."""
        if stage == "validator":
            clock.now = 3000
        return valid(response)

    async with HTTPFaultServer(good_response) as server:
        dispatcher = make_dispatcher(monkeypatch, server, clock, hooks)
        try:
            with pytest.raises(DispatchError):
                await dispatcher.execute(
                    prepared(),
                    context=context(),
                    validate=validate,
                    extract_usage=extract,
                )
            assert len(dispatcher.recorded_usage()) == 1
            assert hooks.observations == []
        finally:
            await dispatcher.aclose()


@run_async
async def test_payload_forgery_and_wrong_context_never_send(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Payload forgery and wrong context never send."""
    clock = Clock()
    hooks = Hooks(clock)
    async with HTTPFaultServer(good_response) as server:
        dispatcher = make_dispatcher(monkeypatch, server, clock, hooks)
        try:
            with pytest.raises(DispatchError):
                await dispatcher.execute(
                    replace(prepared(), body=b"forged"),
                    context=context(),
                    validate=valid,
                )
            assert not hooks.intents and not server.requests
        finally:
            await dispatcher.aclose()


@run_async
@pytest.mark.parametrize(
    "raw",
    [
        b"HTTP/1.1 200 OK\r\nContent-Length: 65537\r\n\r\n",
        b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n10001\r\n"
        + b"x" * 65537
        + b"\r\n0\r\n\r\n",
    ],
    ids=["content_length", "chunked"],
)
async def test_body_size_is_typed_and_never_corrective_output(
    monkeypatch: pytest.MonkeyPatch,
    raw: bytes,
) -> None:
    """Body size is typed and never corrective output."""

    async def handler(
        reader: asyncio.StreamReader,
        writer: asyncio.StreamWriter,
        request: ReceivedRequest,
    ) -> None:
        """Handler."""
        writer.write(raw)
        await writer.drain()

    clock = Clock()
    hooks = Hooks(clock)
    async with HTTPFaultServer(handler) as server:
        dispatcher = make_dispatcher(monkeypatch, server, clock, hooks)
        try:
            with pytest.raises(DispatchError) as error:
                await dispatcher.execute(prepared(), context=context(), validate=valid)
            assert error.value.fact == "size_limit"
            assert hooks.observations == [] and dispatcher.recorded_usage() == ()
        finally:
            await dispatcher.aclose()


def test_no_hooks_no_network_and_credentials_and_bodies_hidden() -> None:
    """No hooks no network and credentials and bodies hidden."""
    with pytest.raises(DispatchError):
        AuthorizedDispatcher(start_frame(), hooks=None, endpoints={}, clock=Clock())  # type: ignore[arg-type]
    assert "private-body" not in repr(prepared())
    assert "dummy-credential" not in repr(
        Endpoint("https://api.deepseek.com", "dummy-credential")
    )
    assert "private-body" not in repr(
        CompleteResponse(200, b"private-body", "id", "hash")
    )


@pytest.mark.parametrize(
    "body",
    [
        b'{"x":1,"x":2}',
        b'{"x":"\\ud800"}',
        b'{"x":1e400}',
        b'{"x":' + b"9" * 400 + b"}",
        b"[" * 66 + b"0" + b"]" * 66,
        b"{} sensitive-trailing",
        b'{"x":"\xff"}',
    ],
)
def test_strict_json_rejects_ambiguous_or_unbounded_bodies(body: bytes) -> None:
    """Strict json rejects ambiguous or unbounded bodies."""
    with pytest.raises(ToolError, match="^INVALID_JSON$") as error:
        strict_json(body)
    assert error.value.__cause__ is None
    assert error.value.__context__ is None


@run_async
async def test_trace_is_validated_and_forwarded_without_changing_hash(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Transmit the trusted W3C parent outside the immutable business fingerprint."""
    clock = Clock()
    hooks = Hooks(clock)
    frame = start_frame()
    parent = "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
    frame["trace_context"] = parent
    async with HTTPFaultServer(good_response) as server:
        monkeypatch.setattr(dispatch, "DEEPSEEK_ORIGIN", server.origin)
        dispatcher = AuthorizedDispatcher(
            frame,
            hooks=hooks,
            endpoints={"deepseek": Endpoint(server.origin)},
            clock=clock,
        )
        request = prepared()
        try:
            await dispatcher.execute(request, context=context(), validate=valid)
            assert server.requests[0].headers["traceparent"] == parent
            assert hooks.intents[0]["parameter_hash"] == request.parameter_hash
        finally:
            await dispatcher.aclose()
    frame["trace_context"] = "00-" + "0" * 32 + "-0123456789abcdef-01"
    with pytest.raises(DispatchError):
        AuthorizedDispatcher(frame, hooks=hooks, endpoints={}, clock=clock)


@run_async
@pytest.mark.parametrize("failure_at", ["authorize", "observe", "settle", "validator"])
async def test_callback_failure_drops_secret_exception_context_and_stops(
    monkeypatch: pytest.MonkeyPatch,
    failure_at: str,
) -> None:
    """No callback or HTTP request exception object escapes the dispatch boundary."""
    clock = Clock()
    hooks = Hooks(clock)

    def fail() -> None:
        raise httpx.ReadError(
            "dummy-sensitive-provider-error",
            request=httpx.Request(
                "GET",
                "https://invalid.example",
                headers={"Authorization": "Bearer dummy-sensitive-credential"},
            ),
        )

    async def fail_hook(frame: Frame) -> Any:
        fail()

    def validator(response: CompleteResponse) -> dict[str, Any]:
        if failure_at == "validator":
            fail()
        return valid(response)

    if failure_at != "validator":
        monkeypatch.setattr(hooks, failure_at, fail_hook)
    async with HTTPFaultServer(good_response) as server:
        dispatcher = make_dispatcher(monkeypatch, server, clock, hooks)
        try:
            with pytest.raises(DispatchError) as error:
                await dispatcher.execute(
                    prepared(),
                    context=context(),
                    validate=validator,
                    extract_usage=usage,
                )
            assert error.value.__context__ is error.value.__cause__ is None
            assert "dummy-sensitive" not in repr(error.value)
            if failure_at == "validator":
                # A complete rejected observation was confirmed; C1 remains
                # blocked so the owner may close it with an error step result.
                assert not dispatcher.closed
            else:
                assert dispatcher.closed
            assert len(dispatcher.recorded_usage()) == (failure_at != "authorize")
        finally:
            await dispatcher.aclose()


@run_async
@pytest.mark.parametrize("confirmed", [True, False])
@pytest.mark.parametrize("followup", ["error_result", "success_result", "http"])
async def test_confirmed_rejection_retains_only_c1_error_result_path(
    monkeypatch: pytest.MonkeyPatch,
    confirmed: bool,
    followup: str,
) -> None:
    """Keep C1 blocked after confirmed rejection; uncertainty closes all authority."""
    clock = Clock()
    hooks = Hooks(clock)

    def rejected(response: CompleteResponse) -> None:
        raise DispatchError("OUTPUT_INVALID")

    async def unconfirmed(observation: Frame) -> None:
        hooks.observations.append(observation)
        raise OSError("dummy-unconfirmed-observation")

    if not confirmed:
        monkeypatch.setattr(hooks, "observe", unconfirmed)
    async with HTTPFaultServer(good_response) as server:
        dispatcher = make_dispatcher(monkeypatch, server, clock, hooks)
        try:
            with pytest.raises(DispatchError):
                await dispatcher.execute(
                    prepared(),
                    context=context(),
                    validate=rejected,
                    extract_usage=usage,
                )
            assert dispatcher.closed is not confirmed
            if followup == "http":
                with pytest.raises(DispatchError):
                    await dispatcher.execute(
                        prepared(), context=context(), validate=valid
                    )
            else:
                result = {
                    key: copy.deepcopy(start_frame()[key])
                    for key in ("version", "request_id", "binding", "emitted_mono_ms")
                }
                error_result = followup == "error_result"
                result.update(
                    kind="step_result",
                    outcome="error" if error_result else "success",
                    error_code="OUTPUT_INVALID" if error_result else "",
                    result={"correction_required": True, "proposal": None}
                    if error_result
                    else {},
                )
                # This is the very Conversation owned by the dispatcher, not a
                # parallel guard granting authority after a local stop.
                if confirmed and error_result:
                    dispatcher._conversation.accept(result, clock.now)
                else:
                    with pytest.raises(ProtocolError):
                        dispatcher._conversation.accept(result, clock.now)
            assert len(server.requests) == len(hooks.intents) == 1
        finally:
            await dispatcher.aclose()


@run_async
@pytest.mark.parametrize("settlement", ["unconfirmed", "anomaly", "wrong_hash"])
async def test_unconfirmed_or_wrong_ack_never_emits_accepted(
    monkeypatch: pytest.MonkeyPatch,
    settlement: str,
) -> None:
    """Known usage cannot authorize output before same-call settled confirmation."""
    clock = Clock()
    hooks = Hooks(clock)
    if settlement == "wrong_hash":
        original = hooks.settle

        async def bad_ack(report: Frame) -> Frame:
            ack = await original(report)
            ack["usage_hash"] = "d" * 64
            return ack

        monkeypatch.setattr(hooks, "settle", bad_ack)
    else:
        hooks.settlement = settlement
    async with HTTPFaultServer(good_response) as server:
        dispatcher = make_dispatcher(monkeypatch, server, clock, hooks)
        try:
            with pytest.raises(DispatchError):
                await dispatcher.execute(
                    prepared(), context=context(), validate=valid, extract_usage=usage
                )
            assert hooks.observations == [] and dispatcher.closed
            reports = dispatcher.recorded_usage()
            reports[0]["usage"]["input_tokens"] = 0
            assert dispatcher.recorded_usage()[0]["usage"]["input_tokens"] == 20
        finally:
            await dispatcher.aclose()


@run_async
async def test_complete_usage_captured_before_bounded_response_close(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """A stuck cleanup cannot discard acquired usage or create detached work."""
    original_close = httpx.Response.aclose
    blocked = asyncio.Event()

    async def slow_close(response: httpx.Response) -> None:
        await original_close(response)
        await blocked.wait()

    clock = Clock()
    hooks = Hooks(clock)
    async with HTTPFaultServer(good_response) as server:
        dispatcher = make_dispatcher(monkeypatch, server, clock, hooks)
        monkeypatch.setattr(httpx.Response, "aclose", slow_close)
        try:
            with pytest.raises(DispatchError):
                await asyncio.wait_for(
                    dispatcher.execute(
                        prepared(),
                        context=context(),
                        validate=valid,
                        extract_usage=usage,
                    ),
                    1,
                )
            assert len(dispatcher.recorded_usage()) == len(hooks.reports) == 1
            assert hooks.observations[0]["business_outcome"] == "rejected"
        finally:
            await dispatcher.aclose()
