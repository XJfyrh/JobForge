"""Three registered read tools bound to a caller-authorized business snapshot."""

from __future__ import annotations

import re
import time
from collections.abc import Callable
from dataclasses import dataclass
from typing import Any
from uuid import UUID

import httpx

from jobforge_agent.embedding import MODEL, MODEL_DIGEST, OllamaEmbedding
from jobforge_agent.errors import ToolError
from jobforge_agent.http import BoundedHTTP

TOOL_NAMES = frozenset({"get_order", "get_delivery", "search_policy"})
TOOLS = [
    {
        "type": "function",
        "function": {
            "name": name,
            "description": description,
            "parameters": {
                "type": "object",
                "additionalProperties": False,
                "properties": {argument: schema},
                "required": [argument],
            },
        },
    }
    for name, argument, schema, description in [
        (
            "get_order",
            "order_id",
            {"type": ["string", "null"]},
            "Read the authorized snapshot order; null means no associated order.",
        ),
        (
            "get_delivery",
            "order_id",
            {"type": ["string", "null"]},
            "Read delivery for the authorized snapshot order.",
        ),
        (
            "search_policy",
            "query",
            {"type": "string", "minLength": 1, "maxLength": 512},
            "Search the snapshot policy with a query of at most 512 UTF-8 bytes.",
        ),
    ]
]


@dataclass(frozen=True)
class SnapshotBinding:
    """Trusted caller configuration; model arguments cannot change these fields."""

    snapshot_id: str
    order_id: str | None
    profile_hash: str
    index_id: str
    policy_version: str

    def __post_init__(self) -> None:
        """Reject invalid binding identifiers before any network dispatch."""
        try:
            if str(UUID(self.snapshot_id)) != self.snapshot_id:
                raise ValueError("invalid snapshot")
            if str(UUID(self.index_id)) != self.index_id:
                raise ValueError("invalid index")
        except (ValueError, TypeError, AttributeError) as exc:
            raise ToolError("INVALID_CONFIGURATION") from exc
        if not isinstance(self.profile_hash, str) or not re.fullmatch(
            "[0-9a-f]{64}", self.profile_hash
        ):
            raise ToolError("INVALID_CONFIGURATION")
        if self.order_id is not None and (
            not isinstance(self.order_id, str)
            or not re.fullmatch("[A-Za-z0-9][A-Za-z0-9._:-]{0,127}", self.order_id)
        ):
            raise ToolError("INVALID_CONFIGURATION")
        if not isinstance(self.policy_version, str) or not re.fullmatch(
            "[A-Za-z0-9_.-]{1,128}", self.policy_version
        ):
            raise ToolError("INVALID_CONFIGURATION")


class BusinessTools:
    """Dispatch one authorized read tool over real HTTP with fixed URL paths."""

    def __init__(
        self,
        business_origin: str,
        bearer_key: str,
        binding: SnapshotBinding,
        embedding: OllamaEmbedding,
        *,
        transport: httpx.AsyncBaseTransport | None = None,
        before_send: Callable[[str], None] | None = None,
    ) -> None:
        """Receive deployment credentials and binding from the trusted caller."""
        self.binding = binding
        self._embedding = embedding
        self._http = BoundedHTTP(
            business_origin,
            bearer_key=bearer_key,
            transport=transport,
            before_send=before_send,
        )

    async def aclose(self) -> None:
        """Close business HTTP; the caller separately owns the embedding client."""
        await self._http.aclose()

    async def execute_call(self, call: Any, *, deadline: float) -> dict[str, Any]:
        """Reject unknown envelope fields and multi-call arrays before dispatch."""
        if not isinstance(call, dict) or set(call) != {"name", "arguments"}:
            raise ToolError("INVALID_TOOL_CALL")
        return await self.execute(call["name"], call["arguments"], deadline=deadline)

    async def execute(
        self, name: Any, arguments: Any, *, deadline: float
    ) -> dict[str, Any]:
        """Execute a single fixed capability after validating its entire input."""
        if not isinstance(name, str) or name not in TOOL_NAMES:
            raise ToolError("UNKNOWN_TOOL")
        key = "query" if name == "search_policy" else "order_id"
        if not isinstance(arguments, dict) or set(arguments) != {key}:
            raise ToolError("INVALID_ARGUMENT")
        argument = arguments[key]
        path = f"/business/v1/snapshots/{self.binding.snapshot_id}"
        if name != "search_policy":
            if argument is not None and not isinstance(argument, str):
                raise ToolError("INVALID_ARGUMENT")
            if argument != self.binding.order_id:
                raise ToolError("OBJECT_NOT_AUTHORIZED")
            kind = "order" if name == "get_order" else "delivery"
            result = await self._http.request(
                "GET",
                path + "/" + kind,
                deadline=min(deadline, time.monotonic() + 10),
                max_response=8 * 1024,
            )
            if (
                result.get("snapshot_id") != self.binding.snapshot_id
                or result.get("evidence_ref")
                != f"business-evidence:{self.binding.snapshot_id}:{kind}"
            ):
                raise ToolError("INVALID_EVIDENCE")
            fact = result.get(kind)
            if (
                result.get("kind") != kind
                or type(result.get("missing")) is not bool
                or (
                    not result["missing"]
                    and (
                        not isinstance(fact, dict)
                        or fact.get("order_id") != self.binding.order_id
                    )
                )
                or (result["missing"] and fact is not None)
            ):
                raise ToolError("INVALID_EVIDENCE")
            return result
        try:
            if (
                not isinstance(argument, str)
                or not argument.strip()
                or len(argument.encode("utf-8")) > 512
            ):
                raise ToolError("INVALID_ARGUMENT")
        except UnicodeError as exc:
            raise ToolError("INVALID_ARGUMENT") from exc
        embeddings = await self._embedding.embed([argument], deadline=deadline)
        result = await self._http.request(
            "POST",
            path + "/policies/search",
            deadline=min(deadline, time.monotonic() + 10),
            max_response=8 * 1024,
            body={
                "embedding_model": MODEL,
                "embedding_digest": MODEL_DIGEST,
                "query_vector": embeddings[0],
            },
        )
        if result.get("snapshot_id") != self.binding.snapshot_id:
            raise ToolError("INVALID_EVIDENCE")
        matches = result.get("matches")
        if not isinstance(matches, list) or len(matches) > 3:
            raise ToolError("INVALID_RESPONSE")
        seen: set[str] = set()
        for item in matches:
            if not isinstance(item, dict):
                raise ToolError("INVALID_RESPONSE")
            chunk_id = item.get("chunk_id")
            if (
                not isinstance(chunk_id, str)
                or not re.fullmatch("[A-Za-z0-9][A-Za-z0-9._:-]{0,127}", chunk_id)
                or chunk_id in seen
                or item.get("index_id") != self.binding.index_id
                or item.get("policy_version") != self.binding.policy_version
                or item.get("evidence_ref")
                != f"business-policy:{self.binding.index_id}:{chunk_id}"
            ):
                raise ToolError("INVALID_EVIDENCE")
            text, distance = item.get("text"), item.get("distance")
            if (
                not isinstance(text, str)
                or not text.strip()
                or isinstance(distance, bool)
                or not isinstance(distance, (float, int))
                or not -0.000001 <= distance <= 2.000001
                or not isinstance(item.get("source"), str)
            ):
                raise ToolError("INVALID_RESPONSE")
            try:
                if len(text.encode("utf-8")) > 768:
                    raise ToolError("INVALID_RESPONSE")
            except UnicodeError as exc:
                raise ToolError("INVALID_RESPONSE") from exc
            seen.add(chunk_id)
        return result
