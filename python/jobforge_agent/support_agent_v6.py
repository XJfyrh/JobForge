"""Independent adjudication with one Go-scheduled, source-checked policy read."""

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
from jobforge_agent.runtime_input import RuntimeCheckpoint, is_investigation_complete
from jobforge_agent.support_adapter import SYSTEM_INSTRUCTIONS
from jobforge_agent.support_agent import POLICY_TOPICS, SupportAgentAdapter
from jobforge_agent.support_agent_v5 import (
    CONDITION_DEFINITIONS,
    DEFINITION_ALIASES,
    FINAL_AGENT_CONTRACT,
    FINAL_EXCLUDED_FACTS,
    FINAL_RECHECK,
    FINAL_SYSTEM,
    SupportAgentV5Adapter,
)
from jobforge_agent.support_contract import require

PROMPT_VERSION = "support-agent-prompt-v6"
SUPPLEMENT_ENVELOPE = """
OUTPUT ENVELOPE: Decide independently from committed facts and retrieved policies; there is no prior proposal to endorse. Return ONLY one JSON object, either {"type":"final","proposal":{...}} with the exact six proposal fields, or {"type":"policy_supplement","topic":<one listed topic>,"required_aliases":[<missing defining paragraph aliases>]}.
Before final, review the actual customer's problem and each applicable condition, not only the tracking outcome. If an applicable definition or a policy needed to substantiate a claim is absent, request one focused policy supplement instead of citing an unrelated paragraph or guessing the rule. A vague actual problem report may require clarification even when tracking is normal; a correction paragraph does not define structured contradictions. Do not request a supplement merely because a topic is listed.
The policy_supplement catalog is navigation, NOT evidence. required_aliases must be a nonempty sorted unique subset of that topic's missing_aliases. Select the particular definition you need, not every paragraph. The host can perform exactly one read-only policy search with a fixed topic query; no other tool or query is available. It will continue only if ALL requested aliases are actually retrieved. No proposal, conclusion or explanation is permitted in a supplement object. If the catalog is empty, only final is allowed.
CAPACITY: The shared contract's 1024 output token figure does not apply to this thinking step. Private reasoning shares a larger completion capacity; returned JSON must remain within 16 KiB. Return no reasoning or commentary.
"""
SUPPLEMENT_SYSTEM = (
    SYSTEM_INSTRUCTIONS
    + SUPPLEMENT_ENVELOPE
    + FINAL_AGENT_CONTRACT
    + CONDITION_DEFINITIONS
    + FINAL_RECHECK
)


class SupportAgentV6Adapter(SupportAgentV5Adapter):
    """Leave v5 investigation unchanged and version only the adjudication edge."""

    def _final_messages(
        self, checkpoint: RuntimeCheckpoint
    ) -> Sequence[Mapping[str, object]]:
        require(bool(checkpoint["steps"]))
        first = is_investigation_complete(
            checkpoint["steps"][-1]["result_json"]["content"]
        )
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
        """Accept a supplement only before any adjudication has committed."""
        if (
            checkpoint["next_step"]["kind"] == "model_proposal"
            and value.get("type") == "policy_supplement"
        ):
            require(bool(checkpoint["steps"]))
            require(
                is_investigation_complete(
                    checkpoint["steps"][-1]["result_json"]["content"]
                )
            )
            return validate_supplement(value, checkpoint)
        return super().validate_proposal(value, checkpoint)
