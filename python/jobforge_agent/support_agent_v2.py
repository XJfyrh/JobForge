"""Versioned evidence navigation; Go still owns tools, limits and decisions."""

from __future__ import annotations

import json
from collections.abc import Mapping, Sequence
from typing import Any

from jobforge_agent.dispatch import DispatchError
from jobforge_agent.runtime_input import RuntimeCheckpoint
from jobforge_agent.support_agent import (
    POLICY_TOPICS,
    SupportAgentAdapter,
    tool_decision,
)

PROMPT_VERSION = "support-agent-prompt-v2"

# These are navigation roles, not policy evidence or evaluated claim predicates.
POLICY_ROLES: dict[str, tuple[tuple[str, ...], str]] = {
    "timing_definition": (
        ("P02.1",),
        POLICY_TOPICS["timing"][1],
    ),
    "missing_delivered_event": (
        ("P02.2",),
        "Delivered aggregate without a valid delivered event missing delivery evidence",
    ),
    "outstanding_delay": (
        ("P07.1",),
        "Undelivered outstanding order at least 48 hours after promised delivery escalation",
    ),
    "completed_delay": (
        ("P07.2",),
        "Completed late delivery record timing conclusion without outstanding escalation",
    ),
    "missing_facts": (
        ("P03.1", "P03.2"),
        POLICY_TOPICS["missing_facts"][1],
    ),
    "recipient_dispute": (
        ("P04.1", "P04.2"),
        "Delivered customer disputes receipt wrong address unauthorized recipient or safe place",
    ),
    "conflict_definition": (
        ("P05.1",),
        "Authoritative structured conflict pre handover same time source key post delivery order delivery contradiction definition",
    ),
    "carrier_correction": (
        ("P05.2", "P06.2"),
        POLICY_TOPICS["carrier_correction"][1],
    ),
    "critical_exception": (
        ("P06.1",),
        POLICY_TOPICS["critical_exception"][1],
    ),
    "informational_status": (
        ("P08.1",),
        "Captured informational_only ticket no action when no substantive escalation or clarification",
    ),
    "preserve_escalation": (
        ("P08.2", "P09.1"),
        "Captured already escalated ticket preserve status record resolution without silent downgrade",
    ),
    "decision_priority": (
        ("P10.1",),
        "Decision priority structured conflict delivered recipient dispute critical exception missing necessary facts timing",
    ),
}

VERSIONED_GUIDANCE = """
VERSIONED NAVIGATION: Keep the original policy_retrieval catalog and previous_tools meanings. policy_navigation splits those topics into focused searches: retrieved contains only actually available aliases; missing_aliases is navigation, not evidence and not a requirement to fetch every role. Read the actual relevant paragraphs and continue searching for each needed claim, using the original next-decision check. You may choose a different focused query. completed_requests lists only already committed reads with their exact arguments; do not repeat those requests.
INDEPENDENT GAPS: Request the union of independently missing necessary facts, with their own applicable policies. Preserve the original hierarchy: an absent relationship blocks requests for its downstream fields. An actual vague problem description is independent of missing tracking relationships; do not add a missing claim merely because that kind appears in the catalog.
CORRECTION SOURCE: A factual carrier note must identify the earlier event it corrects. A later normal scan or customer prose cannot establish a correction. Use the retrieved policy to determine the active timeline.
ACTIVE EVENT: For completed delivery use the earliest active delivered event; for outstanding delivery use the latest active event. Use the accepted UTC time differences and actual retrieved policy, never an older exception solely because it is critical.
OUTER CONTRACT: Return one outer object with literal type tool or final. A final object is exactly {"type":"final","proposal":{...}}; proposal.decision is proposal or no_action. Keep claims minimal under the original contract, never fill a claim kind just because navigation mentions it.
"""


class SupportAgentV2Adapter(SupportAgentAdapter):
    """Keep v1 validation while presenting versioned source-only navigation."""

    def proposal_messages(
        self, checkpoint: RuntimeCheckpoint, *, correction: bool
    ) -> Sequence[Mapping[str, object]]:
        """Reuse factual projection without inferring policy applicability."""
        original = super().proposal_messages(checkpoint, correction=correction)
        facts: dict[str, Any] = json.loads(str(original[1]["content"]))
        completed: list[dict[str, Any]] = []
        for index, accepted in enumerate(checkpoint["steps"]):
            kind = accepted["step"]["kind"]
            if kind in {"get_order", "get_delivery", "search_policy"}:
                # Only a committed tool, not a pending model choice, is complete.
                decision = tool_decision(
                    checkpoint["steps"][index - 1]["result_json"]["content"],
                    checkpoint,
                )
                completed.append({"name": kind, "arguments": decision["arguments"]})
        facts["completed_requests"] = completed
        queries = {
            r["arguments"]["query"] for r in completed if r["name"] == "search_policy"
        }
        available = set(facts["available_refs"])
        facts["policy_navigation"] = {}
        for role, (aliases, query) in POLICY_ROLES.items():
            fetched = [alias for alias in aliases if alias in available]
            missing = [alias for alias in aliases if alias not in available]
            entry: dict[str, Any] = {"retrieved": fetched, "missing_aliases": missing}
            if missing and query not in queries:
                entry["suggested_query"] = query
            facts["policy_navigation"][role] = entry
        instructions = str(original[0]["content"]) + VERSIONED_GUIDANCE
        body = json.dumps(
            facts, ensure_ascii=False, allow_nan=False, separators=(",", ":")
        )
        if len((instructions + body).encode("utf-8")) > 65536:
            raise DispatchError("OUTPUT_INVALID", fact="size_limit", stop=True)
        return [
            {"role": "system", "content": instructions},
            {"role": "user", "content": body},
        ]
