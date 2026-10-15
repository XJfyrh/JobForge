"""Keep v8 execution with the exact diagnostic-supported final prompt."""

from __future__ import annotations

from collections.abc import Mapping, Sequence
from typing import cast

from jobforge_agent.runtime_input import RuntimeCheckpoint
from jobforge_agent.support_agent_v5 import SupportAgentV5Adapter
from jobforge_agent.support_agent_v8 import FINAL_SYSTEM as V8_FINAL_SYSTEM
from jobforge_agent.support_agent_v8 import SupportAgentV8Adapter

PROMPT_VERSION = "support-agent-prompt-v9"

FINAL_SYSTEM = (
    V8_FINAL_SYSTEM
    + "\nCRITICAL AND TIMING COMPLETENESS:\n"
    + "An active critical finding needs BOTH a critical claim and a separate timing claim when timing is supported by the captured facts. The critical claim establishes the exception; the timing claim establishes whether the promise was missed. A future promise still needs its outstanding_not_overdue timing claim even when the critical exception makes the conclusion insufficient. Each claim needs its own supporting fields and policy. Do not add timing to a conflict or dispute merely to fill slots.\n"
    + "\nOUTSTANDING TIMING EVIDENCE:\n"
    + "Every outstanding timing claim must cite E2#/delivery/events in its own refs: the full event list proves the absence of a delivered event. A delivery status, a single event or references on another claim cannot replace that aggregate reference.\n"
)


class SupportAgentV9Adapter(SupportAgentV8Adapter):
    """Preserve investigation and validation; replace only final instructions."""

    def proposal_messages(
        self, checkpoint: RuntimeCheckpoint, *, correction: bool
    ) -> Sequence[Mapping[str, object]]:
        """Apply v8's guards and the original size bound to the new final text."""
        messages = super().proposal_messages(checkpoint, correction=correction)
        if checkpoint["next_step"]["kind"] != "model_proposal":
            return messages
        return SupportAgentV5Adapter._bounded(
            FINAL_SYSTEM, cast(str, messages[1]["content"])
        )
