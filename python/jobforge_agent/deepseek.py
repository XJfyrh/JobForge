"""Fixed non-streaming DeepSeek chat preparation and complete-response parsing.

The public model alias and response fingerprint are audit evidence, not an
immutable provider version or price lock. This module owns no network transport,
execution permission, retry, correction scheduling, credentials, or pricing.
"""

from __future__ import annotations

import hashlib
import json
import re
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass
from typing import Any, TypeVar

from jobforge_agent.dispatch import (
    AuthorizedDispatcher,
    CompleteResponse,
    DispatchError,
    PreparedRequest,
    RunCallContext,
    UsageEvidence,
    prepare_request,
)
from jobforge_agent.errors import ToolError
from jobforge_agent.http import strict_json
from jobforge_agent.protocol_v2 import MAX_INTEGER
from jobforge_agent.provider_audit import capture_chat_report

ORIGIN = "https://api.deepseek.com"
MODEL = "deepseek-flash"
CHAT_PATH = "/chat/completions"
MAX_MESSAGE_BYTES = 16 * 1024
MAX_REQUEST_BYTES = 64 * 1024
MAX_RESPONSE_BYTES = 64 * 1024
MAX_CONTENT_BYTES = 16 * 1024
MAX_OUTPUT_TOKENS = 1024
T = TypeVar("T")

_IDENTIFIER = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:-]{0,127}")
_CALL_ID = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")
_ENVELOPE_REQUIRED = {
    "id",
    "object",
    "created",
    "model",
    "system_fingerprint",
}


@dataclass(frozen=True)
class ResponseIdentity:
    """Retain only bounded provider identifiers and a complete-response digest."""

    response_id: str
    model: str
    system_fingerprint: str
    created: int
    response_sha256: str


@dataclass(frozen=True)
class CompleteChatUsage:
    """Keep complete counters without applying a permit or output-token cap."""

    input_tokens: int
    output_tokens: int
    cached_input_tokens: int
    receipt_hash: str
    usage_hash: str
    identity: ResponseIdentity
    reasoning_tokens: int | None


def _chat_payload(messages: Sequence[Mapping[str, object]]) -> dict[str, Any]:
    if not isinstance(messages, (list, tuple)) or not messages:
        raise ToolError("INVALID_ARGUMENT")
    # Even the shortest empty message needs 28 serialized bytes. Bound the
    # input collection before constructing another one; the exact byte cap wins.
    if len(messages) > MAX_REQUEST_BYTES // 28:
        raise ToolError("SIZE_LIMIT")
    normalized: list[dict[str, str]] = []
    total = 0
    invalid = False
    try:
        for message in messages:
            if not isinstance(message, Mapping) or set(message) != {"role", "content"}:
                raise ToolError("INVALID_ARGUMENT")
            role, content = message["role"], message["content"]
            if (
                not isinstance(role, str)
                or role not in {"system", "user", "assistant"}
                or not isinstance(content, str)
            ):
                raise ToolError("INVALID_ARGUMENT")
            total += len(content.encode("utf-8"))
            if total > MAX_MESSAGE_BYTES:
                raise ToolError("SIZE_LIMIT")
            normalized.append({"role": role, "content": content})
    except (UnicodeError, ValueError, TypeError):
        invalid = True
    if invalid:
        raise ToolError("INVALID_ARGUMENT")
    return {
        "model": MODEL,
        "messages": normalized,
        "thinking": {"type": "disabled"},
        "stream": False,
        "max_tokens": MAX_OUTPUT_TOKENS,
        "response_format": {"type": "json_object"},
        "temperature": 0,
    }


def _failure(error: ToolError) -> DispatchError:
    if error.code == "SIZE_LIMIT":
        return DispatchError("OUTPUT_INVALID", fact="size_limit", stop=True)
    if error.code == "PROFILE_UNAVAILABLE":
        return DispatchError("PROFILE_UNAVAILABLE", fact="identity", stop=True)
    if error.code == "PROTOCOL_ERROR":
        return DispatchError("PROTOCOL_ERROR", fact="provider_envelope", stop=True)
    if error.code == "OUTPUT_INVALID":
        return DispatchError("OUTPUT_INVALID", fact="model_output")
    return DispatchError("INPUT_INVALID")


def prepare_chat_request(
    messages: Sequence[Mapping[str, object]], *, context: RunCallContext
) -> PreparedRequest:
    """Prepare one immutable fixed request with no caller-selected URL or model."""
    failure = None
    try:
        payload = _chat_payload(messages)
    except ToolError as error:
        failure = _failure(error)
    if failure is not None:
        raise failure
    request = prepare_request(
        context=context,
        endpoint="deepseek",
        subcall="chat",
        method="POST",
        path=CHAT_PATH,
        body=payload,
        max_response_bytes=MAX_RESPONSE_BYTES,
    )
    if len(request.body) > MAX_REQUEST_BYTES:
        raise DispatchError("OUTPUT_INVALID", fact="size_limit", stop=True)
    return request


def _safe_integer(value: Any) -> bool:
    return type(value) is int and 0 <= value <= MAX_INTEGER


def _identifier(value: Any) -> bool:
    return isinstance(value, str) and _IDENTIFIER.fullmatch(value) is not None


def _envelope(raw: bytes) -> tuple[dict[str, Any], ResponseIdentity]:
    if len(raw) > MAX_RESPONSE_BYTES:
        raise ToolError("SIZE_LIMIT")
    invalid = False
    try:
        value = strict_json(raw)
    except ToolError:
        invalid = True
    if invalid:
        raise ToolError("PROTOCOL_ERROR")
    if (
        not isinstance(value, dict)
        or not _ENVELOPE_REQUIRED <= value.keys()
        or value.keys() - (_ENVELOPE_REQUIRED | {"usage", "choices"})
        or value["object"] != "chat.completion"
        or value["model"] != MODEL
        or not _identifier(value["id"])
        or not _identifier(value["system_fingerprint"])
        or not _safe_integer(value["created"])
    ):
        raise ToolError("PROFILE_UNAVAILABLE")
    identity = ResponseIdentity(
        value["id"],
        MODEL,
        value["system_fingerprint"],
        value["created"],
        hashlib.sha256(raw).hexdigest(),
    )
    return value, identity


def _choice(envelope: dict[str, Any]) -> dict[str, Any]:
    choices = envelope.get("choices")
    if not isinstance(choices, list) or len(choices) != 1:
        raise ToolError("PROTOCOL_ERROR")
    choice = choices[0]
    if (
        not isinstance(choice, dict)
        or not {"index", "message", "finish_reason"} <= choice.keys()
        or choice.keys() - {"index", "message", "finish_reason", "logprobs"}
        or type(choice["index"]) is not int
        or choice["index"] != 0
        or choice.get("logprobs") is not None
    ):
        raise ToolError("PROTOCOL_ERROR")
    message = choice["message"]
    if (
        not isinstance(message, dict)
        or not {"role", "content"} <= message.keys()
        or message.keys() - {"role", "content", "reasoning_content", "tool_calls"}
        or message["role"] != "assistant"
        or (message["content"] is not None and not isinstance(message["content"], str))
    ):
        raise ToolError("PROTOCOL_ERROR")
    return choice


def complete_usage(raw: bytes, *, physical_call_id: str) -> CompleteChatUsage | None:
    """Project complete compatible usage from the single audit fact extractor.

    Runtime reporting always uses the full CallReport, including audit-only and
    incompatible-model evidence. This helper exposes only compatible usage and
    never decides pricing or grants permission.
    """
    if (
        not isinstance(physical_call_id, str)
        or _CALL_ID.fullmatch(physical_call_id) is None
    ):
        raise ToolError("INVALID_ARGUMENT")
    captured = capture_chat_report(
        raw,
        http_status=200,
        physical_call_id=physical_call_id,
        expected_response_model=MODEL,
    )
    audit, usage = captured.provider_audit, captured.usage
    if usage is None or audit is None or audit.identity_state != "compatible":
        return None
    assert audit.response_id is not None and audit.response_model is not None
    assert audit.system_fingerprint is not None and audit.created is not None
    assert audit.response_sha256 is not None
    identity = ResponseIdentity(
        audit.response_id,
        audit.response_model,
        audit.system_fingerprint,
        audit.created,
        audit.response_sha256,
    )
    return CompleteChatUsage(
        usage.input_tokens,
        usage.output_tokens,
        usage.cached_input_tokens,
        usage.receipt_hash,
        usage.usage_hash,
        identity,
        audit.reasoning_tokens,
    )


def proposal_object(raw: bytes) -> dict[str, Any]:
    """Reject incomplete or invalid output without altering independently read usage."""
    envelope, _ = _envelope(raw)
    choice = _choice(envelope)
    message = choice["message"]
    provider_usage = envelope.get("usage")
    details = (
        provider_usage.get("completion_tokens_details")
        if isinstance(provider_usage, dict)
        else None
    )
    reasoning = details.get("reasoning_tokens") if isinstance(details, dict) else None
    if isinstance(reasoning, int) and _safe_integer(reasoning) and reasoning > 0:
        # Count this complete usage, but never continue a batch whose provider
        # reports thinking despite the fixed non-thinking request configuration.
        raise ToolError("PROFILE_UNAVAILABLE")
    if message.get("reasoning_content") not in (None, "") or message.get(
        "tool_calls"
    ) not in (None, []):
        raise ToolError("PROTOCOL_ERROR")
    content = message["content"]
    if choice["finish_reason"] != "stop" or not isinstance(content, str):
        raise ToolError("OUTPUT_INVALID")
    invalid = False
    try:
        encoded = content.encode("utf-8")
    except UnicodeError:
        invalid = True
    if invalid:
        raise ToolError("OUTPUT_INVALID")
    if len(encoded) > MAX_CONTENT_BYTES:
        raise ToolError("SIZE_LIMIT")
    invalid = False
    try:
        value = strict_json(encoded)
    except ToolError:
        invalid = True
    if invalid:
        raise ToolError("OUTPUT_INVALID")
    if not isinstance(value, dict):
        raise ToolError("OUTPUT_INVALID")
    return value


def extract_chat_usage(response: CompleteResponse) -> UsageEvidence | None:
    """Capture billing evidence before any schema validation or asynchronous hook."""
    if response.status_code != 200:
        return None
    report = complete_usage(response.body, physical_call_id=response.physical_call_id)
    if report is None:
        return None
    identity = report.identity
    audit = json.dumps(
        {
            "id": identity.response_id,
            "model": identity.model,
            "fp": identity.system_fingerprint,
            "created": identity.created,
            "sha256": identity.response_sha256,
        },
        separators=(",", ":"),
    )
    return UsageEvidence(
        report.input_tokens,
        report.output_tokens,
        report.cached_input_tokens,
        report.receipt_hash,
        provider_identity=audit,
        reasoning_tokens=report.reasoning_tokens,
    )


class DeepSeekChat:
    """Use the shared authorized dispatcher; no network or permission fallback."""

    def __init__(self, dispatcher: AuthorizedDispatcher) -> None:
        """Borrow a dispatcher that owns the original step and fixed endpoint."""
        self._dispatcher = dispatcher

    async def propose(
        self,
        messages: Sequence[Mapping[str, object]],
        *,
        context: RunCallContext,
        validate_proposal: Callable[[dict[str, Any]], T],
    ) -> T:
        """Return only a validated proposal after observation/settlement barriers.

        The trusted schema validator is synchronous and reports invalid proposals
        with ValueError, ToolError or DispatchError. Correction scheduling belongs
        to the future Worker. Cancellation cannot retract a provider-side send or
        promise a refund; the dispatcher preserves already captured usage only.
        """
        request = prepare_chat_request(messages, context=context)

        def validate(response: CompleteResponse) -> T:
            if response.status_code == 429 or 500 <= response.status_code <= 599:
                raise DispatchError("DEPENDENCY_UNAVAILABLE")
            if response.status_code != 200:
                raise DispatchError("PROFILE_UNAVAILABLE", fact="identity", stop=True)
            failure = None
            try:
                proposal = proposal_object(response.body)
            except ToolError as error:
                failure = _failure(error)
            if failure is not None:
                raise failure
            try:
                return validate_proposal(proposal)
            except DispatchError as error:
                failure = DispatchError(error.code, fact=error.fact, stop=error.stop)
            except (ToolError, ValueError, TypeError):
                failure = DispatchError("OUTPUT_INVALID")
            raise failure

        return await self._dispatcher.execute(
            request,
            context=context,
            validate=validate,
        )
