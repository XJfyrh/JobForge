"""Single-permit HTTP execution using the existing v2 conversation authority."""

from __future__ import annotations

import asyncio
import copy
import hashlib
import json
import re
import time
from collections.abc import Awaitable, Callable, Mapping
from dataclasses import dataclass, field
from typing import Any, Literal, Protocol, TypeVar, cast

import httpx
from opentelemetry.trace import get_current_span
from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator

from jobforge_agent.embedding import LOCAL_ORIGINS
from jobforge_agent.errors import ToolError
from jobforge_agent.http import origin, strict_json
from jobforge_agent.protocol_v2 import (
    MAX_INTEGER,
    Conversation,
    Frame,
    ProtocolError,
    usage_hash,
)

EndpointAlias = Literal["business", "ollama", "deepseek"]
DEEPSEEK_ORIGIN = "https://api.deepseek.com"
OLLAMA_ORIGINS = LOCAL_ORIGINS
_HASH = re.compile(r"[0-9a-f]{64}")
_UUID = r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"
_ERRORS = {
    "BUDGET_EXHAUSTED",
    "STOP_REQUESTED",
    "STALE_LEASE",
    "PROFILE_UNAVAILABLE",
    "DEPENDENCY_UNAVAILABLE",
    "PROTOCOL_ERROR",
    "CALL_CONFLICT",
    "OUTPUT_INVALID",
    "INPUT_INVALID",
    "TIMEOUT",
}
T = TypeVar("T")


class DispatchError(Exception):
    """Expose a fixed failure and an internal fact without provider content."""

    def __init__(self, code: str, *, fact: str = "", stop: bool = False) -> None:
        """Keep size-limit failures distinct from correctable model output."""
        self.code = code if code in _ERRORS else "PROTOCOL_ERROR"
        self.fact = fact if fact in {"", "size_limit", "identity"} else ""
        self.stop = stop
        super().__init__(self.code)


@dataclass(frozen=True)
class RunCallContext:
    """Carry Run hashes, distinct from the business index profile hash."""

    run_profile_hash: str
    snapshot_content_hash: str
    tool_invocation_id: str


@dataclass(frozen=True)
class PreparedRequest:
    """Carry the exact immutable bytes whose fingerprint receives permission."""

    endpoint: EndpointAlias
    subcall: str
    method: Literal["GET", "POST"]
    path: str
    body: bytes = field(repr=False)
    max_response_bytes: int
    parameter_hash: str


@dataclass(frozen=True)
class CompleteResponse:
    """Represent an entirely received bounded body, even for HTTP errors."""

    status_code: int
    body: bytes = field(repr=False)
    physical_call_id: str
    parameter_hash: str


@dataclass(frozen=True)
class UsageEvidence:
    """Keep observed counts without truncation to the reservation."""

    input_tokens: int
    output_tokens: int
    cached_input_tokens: int
    receipt_hash: str
    provider_identity: str = ""
    reasoning_tokens: int | None = None


@dataclass(frozen=True)
class CapturedUsageAudit:
    """Retain bounded live-process response identity, never provider content."""

    physical_call_id: str
    receipt_hash: str
    provider_identity: str
    reasoning_tokens: int | None


@dataclass(frozen=True)
class Endpoint:
    """Contain deployment configuration, never model-supplied credentials."""

    base_origin: str
    bearer_key: str | None = field(default=None, repr=False)


class DispatchHooks(Protocol):
    """Require explicit async control confirmations before any further work."""

    async def authorize(self, intent: Frame) -> Frame:
        """Return the newly issued v2 permit for this exact intent."""
        ...

    async def observe(self, observation: Frame) -> None:
        """Return only when the ordinary observation is confirmed."""
        ...

    async def settle(self, report: Frame) -> Frame:
        """Return the original call's v2 metering acknowledgement."""
        ...


def boottime_ms() -> int:
    """Read the Linux suspend-aware clock; never substitute a wall clock."""
    clock_id = getattr(time, "CLOCK_BOOTTIME", None)
    read_clock = getattr(time, "clock_gettime_ns", None)
    if clock_id is None or read_clock is None:
        raise DispatchError("PROFILE_UNAVAILABLE")
    return int(read_clock(clock_id) // 1_000_000)


def _fingerprint(*values: str) -> str:
    digest = hashlib.sha256()
    for value in values:
        raw = value.encode("utf-8")
        digest.update(len(raw).to_bytes(8, "big"))
        digest.update(raw)
    return digest.hexdigest()


def _shape(request: PreparedRequest) -> None:
    fixed = {
        "profile_version": ("ollama", "GET", "/api/version", 1024),
        "profile_tags": ("ollama", "GET", "/api/tags", 65536),
        "query_embedding": ("ollama", "POST", "/api/embed", 262144),
        "chat": ("deepseek", "POST", "/chat/completions", 65536),
    }
    if request.subcall in fixed:
        endpoint, method, path, cap = fixed[request.subcall]
        valid = (request.endpoint, request.method, request.path) == (
            endpoint,
            method,
            path,
        )
    elif request.subcall in {"get_order", "get_delivery", "search_policy"}:
        suffix = {
            "get_order": "order",
            "get_delivery": "delivery",
            "search_policy": "policies/search",
        }[request.subcall]
        method = "POST" if request.subcall == "search_policy" else "GET"
        valid = (
            request.endpoint == "business"
            and request.method == method
            and re.fullmatch(rf"/business/v1/snapshots/{_UUID}/{suffix}", request.path)
            is not None
        )
        cap = 8192
    else:
        raise DispatchError("INPUT_INVALID")
    if (
        not valid
        or type(request.body) is not bytes
        or type(request.max_response_bytes) is not int
        or not 0 < request.max_response_bytes <= cap
        or (request.method == "GET" and request.body != b"")
    ):
        raise DispatchError("INPUT_INVALID")
    if len(request.body) > 65536:
        raise DispatchError("INPUT_INVALID", fact="size_limit", stop=True)


def _parameter_hash(request: PreparedRequest, context: RunCallContext) -> str:
    if any(
        not isinstance(value, str) or _HASH.fullmatch(value) is None
        for value in (context.run_profile_hash, context.snapshot_content_hash)
    ):
        raise DispatchError("INPUT_INVALID")
    return _fingerprint(
        "jobforge.run.physical-input.v1",
        context.run_profile_hash,
        context.snapshot_content_hash,
        request.subcall,
        request.method,
        request.path,
        hashlib.sha256(request.body).hexdigest(),
    )


def prepare_request(
    *,
    context: RunCallContext,
    endpoint: EndpointAlias,
    subcall: str,
    method: Literal["GET", "POST"],
    path: str,
    body: dict[str, Any] | None = None,
    max_response_bytes: int,
) -> PreparedRequest:
    """Serialize once; subsequent sends use precisely these immutable bytes."""
    failed = False
    try:
        raw = (
            b""
            if body is None
            else json.dumps(
                body, ensure_ascii=False, allow_nan=False, separators=(",", ":")
            ).encode("utf-8")
        )
        if raw:
            strict_json(raw)
    except (ValueError, TypeError, UnicodeError, RecursionError, ToolError):
        failed = True
    if failed:
        raise DispatchError("INPUT_INVALID")
    request = PreparedRequest(
        endpoint, subcall, method, path, raw, max_response_bytes, ""
    )
    _shape(request)
    return PreparedRequest(
        endpoint,
        subcall,
        method,
        path,
        raw,
        max_response_bytes,
        _parameter_hash(request, context),
    )


class AuthorizedDispatcher:
    """Own one step's HTTP resources and delegate all permission to v2."""

    def __init__(
        self,
        execute_step: Frame,
        *,
        hooks: DispatchHooks,
        endpoints: Mapping[EndpointAlias, Endpoint],
        clock: Callable[[], int] = boottime_ms,
    ) -> None:
        """Bind the original frame without an unauthorised fallback path."""
        if hooks is None or any(
            not callable(getattr(hooks, name, None))
            for name in ("authorize", "observe", "settle")
        ):
            raise DispatchError("INPUT_INVALID")
        self._clock = clock
        self._last_now = -1
        self._conversation = Conversation()
        self._start = copy.deepcopy(execute_step)
        try:
            self._conversation.accept(self._start, self._now())
        except ProtocolError:
            raise DispatchError("PROTOCOL_ERROR") from None
        self._hooks = hooks
        self._traceparent = self._start["trace_context"]
        if self._traceparent:
            extracted = TraceContextTextMapPropagator().extract(
                {"traceparent": self._traceparent}
            )
            if not get_current_span(extracted).get_span_context().is_valid:
                raise DispatchError("PROTOCOL_ERROR")
        self._endpoints: dict[EndpointAlias, Endpoint] = {}
        for alias, endpoint in endpoints.items():
            try:
                base = origin(endpoint.base_origin)
            except ToolError:
                raise DispatchError("INPUT_INVALID") from None
            if (
                alias not in {"business", "ollama", "deepseek"}
                or (alias == "deepseek" and base != DEEPSEEK_ORIGIN)
                or (alias == "ollama" and base not in OLLAMA_ORIGINS)
            ):
                raise DispatchError("INPUT_INVALID")
            key = endpoint.bearer_key
            if key is not None and (
                not key
                or len(key) > 4096
                or not key.isascii()
                or any(ord(char) <= 32 or ord(char) == 127 for char in key)
            ):
                raise DispatchError("INPUT_INVALID")
            self._endpoints[alias] = Endpoint(base, key)
        self._clients: dict[EndpointAlias, httpx.AsyncClient] = {}
        self._busy = False
        self._stopped = asyncio.Event()
        self._sequence = 0
        self._reports: list[Frame] = []
        self._audits: list[CapturedUsageAudit] = []
        self._owner: asyncio.Task[Any] | None = None

    @property
    def closed(self) -> bool:
        """Expose the existing protocol's irreversible authority closure."""
        return self._conversation.closed

    def stop(self) -> None:
        """Stop authority immediately and wake the request-owned watcher."""
        self._conversation.stop()
        self._stopped.set()

    def recorded_usage(self) -> tuple[Frame, ...]:
        """Copy bounded already-complete reports, including after cancellation."""
        return tuple(copy.deepcopy(self._reports))

    def recorded_audit(self) -> tuple[CapturedUsageAudit, ...]:
        """Read immutable identity/count audit facts beside the captured reports."""
        return tuple(self._audits)

    def _now(self) -> int:
        now = self._clock()
        if type(now) is not int or not self._last_now <= now <= MAX_INTEGER or now < 0:
            self._conversation.stop()
            raise DispatchError("PROTOCOL_ERROR")
        self._last_now = now
        return now

    def _guard(self, deadline: int, *, metering: bool = False) -> int:
        now = self._now()
        if self._stopped.is_set() or (self.closed and not metering):
            raise DispatchError("STOP_REQUESTED")
        if now >= deadline:
            self.stop()
            raise DispatchError("TIMEOUT")
        return now

    def _frame(self, kind: str, **values: Any) -> Frame:
        return {
            "version": 2,
            "kind": kind,
            "request_id": self._start["request_id"],
            "binding": copy.deepcopy(self._start["binding"]),
            "emitted_mono_ms": self._now(),
            **values,
        }

    async def _bounded(
        self,
        operation: Awaitable[T],
        deadline: int,
        *,
        metering: bool = False,
    ) -> T:
        async def watch() -> None:
            while True:
                now = self._guard(deadline, metering=metering)
                try:
                    await asyncio.wait_for(
                        self._stopped.wait(), timeout=min(0.05, (deadline - now) / 1000)
                    )
                except TimeoutError:
                    continue

        task = asyncio.ensure_future(operation)
        watcher = asyncio.create_task(watch())
        try:
            await asyncio.wait({task, watcher}, return_when=asyncio.FIRST_COMPLETED)
            if watcher.done():
                await watcher
            value = await task
            self._guard(deadline, metering=metering)
            return value
        finally:
            for pending in (task, watcher):
                if not pending.done():
                    pending.cancel()
            await asyncio.gather(task, watcher, return_exceptions=True)

    def _client(self, endpoint: EndpointAlias) -> httpx.AsyncClient:
        if endpoint not in self._clients:
            settings = self._endpoints.get(endpoint)
            if settings is None:
                raise DispatchError("INPUT_INVALID")
            headers = {"Accept": "application/json", "Accept-Encoding": "identity"}
            if settings.bearer_key is not None:
                headers["Authorization"] = f"Bearer {settings.bearer_key}"
            self._clients[endpoint] = httpx.AsyncClient(
                headers=headers,
                follow_redirects=False,
                trust_env=False,
                timeout=httpx.Timeout(60, connect=5),
                transport=httpx.AsyncHTTPTransport(retries=0, trust_env=False),
                limits=httpx.Limits(max_connections=1, max_keepalive_connections=1),
            )
        return self._clients[endpoint]

    def _capture(
        self,
        evidence: UsageEvidence,
        permit: Frame,
        request: PreparedRequest,
    ) -> Frame:
        usage: Frame = {
            "input_tokens": evidence.input_tokens,
            "output_tokens": evidence.output_tokens,
            "cached_input_tokens": evidence.cached_input_tokens,
            "receipt_hash": evidence.receipt_hash,
        }
        usage["usage_hash"] = usage_hash(usage)
        report = self._frame(
            "metering_report",
            call_sequence=permit["call_sequence"],
            physical_call_id=permit["physical_call_id"],
            parameter_hash=request.parameter_hash,
            usage=usage,
        )
        self._conversation.accept_metering(report, self._now())
        self._reports.append(copy.deepcopy(report))
        # Preserve independently valid usage even if a trusted adapter supplies
        # malformed optional metadata; it must never replace the original report.
        reasoning = evidence.reasoning_tokens
        try:
            valid_identity = (
                isinstance(evidence.provider_identity, str)
                and len(evidence.provider_identity.encode("utf-8")) <= 512
            )
        except UnicodeError:
            valid_identity = False
        if not valid_identity or (
            reasoning is not None
            and (
                type(reasoning) is not int
                or not 0 <= reasoning <= evidence.output_tokens
            )
        ):
            self._conversation.stop()
            raise DispatchError("PROTOCOL_ERROR")
        self._audits.append(
            CapturedUsageAudit(
                permit["physical_call_id"],
                evidence.receipt_hash,
                evidence.provider_identity,
                reasoning,
            )
        )
        return report

    async def _send(
        self,
        request: PreparedRequest,
        permit: Frame,
        deadline: int,
        extract_usage: Callable[[CompleteResponse], UsageEvidence | None] | None,
        captured: list[Frame],
        received: list[CompleteResponse],
    ) -> CompleteResponse:
        client = self._client(request.endpoint)
        headers = {"Content-Type": "application/json"} if request.body else {}
        if self._traceparent:
            headers["traceparent"] = self._traceparent
        outbound = client.build_request(
            request.method,
            self._endpoints[request.endpoint].base_origin + request.path,
            content=request.body,
            headers=headers,
        )
        now = self._guard(deadline)
        self._conversation.can_dispatch(permit["physical_call_id"], now)
        response = await client.send(outbound, stream=True)
        try:
            if response.headers.get("content-encoding", "identity") != "identity":
                raise DispatchError("PROTOCOL_ERROR")
            length = response.headers.get("content-length")
            if length is not None:
                if not length.isascii() or not length.isdecimal():
                    raise DispatchError("PROTOCOL_ERROR")
                if int(length) > request.max_response_bytes:
                    raise DispatchError("OUTPUT_INVALID", fact="size_limit", stop=True)
            data = bytearray()
            # Reading the raw stream directly allows capture before an awaited
            # response.aclose(), including its cancellation/failure path.
            assert isinstance(response.stream, httpx.AsyncByteStream)
            async for chunk in response.stream:
                if len(data) + len(chunk) > request.max_response_bytes:
                    raise DispatchError("OUTPUT_INVALID", fact="size_limit", stop=True)
                data.extend(chunk)
            complete = CompleteResponse(
                response.status_code,
                bytes(data),
                permit["physical_call_id"],
                request.parameter_hash,
            )
            received.append(complete)
            if extract_usage is not None:
                evidence = extract_usage(complete)
                if evidence is not None:
                    captured.append(self._capture(evidence, permit, request))
            return complete
        finally:
            # Resource cleanup has a fixed ceiling and never spawns detached work.
            async with asyncio.timeout(0.1):
                await response.aclose()

    async def execute(
        self,
        request: PreparedRequest,
        *,
        context: RunCallContext,
        validate: Callable[[CompleteResponse], T],
        extract_usage: Callable[[CompleteResponse], UsageEvidence | None] | None = None,
    ) -> T:
        """Authorize exactly once and return only after validation/confirmations."""
        if self._busy:
            raise DispatchError("CALL_CONFLICT")
        self._busy = True
        self._owner = asyncio.current_task()
        failure: DispatchError
        try:
            value, rejected = await self._execute(
                request, context, validate, extract_usage
            )
            if rejected is None:
                return cast(T, value)
            # The existing Conversation is now blocked, not closed: it rejects
            # further calls/success while retaining its error-result path. This
            # is a confirmed observation fact, not a second permission state.
            failure = DispatchError(
                rejected.code, fact=rejected.fact, stop=rejected.stop
            )
        except asyncio.CancelledError:
            self.stop()
            raise
        except ProtocolError:
            self.stop()
            failure = DispatchError("PROTOCOL_ERROR")
        except DispatchError as error:
            self.stop()
            failure = DispatchError(error.code, fact=error.fact, stop=error.stop)
        except (httpx.HTTPError, OSError, TimeoutError):
            self.stop()
            failure = DispatchError("DEPENDENCY_UNAVAILABLE")
        except Exception:
            # A trusted callback programming failure is terminal; provider text
            # and arbitrary callback messages must not become protocol errors.
            self.stop()
            failure = DispatchError("PROTOCOL_ERROR")
        finally:
            self._busy = False
            self._owner = None
        # Do not retain HTTPX Request/Authorization or provider parser exceptions
        # via __context__; suppressing their display with `from None` is weaker.
        raise failure

    async def _execute(
        self,
        request: PreparedRequest,
        context: RunCallContext,
        validate: Callable[[CompleteResponse], T],
        extract_usage: Callable[[CompleteResponse], UsageEvidence | None] | None,
    ) -> tuple[T | None, DispatchError | None]:
        deadline = self._conversation.deadline
        self._guard(deadline)
        _shape(request)
        binding = self._start["binding"]
        if (
            context.run_profile_hash != binding["profile_hash"]
            or context.snapshot_content_hash != binding["snapshot_hash"]
            or request.parameter_hash != _parameter_hash(request, context)
            or request.endpoint not in self._endpoints
            or (
                request.endpoint == "business"
                and not request.path.startswith(
                    f"/business/v1/snapshots/{binding['snapshot_id']}/"
                )
            )
        ):
            raise DispatchError("PROTOCOL_ERROR")
        self._sequence += 1
        intent = self._frame(
            "call_intent",
            call_sequence=self._sequence,
            subcall=request.subcall,
            parameter_hash=request.parameter_hash,
            tool_invocation_id=context.tool_invocation_id,
        )
        self._conversation.accept(intent, self._now())
        permit = await self._bounded(
            self._hooks.authorize(copy.deepcopy(intent)), deadline
        )
        self._conversation.accept(permit, self._now())
        if not permit["granted"]:
            raise DispatchError(permit["error_code"])
        deadline = min(deadline, permit["emitted_mono_ms"] + permit["call_ms"])
        captured: list[Frame] = []
        received: list[CompleteResponse] = []
        complete: CompleteResponse | None = None
        failure: DispatchError | None = None
        value: T | None = None
        try:
            complete = await self._bounded(
                self._send(
                    request, permit, deadline, extract_usage, captured, received
                ),
                deadline,
                metering=True,
            )
            self._guard(deadline, metering=True)
            value = validate(complete)
            if not 200 <= complete.status_code < 300:
                raise DispatchError("DEPENDENCY_UNAVAILABLE")
        except DispatchError as error:
            failure = error
        except (httpx.HTTPError, OSError, TimeoutError):
            failure = DispatchError("DEPENDENCY_UNAVAILABLE")
        if complete is None and received:
            complete = received[0]
        if failure is not None and self._stopped.is_set():
            raise failure
        self._guard(deadline, metering=True)
        if failure is not None and (failure.stop or failure.fact == "size_limit"):
            self._conversation.stop()
        if captured:
            ack = await self._bounded(
                self._hooks.settle(copy.deepcopy(captured[0])), deadline, metering=True
            )
            self._conversation.accept_metering(ack, self._now())
        if self.closed:
            raise failure or DispatchError("STOP_REQUESTED")
        observation = self._frame(
            "call_observation",
            call_sequence=permit["call_sequence"],
            physical_call_id=permit["physical_call_id"],
            transport_outcome="response" if complete is not None else "unknown",
            http_status=complete.status_code if complete is not None else 0,
            business_outcome=(
                "unknown" if complete is None else "rejected" if failure else "accepted"
            ),
            error_code=failure.code if failure else "",
            usage_disposition="reported" if captured else "unknown",
            usage_hash=captured[0]["usage"]["usage_hash"] if captured else None,
        )
        self._guard(deadline)
        self._conversation.accept(observation, self._now())
        await self._bounded(self._hooks.observe(copy.deepcopy(observation)), deadline)
        if failure is not None and complete is None:
            raise failure
        return value, failure

    async def aclose(self) -> None:
        """Stop and join the active call before bounded client cleanup."""
        self.stop()
        owner = self._owner
        if owner is not None and owner is not asyncio.current_task():
            owner.cancel()
            await asyncio.gather(owner, return_exceptions=True)
        for client in self._clients.values():
            async with asyncio.timeout(0.1):
                await client.aclose()
        self._clients.clear()
