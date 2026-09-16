"""Bind only statically registered adapters to a fixed deployment manifest."""

from __future__ import annotations

import copy
from collections.abc import Mapping, Sequence
from pathlib import Path
from typing import Any, Protocol

from jobforge_agent.dispatch import DispatchError
from jobforge_agent.errors import ToolError
from jobforge_agent.http import strict_json
from jobforge_agent.runtime_input import (
    EXECUTOR_VERSION,
    HASH,
    IDENTIFIER,
    RuntimeCheckpoint,
    executor_matches_adapter,
)

_MANIFEST = Path("/etc/jobforge/executor.json")


class RegisteredAdapter(Protocol):
    """Prepare bounded business data without owning execution or scheduling."""

    adapter_id: str
    strategy: str
    proposal_schema: str

    def policy_query(self, checkpoint: RuntimeCheckpoint) -> str:
        """Build a bounded query from protected accepted facts."""
        ...

    def proposal_messages(
        self, checkpoint: RuntimeCheckpoint, *, correction: bool
    ) -> Sequence[Mapping[str, object]]:
        """Build fixed-strategy messages; do not dispatch requests."""
        ...

    def validate_proposal(
        self, value: dict[str, Any], checkpoint: RuntimeCheckpoint
    ) -> dict[str, Any]:
        """Validate the strategy's existing registered proposal schema."""
        ...


def _manifest_profiles(
    executor_version: str = EXECUTOR_VERSION,
) -> list[dict[str, str]]:
    try:
        with _MANIFEST.open("rb") as source:
            raw = source.read(16385)
        if len(raw) > 16384:
            raise ValueError()
        value = strict_json(raw)
        if (
            not isinstance(value, dict)
            or set(value) != {"schema_version", "executor_version", "profiles"}
            or type(value["schema_version"]) is not int
            or value["schema_version"] != 1
            or value["executor_version"] != executor_version
            or not isinstance(value["profiles"], list)
            or not 1 <= len(value["profiles"]) <= 32
        ):
            raise ValueError()
        seen: set[str] = set()
        profiles: list[dict[str, str]] = []
        for item in value["profiles"]:
            if not isinstance(item, dict) or set(item) != {
                "profile_id",
                "profile_hash",
                "adapter_id",
            }:
                raise ValueError()
            for key, pattern in (
                ("profile_id", IDENTIFIER),
                ("profile_hash", HASH),
                ("adapter_id", IDENTIFIER),
            ):
                if (
                    not isinstance(item[key], str)
                    or pattern.fullmatch(item[key]) is None
                ):
                    raise ValueError()
            if item["profile_id"] in seen:
                raise ValueError()
            seen.add(item["profile_id"])
            profiles.append(item)
        return profiles
    except (OSError, ValueError, TypeError, ToolError):
        raise DispatchError("PROFILE_UNAVAILABLE") from None


def resolve_adapter(
    adapter_id: str, profile_id: str, profile_hash: str, executor_version: str
) -> RegisteredAdapter:
    """Require manifest identity and fixed build registration before any work."""
    from jobforge_agent.runtime_registry import REGISTRY

    expected = {
        "profile_id": profile_id,
        "profile_hash": profile_hash,
        "adapter_id": adapter_id,
    }
    if not executor_matches_adapter(
        executor_version, adapter_id
    ) or expected not in _manifest_profiles(executor_version):
        raise DispatchError("PROFILE_UNAVAILABLE")
    adapter = REGISTRY.get(adapter_id)
    if adapter is None or adapter.adapter_id != adapter_id:
        raise DispatchError("PROFILE_UNAVAILABLE")
    return adapter


def validate_existing_proposal(
    value: dict[str, Any], checkpoint: RuntimeCheckpoint
) -> dict[str, Any]:
    """Validate the existing four-field proposal against accepted evidence only."""
    if not isinstance(value, dict) or set(value) != {
        "decision",
        "summary",
        "evidence_refs",
        "action",
    }:
        raise DispatchError("OUTPUT_INVALID")
    decision, summary, refs, action = (
        value[key] for key in ("decision", "summary", "evidence_refs", "action")
    )
    allowed = {f"business-evidence:{checkpoint['snapshot']['snapshot_id']}:ticket"}
    for accepted in checkpoint["steps"]:
        if accepted["step"]["kind"] in {
            "read_ticket",
            "get_order",
            "get_delivery",
            "search_policy",
        }:
            allowed.update(accepted["result_json"]["evidence_refs"])
    if (
        not isinstance(summary, str)
        or not summary
        or len(summary.encode("utf-8")) > 8192
        or not isinstance(refs, list)
        or not 1 <= len(refs) <= 32
        or not all(isinstance(ref, str) and ref in allowed for ref in refs)
        or len(set(refs)) != len(refs)
        or not (
            (decision == "no_action" and action == "")
            or (
                decision == "proposal"
                and action in ("record_conclusion", "request_information", "escalate")
            )
        )
    ):
        raise DispatchError("OUTPUT_INVALID")
    return copy.deepcopy(value)
