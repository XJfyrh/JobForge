"""Preserve v3 investigation while isolating a final-only adjudication request."""

from __future__ import annotations

import copy
from collections.abc import Mapping, Sequence
from typing import Any

from jobforge_agent.runtime_input import RuntimeCheckpoint, is_investigation_complete
from jobforge_agent.support_agent_v3 import SupportAgentV3Adapter
from jobforge_agent.support_contract import bounded_json, require

PROMPT_VERSION = "support-agent-prompt-v4"
FINAL_INSTRUCTIONS = """
FINAL ADJUDICATION:
Investigation has ended. Make an independent final decision from the committed facts and actually retrieved policy below. There is no prior proposal to endorse. The investigation instructions above still define the business and source contract, but no tools or additional searches are available in this step. Return ONLY {"type":"final","proposal":{...}} with the existing six proposal fields. Never return a tool decision. Reason privately; include no reasoning, checklist or commentary in final content. Keep the final JSON compact; the separate reasoning capacity is not an invitation to add claims. Recheck the actual conditions and priority of every applicable retrieved policy before selecting the conclusion and action.
"""


class SupportAgentV4Adapter(SupportAgentV3Adapter):
    """Separate an unapproved draft from the source-validated final proposal."""

    def proposal_messages(
        self, checkpoint: RuntimeCheckpoint, *, correction: bool
    ) -> Sequence[Mapping[str, object]]:
        """Remove the draft before any projection, size check or serialization."""
        final = checkpoint["next_step"]["kind"] == "model_proposal"
        if not final:
            return super().proposal_messages(checkpoint, correction=correction)
        require(not correction and bool(checkpoint["steps"]))
        require(
            is_investigation_complete(checkpoint["steps"][-1]["result_json"]["content"])
        )
        evidence = copy.deepcopy(checkpoint)
        evidence["steps"] = evidence["steps"][:-1]
        original = super().proposal_messages(evidence, correction=False)
        instructions = str(original[0]["content"]) + FINAL_INSTRUCTIONS
        body = str(original[1]["content"])
        require(len((instructions + body).encode("utf-8")) <= 65536)
        return [
            {"role": "system", "content": instructions},
            {"role": "user", "content": body},
        ]

    def validate_proposal(
        self, value: dict[str, Any], checkpoint: RuntimeCheckpoint
    ) -> dict[str, Any]:
        """Only the final step applies the existing proposal/source validator."""
        if checkpoint["next_step"]["kind"] == "model_proposal":
            require(value.get("type") == "final")
            return super().validate_proposal(value, checkpoint)
        if value.get("type") != "final":
            return super().validate_proposal(value, checkpoint)
        require(set(value) == {"type", "proposal"})
        marker = {
            "type": "investigation_complete",
            "draft": copy.deepcopy(value["proposal"]),
        }
        bounded_json(marker)
        require(is_investigation_complete(marker))
        return marker
