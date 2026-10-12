"""Final-only adjudication projection plus condition-driven investigation review.

v5 keeps every accepted fact, tool, source contract and validation rule from
v1-v4 unchanged. It changes only what each of the two model stages is shown:

* the final stage receives a self-contained adjudication request with no tool
  protocol, no tool budget and no search navigation, so nothing in the prompt or
  the payload can invite a tool decision that this step cannot execute;
* the investigation stage must classify every condition class before closing and
  retrieve a missing definition, instead of only gathering support for claims it
  already intends to make.

The prompt constants below are deliberately self-contained. Older prompt
versions stay byte-frozen, so v5 owns its own text rather than editing shared
constants.
"""

from __future__ import annotations

import copy
import json
from collections.abc import Mapping, Sequence
from typing import Any

from jobforge_agent.dispatch import DispatchError
from jobforge_agent.runtime_input import RuntimeCheckpoint, is_investigation_complete
from jobforge_agent.support_adapter import SYSTEM_INSTRUCTIONS
from jobforge_agent.support_agent_v3 import SupportAgentV3Adapter
from jobforge_agent.support_agent_v4 import SupportAgentV4Adapter
from jobforge_agent.support_contract import require

PROMPT_VERSION = "support-agent-prompt-v5"

# Defining paragraphs per navigation topic, taken from the existing role table.
# A topic may also list supporting aliases; only these decide whether the class
# definition is actually available. This is navigation metadata, never a policy
# predicate: nothing here evaluates whether a condition holds.
DEFINITION_ALIASES: dict[str, tuple[str, ...]] = {
    "timing": ("P02.1", "P02.2"),
    "extended_delay": ("P07.1", "P07.2"),
    "missing_facts": ("P03.1", "P03.2"),
    "delivery_dispute": ("P04.1", "P04.2"),
    "structured_conflict": ("P05.1",),
    "critical_exception": ("P06.1",),
    "carrier_correction": ("P05.2", "P06.2"),
    "ticket_status": ("P08.1", "P08.2", "P09.1"),
}

# Payload keys that only make sense while tools are still available. They are
# removed from the final projection; every remaining key is kept verbatim.
FINAL_EXCLUDED_FACTS: tuple[str, ...] = (
    "previous_tools",
    "remaining_tools",
    "completed_requests",
    "policy_navigation",
    "policy_retrieval",
)

CONDITION_DEFINITIONS = """
CONDITION DEFINITIONS (these define terms the retrieved policy already uses; they add no new rule):
COMPLETED VERSUS OUTSTANDING: The timing branch is decided ONLY by the active event timeline of this returned delivery, never by the aggregate delivery status. An event is active when no factual carrier note on a strictly later event of this delivery explicitly corrects it. If any active event has status delivered, delivery is COMPLETED: select the earliest active delivered event, use its event_minus_promise_seconds, and the outstanding overdue thresholds do not apply. This remains true when the aggregate delivery status is lost, damaged or returned_to_sender. Delivery is OUTSTANDING only when no active delivered event exists; then classify observed_minus_promise_seconds. A critical exception recorded after a valid delivery does not make delivery outstanding, and an aggregate exception status does not cancel an existing active delivered event.
CRITICAL EXCEPTION CONCLUSION: When an active critical exception governs, the conclusion is delayed only if the applicable timing test is outstanding_overdue_lt48 or outstanding_overdue_ge48. Otherwise the conclusion is insufficient, because the retrieved exception policy conditions delayed on a missed promise with delivery outstanding. The timing claim still reports the actual completed-delivery result; never restate it as an outstanding branch to match the conclusion, and never drop it.
"""

CONDITION_ENUMERATION = """
CONDITION ENUMERATION (before returning the closed investigation marker):
Work through these condition classes in this exact order, each with the policy_retrieval topic key that carries its definition: structured contradiction = structured_conflict; valid delivered recipient dispute = delivery_dispute; active critical exception = critical_exception; necessary missing facts = missing_facts; timing = timing (its 48-hour and completed-late thresholds live under extended_delay). For EACH class decide exactly one state from the captured fields alone: ESTABLISHED (the captured facts satisfy it), EXCLUDED (the captured facts contradict it), or UNDETERMINED (the captured fields are not sufficient to decide, or deciding would require a rule you have not retrieved).
Excluding a class requires a concrete captured fact: an absent status, an absent event pair, a present relationship, or the specific ticket text. Silence and "I saw no problem" are not exclusions.
If a class is ESTABLISHED or UNDETERMINED and its defining paragraph is not in available_refs, your next response must be one focused search_policy for that definition, provided remaining_tools is positive. Decide this from policy_retrieval[topic].definition_retrieved: a topic whose definition_retrieved is false is NOT covered, even when its retrieved list is non-empty because a sibling paragraph is present.
An EXCLUDED class needs no retrieval. Do not retrieve a class merely because navigation lists it, and do not retrieve the whole catalog.
Only when every one of the five classes is either ESTABLISHED with its definition in available_refs, or EXCLUDED, may you return the closed investigation marker. If remaining_tools is zero and a class is still UNDETERMINED, decide it from the captured fields and the policy already in available_refs, then continue; never invent an alias and never treat an undetermined class as excluded without a concrete fact.
"""

NAVIGATION_CLARITY = """
NAVIGATION MEANING: policy_retrieval[topic].retrieved lists every alias of that topic already in available_refs, while definition_retrieved states whether the paragraph that DEFINES that class is among them. policy_navigation[role].missing_aliases remains navigation rather than evidence, and still does not require fetching every role. A missing defining alias blocks ASSERTING that class: never assert a claim whose defining paragraph is absent, and never treat a topic as covered merely because a sibling paragraph is present. It does not by itself block EXCLUDING that class, because an exclusion that rests on a concrete captured fact needs no policy. However, if deciding whether the class applies would require reading a rule you have not retrieved, the honest state is UNDETERMINED, not EXCLUDED.
"""

INVESTIGATION_ADDITIONS = (
    CONDITION_ENUMERATION + CONDITION_DEFINITIONS + NAVIGATION_CLARITY
)

FINAL_ENVELOPE = """
OUTPUT ENVELOPE FOR THIS STEP: The six fields described above are exactly the content of proposal. Return one object {"type":"final","proposal":{...}} and nothing else. Investigation and retrieval have ended: no tool exists in this step, no additional search is possible, and any tool object is an error. There is no prior proposal to endorse or reject; decide independently from the committed facts and the actually retrieved policy in the user JSON.
CAPACITY FOR THIS STEP: the "within 1024 output tokens" figure in the shared contract above does NOT apply here. This call has a much larger completion capacity that is shared with your private reasoning; the returned final content is still limited to 16 KiB, so keep that JSON compact and put no reasoning, checklist or commentary in it.
"""

FINAL_AGENT_CONTRACT = """
AGENT FIELD NAMING: decision is proposal, except no_action uses action="". proposal action is record_conclusion, request_information or escalate. These correspond to policy record_resolution, request_information and escalate_human. conclusion is on_time, delayed, disputed, insufficient or conflicting. record_conclusion preserves T.status; request_information targets awaiting_information; escalate targets escalated. no_action preserves T.status. requested_fields contains only actually missing necessary facts, otherwise [].
Ordinary open inquiries need record_conclusion even when no escalation is needed. no_action is available only for an ALREADY informational_only ticket whose status policy is in available_refs; on_time is not a reason by itself for no_action.

KEEP CLAIMS MINIMAL (1-4). A claim asserts a FACT, not a proposed status. Do not add a timing claim to a dispute/conflict merely to fill slots. Do not add a ticket_status claim to explain an escalation. Read the retrieved policies to decide priority: structured contradiction, valid delivered dispute, active critical exception, necessary missing facts, then timing/extended delay. For a conflict or dispute use its claim alone; a critical finding also needs timing to establish whether the promise was missed. Normal timing needs a correction claim only if a genuine carrier correction affected the active timeline, and a ticket_status claim only when an actual special status affects the action.
Missing relationships are hierarchical: no related order -> ticket.order_id; order without delivery association -> order.delivery_id; an existing delivery with no events -> delivery.usable_tracking_events; delivered aggregate with no delivered event -> delivery.delivered_event. Do not request all downstream missing fields after one missing relationship. A vague actual problem report needs ticket.problem_description even if tracking is normal; a specific request about timing is not vague. Other substantive escalation findings take policy priority over missing clarification.

CITATION CHECKLIST:
- Every timing claim cites E1#/order/promised_delivery_at. Every outstanding timing claim ALSO cites T#/observed_at AND E2#/delivery/events (the aggregate list proves no delivery), not just an event ID. Retrieved timing policy P02 supports promised-time/fulfillment; overdue-48h needs P07.1; completed-late may use P07.2. P06 about exceptions cannot establish the timing calculation by itself.
- Dispute cites T#/description and retrieved recipient-dispute P04.
- Critical cites E2#/delivery/status or E2#/delivery/events and retrieved active-carrier-exception P06.1 (or decision-priority P10.1); its timing claim needs a separate timing policy.
- Missing cites the missing relationship field or missing flag; event absence cites E2#/delivery/events. Missing delivered event also cites E2#/delivery/status, supported by P03 (P02.2 also supports missing delivered event).
- Conflict cites both actual conflicting facts; same_time ALSO cites E2#/delivery/events; order_delivery cites E1#/order/status and E2#/delivery/status, supported by authoritative-record-conflict P05 or decision-priority P10.1.
- Correction cites its actual two event IDs and retrieved explicit-carrier-correction P05.2/P06.2; the corrected timeline's timing claim needs a timing policy.
- informational_no_action cites T#/status and retrieved informational-only P08.1. preserve_escalated cites T#/status and retrieved existing-escalation/status-preservation P08.2/P09.1. A timing paragraph cannot support either status claim.
Each included claim must be independently true and cite its own relevant retrieved policy and concrete fields. If the applicable policy is not in available_refs, do not substitute an unrelated paragraph. Avoid unnecessary claims. Keep the final JSON compact.

INDEPENDENT GAPS: Assert the union of independently missing necessary facts, with their own applicable policies. Preserve the hierarchy: an absent relationship blocks requests for its downstream fields. An actual vague problem description is independent of missing tracking relationships; do not add a missing claim merely because navigation mentioned it.
CORRECTION SOURCE: A factual carrier note must identify the earlier event it corrects. A later normal scan or customer prose cannot establish a correction. Determine the active timeline from the retrieved correction policy.
ACTIVE EVENT: For completed delivery use the earliest active delivered event; for outstanding delivery use the latest active event. Use the accepted UTC time differences and the actual retrieved policy, never an older exception solely because it is critical.
"""

FINAL_RECHECK = """
FINAL CONDITION REVIEW:
Treat every claim you are about to include as a hypothesis. Read its actual defining retrieved policy and check ALL necessary conditions against the captured fields. Navigation labels and correction/priority paragraphs do not substitute for a missing definition.
CONFLICT: Apply the actual wording of the retrieved conflict definition. For source_key compare the COMPLETE key identified in BOTH events' factual notes, including any namespace or prefix, then check whether the actual statuses differ. Similar suffixes and repeated identical statuses do not establish this conflict. For same_time compare UTC instants and check that the pair belongs to the LATEST ACTIVE timestamp group; two different lifecycle labels are not automatically incompatible. For order_delivery check the exact captured ORDER status and current DELIVERY status required by the definition, not an event's status or an approximate synonym. Distinguish each defined conflict type and cite the pair that proves that type. Do not discard a conflicting event as a duplicate, and do not treat one corrected event as removing the other active events.
CORRECTION: A named event is corrected only if the factual note on an actual STRICTLY LATER event explicitly corrects it and both events belong to this returned delivery. Compare UTC instants, not array positions or timestamp spelling. Equal instants, a note earlier than its target, a target absent from this delivery, and ordinary later scans cannot establish that relation. Keep every uncorrected exception in consideration even when a newer normal scan exists.
TIMING: Use the existing time_differences values after selecting the valid timing event, applying COMPLETED VERSUS OUTSTANDING above. For completed delivery, positive event_minus_promise_seconds means delivered_late, while zero or negative means delivered_not_late. For outstanding delivery, classify observed_minus_promise_seconds: <=0 is outstanding_not_overdue; >0 and <172800 is outstanding_overdue_lt48; >=172800 is outstanding_overdue_ge48. A captured informational_only status may affect the action but cannot turn a positive late-delivery duration into on_time. Check conclusion independently from decision/action.
Recheck the actual conditions and priority of every applicable retrieved policy before selecting the conclusion and action. Reason privately; include no reasoning, checklist or commentary in the final content.
"""

FINAL_SYSTEM = (
    SYSTEM_INSTRUCTIONS
    + FINAL_ENVELOPE
    + FINAL_AGENT_CONTRACT
    + CONDITION_DEFINITIONS
    + FINAL_RECHECK
)

# Tool affordances that must never reach the final stage.
FORBIDDEN_FINAL_TOKENS: tuple[str, ...] = (
    '"type":"tool"',
    "search_policy",
    "get_order",
    "get_delivery",
    "remaining_tools",
    "previous_tools",
    "completed_requests",
    "policy_navigation",
    "policy_retrieval",
    "NEXT-DECISION CHECK",
    "suggested_query",
    "missing_aliases",
)


def definition_retrieved(topic: str, retrieved: object) -> bool:
    """Report whether a topic's DEFINING paragraph is available.

    A topic's alias set can contain supporting paragraphs shared with a sibling
    topic, so a non-empty retrieved list does not prove the class definition was
    read. This reads only navigation metadata; it never evaluates policy truth.
    """
    aliases = DEFINITION_ALIASES[topic]
    present = retrieved if isinstance(retrieved, list) else []
    return any(alias in present for alias in aliases)


class SupportAgentV5Adapter(SupportAgentV4Adapter):
    """Separate the two stages' tasks without touching any accepted contract."""

    def proposal_messages(
        self, checkpoint: RuntimeCheckpoint, *, correction: bool
    ) -> Sequence[Mapping[str, object]]:
        """Project a final-only adjudication or an enumerated investigation."""
        if checkpoint["next_step"]["kind"] == "model_proposal":
            require(not correction)
            return self._final_messages(checkpoint)
        return self._investigation_messages(checkpoint, correction=correction)

    def _investigation_messages(
        self, checkpoint: RuntimeCheckpoint, *, correction: bool
    ) -> Sequence[Mapping[str, object]]:
        """Keep every v4 investigation fact and add only navigational clarity."""
        original = super().proposal_messages(checkpoint, correction=correction)
        instructions = str(original[0]["content"]) + INVESTIGATION_ADDITIONS
        facts: dict[str, Any] = json.loads(str(original[1]["content"]))
        for topic, entry in facts["policy_retrieval"].items():
            entry["definition_retrieved"] = definition_retrieved(
                topic, entry["retrieved"]
            )
        body = json.dumps(
            facts, ensure_ascii=False, allow_nan=False, separators=(",", ":")
        )
        return self._bounded(instructions, body)

    def _final_messages(
        self, checkpoint: RuntimeCheckpoint
    ) -> Sequence[Mapping[str, object]]:
        """Drop the draft, then drop every tool affordance from the projection."""
        require(bool(checkpoint["steps"]))
        require(
            is_investigation_complete(checkpoint["steps"][-1]["result_json"]["content"])
        )
        evidence = copy.deepcopy(checkpoint)
        evidence["steps"] = evidence["steps"][:-1]
        # Bypass v4 so the final stage is constructed here, not appended to.
        original = SupportAgentV3Adapter.proposal_messages(
            self, evidence, correction=False
        )
        facts: dict[str, Any] = json.loads(str(original[1]["content"]))
        for key in FINAL_EXCLUDED_FACTS:
            facts.pop(key, None)
        facts["final_stage"] = True
        body = json.dumps(
            facts, ensure_ascii=False, allow_nan=False, separators=(",", ":")
        )
        return self._bounded(FINAL_SYSTEM, body)

    @staticmethod
    def _bounded(instructions: str, body: str) -> Sequence[Mapping[str, object]]:
        """Apply the original message-size contract to both stages."""
        if len((instructions + body).encode("utf-8")) > 65536:
            raise DispatchError("OUTPUT_INVALID", fact="size_limit", stop=True)
        return [
            {"role": "system", "content": instructions},
            {"role": "user", "content": body},
        ]
