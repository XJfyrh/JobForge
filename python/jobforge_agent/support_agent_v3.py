"""Versioned policy-condition review over the unchanged accepted evidence."""

from __future__ import annotations

from collections.abc import Mapping, Sequence

from jobforge_agent.dispatch import DispatchError
from jobforge_agent.runtime_input import RuntimeCheckpoint
from jobforge_agent.support_agent_v2 import SupportAgentV2Adapter

PROMPT_VERSION = "support-agent-prompt-v3"

CONDITION_REVIEW = """
POLICY-CONDITION REVIEW (before choosing final):
Treat every proposed claim as a hypothesis. Read its actual defining policy and check ALL necessary conditions against the captured fields. Navigation labels and correction/priority paragraphs do not substitute for a missing definition. If a hypothesis fails, continue investigating the remaining facts and retrieve their applicable policies before final. Do not manufacture a conflict to avoid another search.

CONFLICT: Retrieve the conflict_definition paragraph before asserting a structured conflict. Apply its actual wording. For source_key compare the COMPLETE key identified in BOTH events' factual notes, including any namespace or prefix, then check whether the actual statuses differ. Similar suffixes and repeated identical statuses do not establish this conflict. For same_time compare UTC instants and check that the pair belongs to the LATEST ACTIVE timestamp group; two different lifecycle labels are not automatically incompatible. For order_delivery check the exact captured ORDER status and current DELIVERY status required by the definition, not an event's status or an approximate synonym. Distinguish each defined conflict type and cite the pair that proves that type.

CORRECTION: A named event is corrected only if the factual note on an actual STRICTLY LATER event explicitly corrects it and both events belong to this returned delivery. Compare UTC instants, not array positions or timestamp spelling. Equal instants, a note earlier than its target, a target absent from this delivery, and ordinary later scans cannot establish that relation. Keep every uncorrected exception in consideration even when a newer normal scan exists; retrieve the active-exception policy if necessary. Determine the active timeline from the retrieved correction policy before selecting timing events.

TIMING: Use the existing time_differences values after selecting the valid timing event. For completed delivery, positive event_minus_promise_seconds means delivered_late, while zero or negative means delivered_not_late. For outstanding delivery, classify observed_minus_promise_seconds: <=0 is outstanding_not_overdue; >0 and <172800 is outstanding_overdue_lt48; >=172800 is outstanding_overdue_ge48. Retrieve the applicable timing policy separately. A captured informational_only status may affect the action but cannot turn a positive late-delivery duration into on_time. Check conclusion independently from decision/action.

Only after these checks, return the existing compact tool/final JSON. The facts, retrieved policy and original six-field contract remain authoritative. Add no reasoning text, checklist fields or extra claims to the output.
"""


class SupportAgentV3Adapter(SupportAgentV2Adapter):
    """Add a source-grounded review instruction without evaluating policy truth."""

    def proposal_messages(
        self, checkpoint: RuntimeCheckpoint, *, correction: bool
    ) -> Sequence[Mapping[str, object]]:
        """Keep every v2 fact and citation; select no tool or business answer."""
        original = super().proposal_messages(checkpoint, correction=correction)
        instructions = str(original[0]["content"]) + CONDITION_REVIEW
        body = str(original[1]["content"])
        if len((instructions + body).encode("utf-8")) > 65536:
            raise DispatchError("OUTPUT_INVALID", fact="size_limit", stop=True)
        return [
            {"role": "system", "content": instructions},
            {"role": "user", "content": body},
        ]
