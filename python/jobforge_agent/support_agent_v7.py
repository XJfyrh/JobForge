"""Independent adjudication after Go's status-policy read prerequisite."""

from __future__ import annotations

import copy
import json
from collections.abc import Mapping, Sequence
from typing import Any

from jobforge_agent.policy_supplement import (
    supplement_satisfied,
    supplement_tool,
    validate_supplement,
)
from jobforge_agent.runtime_input import RuntimeCheckpoint
from jobforge_agent.status_policy import first_adjudication, status_policy_read
from jobforge_agent.support_agent import POLICY_TOPICS, SupportAgentAdapter
from jobforge_agent.support_agent_v5 import (
    DEFINITION_ALIASES,
    FINAL_EXCLUDED_FACTS,
    FINAL_SYSTEM,
    SupportAgentV5Adapter,
)
from jobforge_agent.support_agent_v6 import SUPPLEMENT_SYSTEM
from jobforge_agent.support_contract import require

PROMPT_VERSION = "support-agent-prompt-v7"


class SupportAgentV7Adapter(SupportAgentV5Adapter):
    """Keep the investigation and model tasks, version their evidence boundary."""

    def _final_messages(
        self, checkpoint: RuntimeCheckpoint
    ) -> Sequence[Mapping[str, object]]:
        first = first_adjudication(checkpoint)
        if not first:
            supplement_satisfied(checkpoint)
        evidence = copy.deepcopy(checkpoint)
        evidence["steps"] = [
            s
            for s in evidence["steps"]
            if s["step"]["kind"]
            in {"read_ticket", "get_order", "get_delivery", "search_policy"}
        ]
        messages = SupportAgentAdapter.proposal_messages(
            self, evidence, correction=False
        )
        facts = json.loads(str(messages[1]["content"]))
        remaining = facts.get("remaining_tools", 0)
        for key in FINAL_EXCLUDED_FACTS:
            facts.pop(key, None)
        facts["final_stage"] = True
        if first:
            facts["policy_supplement"] = {
                topic: {
                    "missing_aliases": missing,
                    "description": POLICY_TOPICS[topic][1],
                }
                for topic, aliases in DEFINITION_ALIASES.items()
                if remaining > 0
                and (topic != "ticket_status" or not status_policy_read(checkpoint))
                and all(
                    s["result_json"]["content"]
                    != supplement_tool(
                        {
                            "type": "policy_supplement",
                            "topic": topic,
                            "required_aliases": [aliases[0]],
                        }
                    )
                    for s in checkpoint["steps"]
                )
                and (
                    missing := [a for a in aliases if a not in facts["available_refs"]]
                )
            }
        return self._bounded(
            SUPPLEMENT_SYSTEM if first else FINAL_SYSTEM,
            json.dumps(
                facts, ensure_ascii=False, allow_nan=False, separators=(",", ":")
            ),
        )

    def validate_proposal(
        self, value: dict[str, Any], checkpoint: RuntimeCheckpoint
    ) -> dict[str, Any]:
        """The prerequisite read does not consume the one optional supplement."""
        if checkpoint["next_step"]["kind"] == "model_proposal":
            first = first_adjudication(checkpoint)
            if not first:
                supplement_satisfied(checkpoint)
            if value.get("type") == "policy_supplement":
                require(first)
                return validate_supplement(value, checkpoint)
        return super().validate_proposal(value, checkpoint)
