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

from jobforge_agent import outbound_audit
from jobforge_agent.embedding import LOCAL_ORIGINS
from jobforge_agent.errors import ToolError
from jobforge_agent.http import origin, strict_json
from jobforge_agent.protocol_v2 import (
    MAX_INTEGER,
    Conversation,
    Frame,
    ProtocolError,
    encode,
    report_hash,
    usage_hash,
)
from jobforge_agent.provider_audit import (
    CallReport,
    ProviderAudit,
    capture_chat_report,
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
class ConfirmedObservation:
    """Remember only an observation accepted by this step's original authority."""

    call_sequence: int
    subcall: str
    tool_invocation_id: str
    physical_call_id: str
    transport_outcome: str
    business_outcome: str
    error_code: str
    usage_disposition: str
    usage_hash: str | None
    audit_hash: str | None
    observation_hash: str
    ack_emitted_mono_ms: int


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

    async def observe(self, observation: Frame) -> Frame:
        """Return the exact v2 ACK after the observation is confirmed."""
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
        expected_response_model: str = "deepseek-flash",
        provider_audit_policy: str = "deepseek-audit-v1",
    ) -> None:
        """Bind the original frame without an unauthorised fallback path."""
        if hooks is None or any(
            not callable(getattr(hooks, name, None))
            for name in ("authorize", "observe", "settle")
        ):
            raise DispatchError("INPUT_INVALID")
        self._clock = clock
        if (
            expected_response_model != "deepseek-flash"
            or provider_audit_policy != "deepseek-audit-v1"
        ):
            raise DispatchError("PROFILE_UNAVAILABLE")
        self._expected_response_model = expected_response_model
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
        self._confirmed: ConfirmedObservation | None = None
        self._finalized = False

    @property
    def closed(self) -> bool:
        """Expose the existing protocol's irreversible authority closure."""
        return self._conversation.closed

    def stop(self) -> None:
        """Stop authority immediately and wake the request-owned watcher."""
        self._conversation.stop()
        self._stopped.set()

    def recorded_usage(self) -> tuple[Frame, ...]:
        """Copy frozen usage/audit reports, including audit-only stopped calls."""
        return tuple(copy.deepcopy(self._reports))

    def recorded_audit(self) -> tuple[CapturedUsageAudit, ...]:
        """Read bounded audit metadata; only report ACKs can confirm persistence."""
        return tuple(self._audits)

    def last_confirmed_observation(self) -> ConfirmedObservation | None:
        """Return an immutable copy only after metering and ordinary ACK checks."""
        return self._confirmed

    def finalize_result(
        self,
        result: dict[str, Any],
        *,
        outcome: Literal["success", "error"],
        error_code: str,
    ) -> Frame:
        """Validate and close the same Conversation with one bound step result."""
        # Imported locally to keep the generic C2 dispatcher independent at load.
        from jobforge_agent.runtime_input import RuntimeInputError, validate_step_result

        kind = self._start["binding"]["step_kind"]
        try:
            if self._finalized or self._busy:
                raise ProtocolError()
            validate_step_result(result, kind)
            confirmed = self._confirmed
            local = kind in {"read_ticket", "submit_proposal"}
            if not local:
                if confirmed is None or (
                    result["physical_call_id"] != confirmed.physical_call_id
                    or result["tool_invocation_id"] != confirmed.tool_invocation_id
                ):
                    raise ProtocolError()
            if outcome == "error":
                if not (
                    kind == "model_proposal"
                    and error_code == "OUTPUT_INVALID"
                    and result["correction_required"]
                    and result["proposal"] is None
                    and confirmed is not None
                    and confirmed.transport_outcome == "response"
                    and confirmed.business_outcome == "rejected"
                    and confirmed.error_code == "OUTPUT_INVALID"
                ):
                    raise ProtocolError()
            elif outcome != "success" or error_code or result["correction_required"]:
                raise ProtocolError()
            elif not local and (
                confirmed is None or confirmed.business_outcome != "accepted"
            ):
                raise ProtocolError()
            frame = self._frame(
                "step_result",
                outcome=outcome,
                error_code=error_code,
                result=copy.deepcopy(result),
            )
            encode(frame)
            self._conversation.accept(frame, self._now())
            self._finalized = True
            return frame
        except RuntimeInputError as error:
            self.stop()
            raise DispatchError(
                "PROTOCOL_ERROR", fact="size_limit" if error.size_limit else ""
            ) from None
        except ProtocolError as error:
            self.stop()
            raise DispatchError(
                "PROTOCOL_ERROR",
                fact="size_limit" if error.code == "FRAME_LIMIT" else "",
            ) from None

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
            provider_audit=None,
        )
        report["report_hash"] = report_hash(report)
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

    def _capture_chat(
        self,
        complete: CompleteResponse | None,
        permit: Frame,
        request: PreparedRequest,
    ) -> Frame:
        captured: CallReport = capture_chat_report(
            complete.body if complete is not None else None,
            http_status=complete.status_code if complete is not None else 0,
            physical_call_id=permit["physical_call_id"],
            expected_response_model=self._expected_response_model,
        )
        report = self._frame(
            "metering_report",
            call_sequence=permit["call_sequence"],
            physical_call_id=permit["physical_call_id"],
            parameter_hash=request.parameter_hash,
            **captured.to_dict(),
        )
        report["report_hash"] = report_hash(report)
        self._conversation.accept_metering(report, self._now())
        self._reports.append(copy.deepcopy(report))
        audit = captured.provider_audit
        assert isinstance(audit, ProviderAudit)
        self._audits.append(
            CapturedUsageAudit(
                permit["physical_call_id"],
                captured.usage.receipt_hash if captured.usage else "",
                json.dumps(audit.to_dict(), separators=(",", ":")),
                audit.reasoning_tokens,
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
        response: httpx.Response | None = None
        complete_body = False
        stage, failure_reason, buffered_bytes = "send", "incomplete", 0
        audit = outbound_audit.begin(
            request,
            permit["physical_call_id"],
            self._start,
            self._endpoints[request.endpoint].base_origin,
        )
        # Optional metadata I/O may consume the remaining dispatch window.
        # Recheck and consume permission immediately before entering HTTP send.
        now = self._guard(deadline)
        self._conversation.can_dispatch(permit["physical_call_id"], now)
        try:
            response = await client.send(outbound, stream=True)
            stage = "headers"
            if audit is not None:
                audit.record("http_response", http_status=response.status_code)
            if response.headers.get("content-encoding", "identity") != "identity":
                failure_reason = "content_encoding"
                raise DispatchError("PROTOCOL_ERROR")
            length = response.headers.get("content-length")
            if length is not None:
                if not length.isascii() or not length.isdecimal():
                    failure_reason = "content_length"
                    raise DispatchError("PROTOCOL_ERROR")
                if int(length) > request.max_response_bytes:
                    failure_reason = "size_limit"
                    raise DispatchError("OUTPUT_INVALID", fact="size_limit", stop=True)
            data = bytearray()
            stage = "body"
            # Reading the raw stream directly allows capture before an awaited
            # response.aclose(), including its cancellation/failure path.
            assert isinstance(response.stream, httpx.AsyncByteStream)
            async for chunk in response.stream:
                if len(data) + len(chunk) > request.max_response_bytes:
                    failure_reason = "size_limit"
                    raise DispatchError("OUTPUT_INVALID", fact="size_limit", stop=True)
                data.extend(chunk)
                buffered_bytes = len(data)
            complete = CompleteResponse(
                response.status_code,
                bytes(data),
                permit["physical_call_id"],
                request.parameter_hash,
            )
            received.append(complete)
            complete_body = True
            if request.subcall == "chat":
                captured.append(self._capture_chat(complete, permit, request))
            elif extract_usage is not None:
                evidence = extract_usage(complete)
                if evidence is not None:
                    captured.append(self._capture(evidence, permit, request))
            return complete
        except asyncio.CancelledError:
            failure_reason = "cancelled"
            raise
        except httpx.TimeoutException:
            failure_reason = "http_timeout"
            raise
        except (httpx.HTTPError, OSError):
            failure_reason = "http_error"
            raise
        finally:
            if audit is not None:
                audit.record(
                    "finish",
                    http_status=response.status_code if response is not None else None,
                    response_complete=complete_body,
                )
                if not complete_body:
                    audit.failure(
                        stage=stage,
                        reason=failure_reason,
                        buffered_bytes=buffered_bytes,
                    )
            # A consumed chat permit always retains one immutable fact, including
            # cancellation before a complete body. Never replace it with a later
            # response or wait for more provider data during shutdown.
            if request.subcall == "chat" and not captured:
                captured.append(self._capture_chat(None, permit, request))
            # Resource cleanup has a fixed ceiling and never spawns detached work.
            if response is not None:
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
        """Authorize once and return only after report and ordinary confirmations.

        Chat facts are always captured by the fixed adapter before validation;
        extract_usage is used only by the existing nonchat embedding adapter.
        """
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
        except (httpx.TimeoutException, TimeoutError):
            self.stop()
            failure = DispatchError("TIMEOUT")
        except (httpx.HTTPError, OSError):
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
        self._confirmed = None
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
        except (httpx.TimeoutException, TimeoutError):
            failure = DispatchError("TIMEOUT")
        except (httpx.HTTPError, OSError):
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
            usage_disposition="reported"
            if captured and captured[0]["usage"] is not None
            else "unknown",
            usage_hash=captured[0]["usage"]["usage_hash"]
            if captured and captured[0]["usage"] is not None
            else None,
            audit_hash=captured[0]["provider_audit"]["audit_hash"]
            if captured and captured[0]["provider_audit"] is not None
            else None,
        )
        self._guard(deadline)
        self._conversation.accept(observation, self._now())
        ack = await self._bounded(
            self._hooks.observe(copy.deepcopy(observation)), deadline
        )
        self._conversation.accept(ack, self._now())
        self._confirmed = ConfirmedObservation(
            permit["call_sequence"],
            request.subcall,
            permit["tool_invocation_id"],
            permit["physical_call_id"],
            observation["transport_outcome"],
            observation["business_outcome"],
            observation["error_code"],
            observation["usage_disposition"],
            observation["usage_hash"],
            observation["audit_hash"],
            ack["observation_hash"],
            ack["emitted_mono_ms"],
        )
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
