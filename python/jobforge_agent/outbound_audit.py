"""Optional fixed-directory metadata at the actual HTTP send boundary.

A dispatch attempt precedes the final permission/deadline check: recording may
take long enough for that check to reject sending. It proves neither entry into
HTTP send nor delivery to the remote service. These records are evidence only:
no permission, recovery, retry or metering reads this file. Missing/partial
records must remain incomplete when exported for scoring.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import TYPE_CHECKING, Any, Literal

from jobforge_agent.embedding import MODEL

if TYPE_CHECKING:
    from jobforge_agent.dispatch import PreparedRequest

_DIRECTORY = Path("/var/lib/jobforge/outbound")
_MAX_RECORD_BYTES = 2048


@dataclass(frozen=True)
class OutboundAudit:
    """Retain only selected immutable request metadata for one physical send."""

    metadata: dict[str, Any] = field(repr=False)

    def failure(self, *, stage: str, reason: str, buffered_bytes: int) -> None:
        """Retain bounded failure facts separately from immutable send records."""
        if stage not in {"send", "headers", "body"} or reason not in {
            "content_encoding",
            "content_length",
            "size_limit",
            "cancelled",
            "http_timeout",
            "http_error",
            "incomplete",
        }:
            return
        if type(buffered_bytes) is not int or not 0 <= buffered_bytes <= 262144:
            return
        try:
            value = {
                "schema_version": 1,
                "physical_call_id": self.metadata["physical_call_id"],
                "stage": stage,
                "reason": reason,
                "buffered_bytes": buffered_bytes,
            }
            path = _DIRECTORY / (self.metadata["physical_call_id"] + ".failure.json")
            with path.open("x", encoding="ascii") as target:
                os.chmod(path, 0o600)
                target.write(json.dumps(value, separators=(",", ":")) + "\n")
        except (OSError, ValueError, TypeError):
            return

    def record(
        self,
        event: Literal["dispatch_attempt", "http_response", "finish"],
        *,
        http_status: int | None = None,
        response_complete: bool = False,
    ) -> None:
        """Append one bounded line; recording failure cannot alter execution."""
        try:
            value = {
                **self.metadata,
                "schema_version": 1,
                "event": event,
                "time": datetime.now(timezone.utc)
                .isoformat(timespec="microseconds")
                .replace("+00:00", "Z"),
                "http_status": http_status,
                "response_complete": response_complete,
            }
            line = (
                json.dumps(value, ensure_ascii=True, separators=(",", ":")) + "\n"
            ).encode("ascii")
            if len(line) > _MAX_RECORD_BYTES:
                return
            path = _DIRECTORY / (self.metadata["physical_call_id"] + ".jsonl")
            descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
            try:
                # A single append avoids buffered interleaving between fixed
                # step processes; a short write remains visibly incomplete.
                os.write(descriptor, line)
            finally:
                os.close(descriptor)
        except (OSError, ValueError, TypeError):
            # No raw exception, request or credential may reach diagnostics.
            return


def begin(
    request: PreparedRequest,
    physical_call_id: str,
    start: dict[str, Any],
    origin: str,
) -> OutboundAudit | None:
    """Select safe request fields only when the trusted mount already exists."""
    try:
        if not _DIRECTORY.is_dir():
            return None
        binding = start["binding"]
        model = thinking = max_tokens = None
        if request.subcall == "chat":
            body = json.loads(request.body)
            model, thinking, max_tokens = (
                body["model"],
                body["thinking"]["type"],
                body["max_tokens"],
            )
            if (
                type(model) is not str
                or type(thinking) is not str
                or type(max_tokens) is not int
            ):
                return None
            resource = model
        elif request.subcall == "query_embedding":
            resource = json.loads(request.body)["model"]
        elif request.subcall in {"profile_version", "profile_tags"}:
            resource = MODEL
        else:
            snapshot = start["checkpoint"]["snapshot"]
            resource = (
                snapshot["index_id"]
                if request.subcall == "search_policy"
                else snapshot["ticket_binding_json"]["order_id"]
            )
        if resource is not None and type(resource) is not str:
            return None
        audit = OutboundAudit(
            {
                "physical_call_id": physical_call_id,
                "tenant_id": binding["tenant_id"],
                "snapshot_id": binding["snapshot_id"],
                "profile_hash": binding["profile_hash"],
                "subcall": request.subcall,
                "method": request.method,
                "endpoint_alias": request.endpoint,
                "origin": origin,
                "resource_id": resource,
                "model": model,
                "thinking": thinking,
                "max_tokens": max_tokens,
            }
        )
        audit.record("dispatch_attempt")
        return audit
    except (OSError, ValueError, TypeError, KeyError):
        return None
