"""Validate one typed policy read against committed sources, never policy truth."""

from __future__ import annotations

import copy
from typing import Any

from jobforge_agent.support_agent import POLICY_TOPICS
from jobforge_agent.support_agent_v5 import DEFINITION_ALIASES
from jobforge_agent.support_contract import require, support_sources


def is_policy_supplement(value: Any) -> bool:
    """Recognize the closed intent without granting permission to execute it."""
    if not isinstance(value, dict) or set(value) != {
        "type",
        "topic",
        "required_aliases",
    }:
        return False
    topic, aliases = value["topic"], value["required_aliases"]
    return (
        value["type"] == "policy_supplement"
        and isinstance(topic, str)
        and topic in DEFINITION_ALIASES
        and isinstance(aliases, list)
        and 0 < len(aliases) <= len(DEFINITION_ALIASES[topic])
        and all(
            isinstance(alias, str) and alias in DEFINITION_ALIASES[topic]
            for alias in aliases
        )
        and aliases == sorted(set(aliases))
    )


def supplement_tool(value: dict[str, Any]) -> dict[str, Any]:
    """Derive the fixed search intent; this function never dispatches it."""
    require(is_policy_supplement(value))
    return {
        "type": "tool",
        "name": "search_policy",
        "arguments": {"query": POLICY_TOPICS[value["topic"]][1]},
    }


def validate_supplement(value: Any, checkpoint: dict[str, Any]) -> dict[str, Any]:
    """Require each requested alias to be missing and prohibit repeat reads."""
    require(is_policy_supplement(value))
    from jobforge_agent.status_policy import status_policy_read

    require(value["topic"] != "ticket_status" or not status_policy_read(checkpoint))
    sources = support_sources(checkpoint, repeated_search=True)
    require(all(alias not in sources.aliases for alias in value["required_aliases"]))
    tool = supplement_tool(value)
    for step in checkpoint["steps"]:
        require(step["step"]["kind"] != "model_proposal")
        require(step["result_json"]["content"] != tool)
    return copy.deepcopy(value)


def supplement_satisfied(checkpoint: dict[str, Any]) -> None:
    """Check the exact committed search and all specified definitions."""
    steps = checkpoint["steps"]
    require(len(steps) >= 2)
    require(steps[-1]["step"]["kind"] == "search_policy")
    require(steps[-2]["step"]["kind"] == "model_proposal")
    value = steps[-2]["result_json"]["content"]
    validate_supplement(value, dict(checkpoint, steps=steps[:-2]))
    sources = support_sources(checkpoint, repeated_search=True)
    require(all(alias in sources.aliases for alias in value["required_aliases"]))
