"""Registered read tools using per-HTTP Run authorization and shared validators."""

from __future__ import annotations

import hashlib
import json
from collections.abc import Callable
from typing import Any, TypeVar

from jobforge_agent.business import (
    SnapshotBinding,
    search_body,
    tool_argument,
    tool_path,
    validate_read_response,
    validate_search_response,
)
from jobforge_agent.dispatch import (
    AuthorizedDispatcher,
    CompleteResponse,
    DispatchError,
    RunCallContext,
    UsageEvidence,
    prepare_request,
)
from jobforge_agent.embedding import (
    MODEL,
    MODEL_DIGEST,
    OLLAMA_VERSION,
    embedding_body,
    validate_embeddings,
    validate_tags,
    validate_version,
)
from jobforge_agent.errors import ToolError
from jobforge_agent.http import strict_json
from jobforge_agent.protocol_v2 import MAX_INTEGER

T = TypeVar("T")


def _checked(check: Callable[[], T], *, input_argument: bool = False) -> T:
    code = ""
    try:
        return check()
    except ToolError as error:
        code = error.code
    # Raise outside the handler so no parser/provider exception is retained.
    if input_argument:
        raise DispatchError("INPUT_INVALID")
    if code == "PROFILE_UNAVAILABLE":
        raise DispatchError("PROFILE_UNAVAILABLE", fact="identity", stop=True)
    raise DispatchError("OUTPUT_INVALID")


def _response_object(response: CompleteResponse) -> dict[str, Any]:
    if response.status_code != 200:
        if response.status_code == 429 or response.status_code >= 500:
            raise DispatchError("DEPENDENCY_UNAVAILABLE")
        raise DispatchError("OUTPUT_INVALID")
    value = _checked(lambda: strict_json(response.body))
    if not isinstance(value, dict):
        raise DispatchError("OUTPUT_INVALID")
    return value


def _response_validator(
    check: Callable[[dict[str, Any]], T],
) -> Callable[[CompleteResponse], T]:
    def validate(response: CompleteResponse) -> T:
        value = _response_object(response)
        return _checked(lambda: check(value))

    return validate


def _embedding_usage(response: CompleteResponse) -> UsageEvidence | None:
    if response.status_code != 200:
        return None
    value: Any = None
    try:
        value = strict_json(response.body)
    except ToolError:
        # An incomplete or ambiguous envelope never supplies trusted usage.
        pass
    if not isinstance(value, dict):
        return None
    if value.get("model") != MODEL:
        raise DispatchError("PROFILE_UNAVAILABLE", fact="identity", stop=True)
    count = value.get("prompt_eval_count")
    if type(count) is not int or not 0 <= count <= MAX_INTEGER:
        return None
    receipt = json.dumps(
        [
            "jobforge.run.ollama-receipt.v1",
            response.physical_call_id,
            response.parameter_hash,
            OLLAMA_VERSION,
            MODEL,
            MODEL_DIGEST,
            hashlib.sha256(response.body).hexdigest(),
        ],
        separators=(",", ":"),
        ensure_ascii=True,
    ).encode("ascii")
    return UsageEvidence(
        input_tokens=count,
        output_tokens=0,
        cached_input_tokens=0,
        receipt_hash=hashlib.sha256(receipt).hexdigest(),
        provider_identity=MODEL,
    )


class RunOllamaEmbedding:
    """Perform the fixed version, digest and single-query embedding sequence."""

    def __init__(self, dispatcher: AuthorizedDispatcher) -> None:
        """Borrow one step's dispatcher without taking its connection ownership."""
        self._dispatcher = dispatcher

    async def query(self, text: str, *, context: RunCallContext) -> list[float]:
        """Accept each validated response before requesting the next permission."""
        body = _checked(lambda: embedding_body([text]), input_argument=True)
        try:
            too_long = len(text.encode("utf-8")) > 512
        except UnicodeError:
            too_long = True
        if too_long:
            raise DispatchError("INPUT_INVALID")
        for subcall, path, maximum, validator in (
            ("profile_version", "/api/version", 1024, validate_version),
            ("profile_tags", "/api/tags", 64 * 1024, validate_tags),
        ):
            request = prepare_request(
                context=context,
                endpoint="ollama",
                subcall=subcall,
                method="GET",
                path=path,
                max_response_bytes=maximum,
            )
            await self._dispatcher.execute(
                request,
                context=context,
                validate=_response_validator(validator),
            )
        request = prepare_request(
            context=context,
            endpoint="ollama",
            subcall="query_embedding",
            method="POST",
            path="/api/embed",
            body=body,
            max_response_bytes=256 * 1024,
        )
        embeddings = await self._dispatcher.execute(
            request,
            context=context,
            validate=_response_validator(lambda value: validate_embeddings(value, 1)),
            extract_usage=_embedding_usage,
        )
        return embeddings[0]


class RunBusinessTools:
    """Build only the registered one-, one- and four-request read tools."""

    def __init__(
        self, dispatcher: AuthorizedDispatcher, binding: SnapshotBinding
    ) -> None:
        """Receive a trusted business binding separate from the Run profile hash."""
        self._dispatcher = dispatcher
        self._binding = binding
        self._embedding = RunOllamaEmbedding(dispatcher)

    async def execute_call(
        self, call: Any, *, context: RunCallContext
    ) -> dict[str, Any]:
        """Reject multi-call or augmented envelopes before any authorization."""
        if not isinstance(call, dict) or set(call) != {"name", "arguments"}:
            raise DispatchError("INPUT_INVALID")
        return await self.execute(call["name"], call["arguments"], context=context)

    async def execute(
        self, name: Any, arguments: Any, *, context: RunCallContext
    ) -> dict[str, Any]:
        """Return only after business validation and dispatch confirmation."""
        argument = _checked(
            lambda: tool_argument(name, arguments, self._binding), input_argument=True
        )
        path = tool_path(self._binding, name)
        if name != "search_policy":
            request = prepare_request(
                context=context,
                endpoint="business",
                subcall=name,
                method="GET",
                path=path,
                max_response_bytes=8 * 1024,
            )
            return await self._dispatcher.execute(
                request,
                context=context,
                validate=_response_validator(
                    lambda value: validate_read_response(value, self._binding, name)
                ),
            )
        assert isinstance(argument, str)
        embedding = await self._embedding.query(argument, context=context)
        request = prepare_request(
            context=context,
            endpoint="business",
            subcall="search_policy",
            method="POST",
            path=path,
            body=search_body(embedding),
            max_response_bytes=8 * 1024,
        )
        return await self._dispatcher.execute(
            request,
            context=context,
            validate=_response_validator(
                lambda value: validate_search_response(value, self._binding)
            ),
        )
