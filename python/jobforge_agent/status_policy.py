"""Check the status-specific read prerequisite without deciding an action."""

from __future__ import annotations

from typing import Any

from jobforge_agent.policy_supplement import supplement_tool, validate_supplement
from jobforge_agent.runtime_input import is_investigation_complete
from jobforge_agent.support_contract import require, support_sources

STATUS_POLICY_ALIASES = {"informational_only": "P08.1", "escalated": "P08.2"}


def investigation_marker(step: dict[str, Any]) -> bool:
    """Distinguish the committed marker from a model supplement intent."""
    result = step["result_json"]
    return (
        step["step"]["kind"] in {"model_decision", "protocol_correction"}
        and result["proposal"] is None
        and not result["correction_required"]
        and is_investigation_complete(result["content"])
    )


def missing_status_policy(checkpoint: dict[str, Any]) -> dict[str, Any] | None:
    """Only authoritative status and actual source-bound aliases select a read."""
    status = checkpoint["snapshot"]["ticket_binding_json"]["status"]
    alias = STATUS_POLICY_ALIASES.get(status)
    if (
        alias is None
        or alias in support_sources(checkpoint, repeated_search=True).aliases
    ):
        return None
    return {
        "type": "policy_supplement",
        "topic": "ticket_status",
        "required_aliases": [alias],
    }


def pending_status_policy(checkpoint: dict[str, Any]) -> dict[str, Any]:
    """Derive the fixed query selected by Go at the marker boundary."""
    require(bool(checkpoint["steps"]) and investigation_marker(checkpoint["steps"][-1]))
    intent = missing_status_policy(checkpoint)
    require(intent is not None)
    assert intent is not None
    validate_supplement(intent, checkpoint)
    return supplement_tool(intent)


def status_policy_satisfied(checkpoint: dict[str, Any]) -> None:
    """A completed search must satisfy the original status dependency."""
    steps = checkpoint["steps"]
    require(len(steps) >= 2 and steps[-1]["step"]["kind"] == "search_policy")
    pending_status_policy(dict(checkpoint, steps=steps[:-1]))
    require(missing_status_policy(checkpoint) is None)


def status_policy_read(checkpoint: dict[str, Any]) -> bool:
    """Recognize a committed prerequisite read, distinct from optional reads."""
    steps = checkpoint["steps"]
    return any(
        index > 0
        and step["step"]["kind"] == "search_policy"
        and investigation_marker(steps[index - 1])
        for index, step in enumerate(steps)
    )


def first_adjudication(checkpoint: dict[str, Any]) -> bool:
    """Require the prerequisite for either route into the first thinking call."""
    steps = checkpoint["steps"]
    require(bool(steps) and missing_status_policy(checkpoint) is None)
    if investigation_marker(steps[-1]):
        return True
    if len(steps) >= 2 and investigation_marker(steps[-2]):
        status_policy_satisfied(checkpoint)
        return True
    return False
