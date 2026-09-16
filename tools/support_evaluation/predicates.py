"""Reviewed policy predicates over bound returned sources, never case labels."""

from __future__ import annotations

from dataclasses import dataclass
from datetime import timedelta
from itertools import combinations
from typing import Any

from tools.support_evaluation.validate_data import instant, validate_anchor

CRITICAL = frozenset({"lost", "damaged", "returned_to_sender"})
TRANSIT = frozenset({"in_transit", "out_for_delivery", "recovered", "handed_over"})


@dataclass(frozen=True)
class Facts:
    """Carry only source-derived truth and the priority-selected required claims."""

    ticket: dict[str, Any]
    order: dict[str, Any] | None
    delivery: dict[str, Any] | None
    events: dict[str, dict[str, Any]]
    active_ids: frozenset[str]
    corrections: frozenset[tuple[str, str]]
    conflicts: frozenset[tuple[str, frozenset[str]]]
    dispute_types: frozenset[str]
    critical_ids: frozenset[str]
    missing: frozenset[str]
    timing: str | None
    timing_ids: frozenset[str]
    expected: dict[str, Any]
    required: frozenset[str]


def derive_facts(
    ticket: dict[str, Any],
    order: dict[str, Any] | None,
    delivery: dict[str, Any] | None,
    anchors: list[dict[str, Any]],
) -> Facts:
    """Recompute UTC boundaries, active events and policy priority from evidence."""
    events = {e["event_id"]: e for e in delivery["events"]} if delivery else {}
    corrections: set[tuple[str, str]] = set()
    source_keys: dict[str, str] = {}
    disputes: set[str] = set()
    vague = False
    for anchor in anchors:
        source = anchor["source"]
        entity = ticket if source["entity_kind"] == "ticket" else delivery
        key = "ticket_id" if source["entity_kind"] == "ticket" else "delivery_id"
        if entity is None or (source["tenant_id"], source["entity_id"]) != (
            entity["tenant_id"],
            entity[key],
        ):
            continue
        document = (
            ticket
            if key == "ticket_id"
            else {"kind": "delivery", "missing": False, "delivery": delivery}
        )
        validate_anchor(anchor, document)
        meaning = anchor["meaning"]
        if anchor["kind"] == "customer_dispute":
            disputes.update(meaning["supported_types"])
        elif anchor["kind"] == "vague_problem":
            vague = True
        elif anchor["kind"] == "carrier_source_key":
            source_keys[source["event_id"]] = meaning["source_key"]
        else:
            corrections.add((source["event_id"], meaning["corrected_event_id"]))
    corrected = {old for _, old in corrections}
    active = {key: event for key, event in events.items() if key not in corrected}
    latest = max((instant(e["occurred_at"]) for e in active.values()), default=None)
    current = {key for key, e in active.items() if instant(e["occurred_at"]) == latest}
    delivered = {key for key, e in active.items() if e["status"] == "delivered"}
    conflicts: set[tuple[str, frozenset[str]]] = set()
    for first, second in combinations(active, 2):
        a, b = active[first], active[second]
        pair = frozenset({first, second})
        at, bt = instant(a["occurred_at"]), instant(b["occurred_at"])
        # These are the independently reviewed incompatible status pairs in
        # this frozen corpus. A new pair needs a new source registration; two
        # different lifecycle labels are not automatically contradictory.
        if (
            frozenset({a["status"], b["status"]})
            in {
                frozenset({"delivered", "lost"}),
                frozenset({"delivered", "in_transit"}),
            }
            and at == bt == latest
        ):
            conflicts.add(("same_time", pair))
        if (
            a["status"] != b["status"]
            and source_keys.get(first) is not None
            and (source_keys.get(first) == source_keys.get(second))
        ):
            conflicts.add(("source_key", pair))
        for x, y in ((a, b), (b, a)):
            if x["status"] == "delivered" and instant(x["occurred_at"]) < instant(
                y["occurred_at"]
            ):
                if y["status"] == "handed_over":
                    conflicts.add(("pre_handover", pair))
                if y["status"] == "in_transit":
                    conflicts.add(("post_delivery", pair))
    if (
        order
        and delivery
        and order["status"] == "fulfilled"
        and delivery["status"] == "in_transit"
    ):
        conflicts.add(("order_delivery", frozenset()))
    critical = {key for key in active if active[key]["status"] in CRITICAL}
    missing: set[str] = set()
    if ticket["order_id"] is None or order is None:
        missing.add("ticket.order_id")
    elif order["delivery_id"] is None or delivery is None:
        missing.add("order.delivery_id")
    elif not active:
        missing.add("delivery.usable_tracking_events")
    elif delivery["status"] == "delivered" and not delivered:
        missing.add("delivery.delivered_event")
    if vague:
        missing.add("ticket.problem_description")
    timing: str | None = None
    timing_ids: set[str] = set()
    if order and delivery and active:
        promise, observed = (
            instant(order["promised_delivery_at"]),
            instant(ticket["observed_at"]),
        )
        if delivered:
            # Multiple delivered scans are only equivalent when they prove the
            # same earliest actual fulfillment; later scans do not reset timing.
            completed = min(instant(active[key]["occurred_at"]) for key in delivered)
            timing_ids = {
                key
                for key in delivered
                if instant(active[key]["occurred_at"]) == completed
            }
            timing = "delivered_not_late" if completed <= promise else "delivered_late"
        else:
            timing_ids = current
            timing = (
                "outstanding_not_overdue"
                if observed <= promise
                else "outstanding_overdue_ge48"
                if observed - promise >= timedelta(hours=48)
                else "outstanding_overdue_lt48"
            )
    required: set[str] = set()
    action, conclusion = "record_conclusion", "on_time"
    fields: list[str] = []
    if conflicts:
        action, conclusion = "escalate", "conflicting"
        required.add("conflict")
    elif delivered and disputes:
        action, conclusion = "escalate", "disputed"
        required.add("dispute")
    elif critical:
        action = "escalate"
        conclusion = (
            "delayed"
            if timing in {"outstanding_overdue_lt48", "outstanding_overdue_ge48"}
            else "insufficient"
        )
        required.update({"critical", "timing"})
    elif missing:
        action, conclusion, fields = (
            "request_information",
            "insufficient",
            sorted(missing),
        )
        required.update("missing:" + field for field in missing)
    else:
        required.add("timing")
        conclusion = (
            "on_time"
            if timing in {"delivered_not_late", "outstanding_not_overdue"}
            else "delayed"
        )
        if timing == "outstanding_overdue_ge48":
            action = "escalate"
        elif ticket["status"] == "informational_only":
            action = ""
            required.add("ticket_status:informational_no_action")
        elif ticket["status"] == "escalated":
            required.add("ticket_status:preserve_escalated")
        if corrections:
            required.add("correction")
    target = {
        "escalate": "escalated",
        "request_information": "awaiting_information",
    }.get(action, ticket["status"])
    return Facts(
        ticket,
        order,
        delivery,
        events,
        frozenset(active),
        frozenset(corrections),
        frozenset(conflicts),
        frozenset(disputes),
        frozenset(critical),
        frozenset(missing),
        timing,
        frozenset(timing_ids),
        {
            "decision": "proposal" if action else "no_action",
            "action": action,
            "conclusion": conclusion,
            "requested_fields": fields,
            "target_ticket_status": target,
        },
        frozenset(required),
    )


def claim_truth(claim: dict[str, Any], facts: Facts) -> bool:
    """Evaluate every claim independently, including extra claims beyond coverage."""
    kind = claim["kind"]
    if kind == "timing":
        return claim["test"] == facts.timing and claim["event_id"] in facts.timing_ids
    if kind == "dispute":
        event = facts.events.get(claim["delivered_event_id"])
        return bool(
            event
            and event["status"] == "delivered"
            and claim["delivered_event_id"] in facts.active_ids
            and claim["type"] in facts.dispute_types
        )
    if kind == "critical":
        return (
            claim["event_id"] in facts.critical_ids
            and facts.events[claim["event_id"]]["status"] == claim["status"]
        )
    if kind == "missing":
        return claim["field"] in facts.missing
    if kind == "conflict":
        return (claim["type"], frozenset(claim["event_ids"])) in facts.conflicts
    if kind == "correction":
        return (
            claim["recovery_event_id"],
            claim["corrected_event_id"],
        ) in facts.corrections
    if kind == "ticket_status":
        return (
            claim["mode"] == "informational_no_action"
            and facts.expected["decision"] == "no_action"
        ) or (
            claim["mode"] == "preserve_escalated"
            and facts.ticket["status"] == "escalated"
            and facts.expected["action"] == "record_conclusion"
        )
    return False


def coverage_key(claim: dict[str, Any]) -> str:
    """Identify one required semantic class, preserving explicit alternatives."""
    kind: str = claim["kind"]
    if kind == "missing":
        return kind + ":" + str(claim["field"])
    if kind == "ticket_status":
        return kind + ":" + str(claim["mode"])
    return kind


def claim_sources(claim: dict[str, Any], facts: Facts, refs: set[str]) -> bool:
    """Require applicable returned paragraphs and the concrete supporting fields.

    refs contains validated short aliases reconstructed from persisted pointers.
    Event-node references inserted by the trusted expander are checked separately
    during persisted-proposal verification and cannot fabricate an event.
    """
    kind = claim["kind"]
    policies: set[str]
    groups: list[set[str]] = []
    if kind == "timing":
        test = claim["test"]
        policies = {"P02.1", "P02.2"}
        if test == "outstanding_overdue_ge48":
            policies = {"P07.1"}
        elif test == "outstanding_overdue_lt48":
            policies.add("P07.1")
        elif test == "delivered_late":
            policies.add("P07.2")
        groups.append({"E1#/order/promised_delivery_at"})
        if test.startswith("outstanding"):
            groups.extend([{"T#/observed_at"}, {"E2#/delivery/events"}])
    elif kind == "dispute":
        policies = {"P04.1", "P04.2"}
        groups.append({"T#/description"})
    elif kind == "critical":
        policies = {"P06.1", "P10.1"}
        groups.append({"E2#/delivery/events", "E2#/delivery/status"})
    elif kind == "missing":
        policies = {"P03.1", "P03.2"}
        field = claim["field"]
        if field == "ticket.order_id":
            groups.append({"T#/order_id", "E1#/missing", "E1#/missing_reason"})
        elif field == "order.delivery_id":
            groups.append(
                {"E1#/order/delivery_id", "E2#/missing", "E2#/missing_reason"}
            )
        elif field == "ticket.problem_description":
            groups.append({"T#/description"})
        else:
            groups.append({"E2#/delivery/events"})
            if field == "delivery.delivered_event":
                policies.add("P02.2")
                groups.append({"E2#/delivery/status"})
    elif kind == "conflict":
        policies = {"P05.1", "P05.2", "P10.1"}
        if claim["type"] == "order_delivery":
            groups.extend([{"E1#/order/status"}, {"E2#/delivery/status"}])
        elif claim["type"] == "same_time":
            groups.append({"E2#/delivery/events"})
    elif kind == "correction":
        policies = {"P05.2", "P06.2"}
    else:
        policies = (
            {"P08.1"}
            if claim["mode"] == "informational_no_action"
            else {"P08.2", "P09.1"}
        )
        groups.append({"T#/status"})
    return bool(refs & policies) and all(refs & choices for choices in groups)
