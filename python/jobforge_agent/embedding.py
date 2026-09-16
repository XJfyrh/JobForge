"""Fixed free local MiniLM embedding with verified model identity."""

from __future__ import annotations

import math
import struct
import time
from collections.abc import Callable
from typing import Any

import httpx

from jobforge_agent.errors import ToolError
from jobforge_agent.http import BoundedHTTP

MODEL = "all-minilm:22m"
MODEL_DIGEST = "1b226e2802dbb772b5fc32a58f103ca1804ef7501331012de126ab22f67475ef"
OLLAMA_VERSION = "0.32.5"
DIMENSIONS = 384
LOCAL_ORIGINS = (
    "http://127.0.0.1:11434",
    "http://127.0.0.1:11435",
    "http://127.0.0.1:11436",
    "http://localhost:11434",
    "http://localhost:11435",
    "http://localhost:11436",
    "http://ollama:11434",
)


def vector(value: Any) -> list[float]:
    """Validate all 384 finite components and a finite nonzero norm."""
    if not isinstance(value, list) or len(value) != DIMENSIONS:
        raise ToolError("INVALID_VECTOR")
    result: list[float] = []
    for component in value:
        if isinstance(component, bool) or not isinstance(component, (int, float)):
            raise ToolError("INVALID_VECTOR")
        try:
            number = float(component)
        except OverflowError as exc:
            raise ToolError("INVALID_VECTOR") from exc
        if not math.isfinite(number) or abs(number) > 3.4028234663852886e38:
            raise ToolError("INVALID_VECTOR")
        result.append(number)
    norm = math.hypot(
        *(struct.unpack("f", struct.pack("f", item))[0] for item in result)
    )
    if not math.isfinite(norm) or norm == 0:
        raise ToolError("INVALID_VECTOR")
    return result


class OllamaEmbedding:
    """Use only the registered local model, with no retries or truncation."""

    def __init__(
        self,
        local_origin: str = LOCAL_ORIGINS[0],
        *,
        transport: httpx.AsyncBaseTransport | None = None,
        before_send: Callable[[str], None] | None = None,
    ) -> None:
        """Select an allowlisted local deployment, never cloud credentials."""
        if local_origin not in LOCAL_ORIGINS:
            raise ToolError("INVALID_CONFIGURATION")
        self._http = BoundedHTTP(
            local_origin, transport=transport, before_send=before_send
        )

    async def aclose(self) -> None:
        """Release owned HTTP connections."""
        await self._http.aclose()

    async def embed(self, texts: list[str], *, deadline: float) -> list[list[float]]:
        """Verify version and digest before each bounded batch of at most 16."""
        try:
            if (
                not isinstance(texts, list)
                or not 1 <= len(texts) <= 16
                or any(
                    not isinstance(text, str)
                    or not text.strip()
                    or len(text.encode("utf-8")) > 768
                    for text in texts
                )
            ):
                raise ToolError("INVALID_ARGUMENT")
        except UnicodeError as exc:
            raise ToolError("INVALID_ARGUMENT") from exc
        deadline = min(deadline, time.monotonic() + 60)
        version = await self._http.request(
            "GET", "/api/version", deadline=deadline, max_response=1024
        )
        if version.get("version") != OLLAMA_VERSION:
            raise ToolError("PROFILE_UNAVAILABLE")
        tags = await self._http.request(
            "GET", "/api/tags", deadline=deadline, max_response=64 * 1024
        )
        models = tags.get("models")
        matches = (
            [
                item
                for item in models
                if isinstance(item, dict) and item.get("name") == MODEL
            ]
            if isinstance(models, list)
            else []
        )
        if len(matches) != 1 or matches[0].get("digest") != MODEL_DIGEST:
            raise ToolError("PROFILE_UNAVAILABLE")
        response = await self._http.request(
            "POST",
            "/api/embed",
            deadline=deadline,
            max_response=256 * 1024,
            body={"model": MODEL, "input": texts, "truncate": False},
        )
        embeddings = response.get("embeddings")
        if (
            response.get("model") != MODEL
            or not isinstance(embeddings, list)
            or len(embeddings) != len(texts)
        ):
            raise ToolError("INVALID_VECTOR")
        return [vector(item) for item in embeddings]
