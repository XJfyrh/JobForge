"""Validate support structure and actual sources without evaluating policy truth."""

from __future__ import annotations

import copy
import json
import re
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any

from jobforge_agent.dispatch import DispatchError
from jobforge_agent.errors import ToolError
from jobforge_agent.http import strict_json

MODEL_FIELDS = {
    "decision",
    "action",
    "conclusion",
    "requested_fields",
    "target_ticket_status",
    "claims",
}
REQUESTED_FIELDS = (
    "ticket.order_id",
    "order.delivery_id",
    "delivery.usable_tracking_events",
    "delivery.delivered_event",
    "ticket.problem_description",
)
POINTERS = {
    "T": (
        "/order_id",
        "/status",
        "/subject",
        "/description",
        "/observed_at",
        "/policy_version",
    ),
    "E1": (
        "/missing",
        "/missing_reason",
        "/order/order_id",
        "/order/delivery_id",
        "/order/status",
        "/order/ordered_at",
        "/order/promised_delivery_at",
    ),
    "E2": (
        "/missing",
        "/missing_reason",
        "/delivery/delivery_id",
        "/delivery/order_id",
        "/delivery/status",
        "/delivery/events",
    ),
}
IDENTIFIER = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:-]{0,127}")
POLICY_ALIAS = re.compile(r"P(0[1-9]|10)\.[12]")
CLAIM_VARIANTS: dict[str, dict[str, tuple[str, ...] | None]] = {
    "timing": {
        "test": (
            "delivered_not_late",
            "delivered_late",
            "outstanding_not_overdue",
            "outstanding_overdue_lt48",
            "outstanding_overdue_ge48",
        ),
        "event_id": None,
    },
    "dispute": {
        "type": (
            "non_receipt",
            "wrong_address",
            "unauthorized_recipient",
            "unauthorized_safe_place",
        ),
        "delivered_event_id": None,
    },
    "critical": {"status": ("lost", "damaged", "returned_to_sender"), "event_id": None},
    "missing": {"field": REQUESTED_FIELDS},
    "conflict": {
        "type": (
            "pre_handover",
            "same_time",
            "source_key",
            "post_delivery",
            "order_delivery",
        ),
        "event_ids": None,
    },
    "correction": {"recovery_event_id": None, "corrected_event_id": None},
    "ticket_status": {"mode": ("informational_no_action", "preserve_escalated")},
}


def require(condition: bool) -> None:
    """Reject protected content with a fixed error that carries no input value."""
    if not condition:
        raise DispatchError("OUTPUT_INVALID")


def bounded_json(value: Any, limit: int = 16384) -> str:
    """Serialize exact content; never truncate a proposal, prompt or source."""
    encoded: bytes | None = None
    try:
        encoded = json.dumps(
            value, ensure_ascii=False, allow_nan=False, separators=(",", ":")
        ).encode("utf-8")
    except (ValueError, TypeError, UnicodeError, RecursionError):
        pass
    require(encoded is not None)
    assert encoded is not None
    if len(encoded) > limit:
        raise DispatchError("OUTPUT_INVALID", fact="size_limit", stop=True)

    def has_nul(item: Any) -> bool:
        if isinstance(item, str):
            return "\x00" in item
        if isinstance(item, dict):
            return any(has_nul(key) or has_nul(child) for key, child in item.items())
        if isinstance(item, list):
            return any(has_nul(child) for child in item)
        return False

    require(not has_nul(value))
    return encoded.decode("utf-8")


def _strings(value: Any, minimum: int, maximum: int) -> bool:
    return (
        isinstance(value, list)
        and minimum <= len(value) <= maximum
        and all(isinstance(item, str) and item for item in value)
        and len(set(value)) == len(value)
    )


def _identifier(value: Any) -> bool:
    return isinstance(value, str) and IDENTIFIER.fullmatch(value) is not None


def event_ids(claim: dict[str, Any]) -> list[str]:
    """Return event identities in the fixed contract order, never model pointers."""
    kind = claim["kind"]
    if kind in {"timing", "critical"}:
        return [claim["event_id"]]
    if kind == "dispute":
        return [claim["delivered_event_id"]]
    if kind == "conflict":
        return claim["event_ids"]
    if kind == "correction":
        return [claim["recovery_event_id"], claim["corrected_event_id"]]
    return []


def _claim(claim: Any, *, persisted: bool) -> None:
    require(isinstance(claim, dict) and isinstance(claim.get("kind"), str))
    variant = CLAIM_VARIANTS.get(claim["kind"])
    require(variant is not None)
    assert variant is not None
    require(set(claim) == {"kind", "refs"} | variant.keys())
    for name, allowed in variant.items():
        if allowed is not None:
            require(isinstance(claim[name], str) and claim[name] in allowed)
        elif name != "event_ids":
            require(_identifier(claim[name]))
    if claim["kind"] == "conflict":
        count = 0 if claim["type"] == "order_delivery" else 2
        require(_strings(claim["event_ids"], count, count))
    ids = event_ids(claim)
    require(all(_identifier(item) for item in ids) and len(set(ids)) == len(ids))
    refs = claim["refs"]
    if persisted:
        require(isinstance(refs, list) and 1 <= len(refs) <= 10)
        for ref in refs:
            require(
                isinstance(ref, dict) and set(ref) == {"evidence_ref", "source_pointer"}
            )
            require(all(isinstance(ref[key], str) and ref[key] for key in ref))
        require(
            len({(ref["evidence_ref"], ref["source_pointer"]) for ref in refs})
            == len(refs)
        )
    else:
        require(_strings(refs, 1, 8))
        allowed_refs = {
            prefix + "#" + pointer
            for prefix, pointers in POINTERS.items()
            for pointer in pointers
        }
        require(
            all(
                ref in allowed_refs or POLICY_ALIAS.fullmatch(ref) is not None
                for ref in refs
            )
        )


def _shape(value: Any, *, persisted: bool) -> None:
    bounded_json(value)
    require(
        isinstance(value, dict)
        and set(value)
        == MODEL_FIELDS | ({"summary", "evidence_refs"} if persisted else set())
    )
    require(all(item is not None for item in value.values()))
    require(
        (value["decision"] == "no_action" and value["action"] == "")
        or (
            value["decision"] == "proposal"
            and value["action"]
            in ("record_conclusion", "request_information", "escalate")
        )
    )
    require(
        value["conclusion"]
        in ("on_time", "delayed", "disputed", "insufficient", "conflicting")
    )
    require(
        value["target_ticket_status"]
        in ("open", "awaiting_information", "escalated", "informational_only")
    )
    require(
        _strings(value["requested_fields"], 0, 5)
        and all(item in REQUESTED_FIELDS for item in value["requested_fields"])
    )
    require(isinstance(value["claims"], list) and 1 <= len(value["claims"]) <= 4)
    seen: set[str] = set()
    for claim in value["claims"]:
        _claim(claim, persisted=persisted)
        identity = copy.deepcopy(claim)
        identity["refs"] = sorted(identity["refs"], key=lambda ref: bounded_json(ref))
        key = json.dumps(identity, sort_keys=True, separators=(",", ":"))
        require(key not in seen)
        seen.add(key)


def render_summary(value: dict[str, Any]) -> str:
    """Render exactly the Go template, faithfully labelling unsupported assertions."""
    action = "no_action" if value["decision"] == "no_action" else value["action"]
    fields = ", ".join(value["requested_fields"]) or "none"
    lines = [
        f"Recommendation: {action}. Conclusion asserted: {value['conclusion']}. Suggested ticket status: {value['target_ticket_status']}. Requested fields: {fields}."
    ]
    for claim in value["claims"]:
        kind = claim["kind"]
        if kind == "timing":
            description = f"timing {claim['test']}; event {claim['event_id']}"
        elif kind == "dispute":
            description = f"dispute {claim['type']}; delivered event {claim['delivered_event_id']}"
        elif kind == "critical":
            description = f"critical {claim['status']}; event {claim['event_id']}"
        elif kind == "missing":
            description = "missing " + claim["field"]
        elif kind == "conflict":
            description = (
                f"conflict {claim['type']}; events [{', '.join(claim['event_ids'])}]"
            )
        elif kind == "correction":
            description = f"correction; recovery event {claim['recovery_event_id']}; corrected event {claim['corrected_event_id']}"
        else:
            description = "ticket_status " + claim["mode"]
        lines.append("Claim asserted: " + description + ".")
    return "\n".join(lines)


def validate_persisted_shape(value: Any) -> None:
    """Check the closed eight-field format without selecting or authorizing a strategy."""
    _shape(value, persisted=True)
    require(
        isinstance(value["summary"], str) and value["summary"] == render_summary(value)
    )
    refs: list[str] = []
    for claim in value["claims"]:
        for source in claim["refs"]:
            if source["evidence_ref"] not in refs:
                refs.append(source["evidence_ref"])
    require(_strings(value["evidence_refs"], 1, 32) and value["evidence_refs"] == refs)


@dataclass
class SupportSources:
    """Keep only actual returned source locations and already accepted facts."""

    aliases: dict[str, dict[str, str]] = field(default_factory=dict)
    events: dict[str, dict[str, str]] = field(default_factory=dict)
    contents: dict[str, dict[str, Any]] = field(default_factory=dict)


def _pointers(
    sources: SupportSources, prefix: str, ref: str, content: dict[str, Any]
) -> None:
    for pointer in POINTERS[prefix]:
        current: Any = content
        for part in pointer[1:].split("/"):
            if not isinstance(current, dict) or part not in current:
                break
            current = current[part]
        else:
            sources.aliases[prefix + "#" + pointer] = {
                "evidence_ref": ref,
                "source_pointer": pointer,
            }


def _source_scalars(
    value: dict[str, Any], strings: tuple[str, ...], dates: tuple[str, ...] = ()
) -> None:
    # Match Go's typed source decode: absent/null scalar fields do not create a
    # new fact; present non-null values must have their original scalar type.
    for name in strings:
        require(value.get(name) is None or isinstance(value[name], str))
    for name in dates:
        raw = value.get(name)
        if raw is None:
            continue
        require(
            isinstance(raw, str)
            and re.fullmatch(
                r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})",
                raw,
            )
            is not None
        )
        valid = False
        try:
            valid = (
                datetime.fromisoformat(raw.replace("Z", "+00:00")).tzinfo is not None
            )
        except ValueError:
            pass
        require(valid)


def _read_source(snapshot: dict[str, Any], kind: str, result: dict[str, Any]) -> None:
    content = result["content"]
    object_kind = kind.removeprefix("get_")
    ref = f"business-evidence:{snapshot['snapshot_id']}:{object_kind}"
    require(
        set(content)
        <= {
            "snapshot_id",
            "evidence_ref",
            "kind",
            "missing",
            "missing_reason",
            "ticket",
            "order",
            "delivery",
        }
    )
    require(
        content.get("snapshot_id") == snapshot["snapshot_id"]
        and content.get("kind") == object_kind
        and content.get("evidence_ref") == ref
        and result["evidence_refs"] == [ref]
    )
    require(type(content.get("missing")) is bool and content.get("ticket") is None)
    vector = snapshot["version_vector_json"]
    expected = vector[object_kind]
    fact = content.get(object_kind)
    require(content.get("delivery" if object_kind == "order" else "order") is None)
    if content["missing"]:
        reason = "not_associated" if expected["id"] is None else "record_not_found"
        require(
            not expected["exists"]
            and fact is None
            and content.get("missing_reason") == reason
        )
        return
    require(
        isinstance(fact, dict)
        and expected["exists"]
        and content.get("missing_reason", "") == ""
    )
    require(
        fact.get("tenant_id") == snapshot["tenant_id"]
        and fact.get(object_kind + "_id") == expected["id"]
    )
    revision = "revision" if object_kind == "order" else "aggregate_revision"
    require(type(fact.get(revision)) is int and fact[revision] == expected[revision])
    if object_kind == "order":
        require(
            set(fact)
            <= {
                "tenant_id",
                "order_id",
                "revision",
                "delivery_id",
                "status",
                "ordered_at",
                "promised_delivery_at",
            }
        )
        require(fact.get("delivery_id") == vector["delivery"]["id"])
        _source_scalars(fact, ("status",), ("ordered_at", "promised_delivery_at"))
    else:
        require(
            set(fact)
            <= {
                "tenant_id",
                "delivery_id",
                "order_id",
                "aggregate_revision",
                "status",
                "events",
            }
        )
        require(
            fact.get("order_id") == vector["order"]["id"]
            and isinstance(fact.get("events"), list)
        )
        _source_scalars(fact, ("status",))
        seen: set[str] = set()
        for event in fact["events"]:
            require(
                isinstance(event, dict)
                and set(event) <= {"event_id", "occurred_at", "status", "note"}
            )
            require(
                _identifier(event.get("event_id")) and event["event_id"] not in seen
            )
            _source_scalars(event, ("status", "note"), ("occurred_at",))
            seen.add(event["event_id"])


def _policy_source(snapshot: dict[str, Any], result: dict[str, Any]) -> None:
    content = result["content"]
    require(
        set(content) == {"snapshot_id", "matches"}
        and content["snapshot_id"] == snapshot["snapshot_id"]
    )
    matches = content["matches"]
    require(
        isinstance(matches, list)
        and len(matches) <= 3
        and len(result["evidence_refs"]) == len(matches)
    )
    seen: set[str] = set()
    for index, hit in enumerate(matches):
        require(
            isinstance(hit, dict)
            and set(hit)
            <= {
                "index_id",
                "chunk_id",
                "policy_version",
                "evidence_ref",
                "source",
                "text",
                "distance",
            }
        )
        alias = hit.get("chunk_id")
        require(
            isinstance(alias, str)
            and POLICY_ALIAS.fullmatch(alias) is not None
            and alias not in seen
        )
        seen.add(alias)
        ref = f"business-policy:{snapshot['index_id']}:{alias}"
        require(
            hit.get("index_id") == snapshot["index_id"]
            and hit.get("policy_version")
            == snapshot["ticket_binding_json"]["policy_version"]
            and hit.get("evidence_ref") == ref
            and result["evidence_refs"][index] == ref
        )
        require(
            all(
                isinstance(hit.get(key), str) and hit[key] for key in ("text", "source")
            )
        )
        require(hit.get("distance") is None or type(hit["distance"]) in (int, float))


def support_sources(
    checkpoint: dict[str, Any], *, repeated_search: bool = False
) -> SupportSources:
    """Bind snapshot, tenant, versions, evidence envelopes and unique event IDs."""
    from jobforge_agent.runtime_input import (
        RuntimeInputError,
        _snapshot,
        validate_step_result,
    )

    failure: DispatchError | None = None
    try:
        snapshot = checkpoint["snapshot"]
        _snapshot(snapshot, snapshot)
        sources = SupportSources()
        policies: dict[str, dict[str, Any]] = {}
        _pointers(
            sources,
            "T",
            f"business-evidence:{snapshot['snapshot_id']}:ticket",
            snapshot["ticket_binding_json"],
        )
        for accepted in checkpoint["steps"]:
            kind = accepted["step"]["kind"]
            if kind not in {"get_order", "get_delivery", "search_policy"}:
                continue
            require(
                kind not in sources.contents
                or (repeated_search and kind == "search_policy")
            )
            result = accepted["result_json"]
            validate_step_result(result, kind)
            content = result["content"]
            if kind == "search_policy":
                _policy_source(snapshot, result)
                for hit in content["matches"]:
                    identity = {
                        key: val for key, val in hit.items() if key != "distance"
                    }
                    previous = policies.get(hit["chunk_id"])
                    require(previous is None or previous == identity)
                    policies[hit["chunk_id"]] = identity
                    sources.aliases[hit["chunk_id"]] = {
                        "evidence_ref": hit["evidence_ref"],
                        "source_pointer": "/text",
                    }
            else:
                _read_source(snapshot, kind, result)
                prefix = "E1" if kind == "get_order" else "E2"
                _pointers(sources, prefix, result["evidence_refs"][0], content)
                if kind == "get_delivery" and content.get("delivery") is not None:
                    for index, event in enumerate(content["delivery"]["events"]):
                        sources.events[event["event_id"]] = {
                            "evidence_ref": result["evidence_refs"][0],
                            "source_pointer": f"/delivery/events/{index}",
                        }
            if kind == "search_policy" and kind in sources.contents:
                combined = {
                    hit["chunk_id"]: hit for hit in sources.contents[kind]["matches"]
                }
                for hit in content["matches"]:
                    combined.setdefault(hit["chunk_id"], copy.deepcopy(hit))
                sources.contents[kind]["matches"] = list(combined.values())
            else:
                sources.contents[kind] = copy.deepcopy(content)
        return sources
    except RuntimeInputError as error:
        failure = DispatchError(
            "OUTPUT_INVALID",
            fact="size_limit" if error.size_limit else "",
            stop=error.size_limit,
        )
    except (KeyError, TypeError, ValueError):
        failure = DispatchError("OUTPUT_INVALID")
    if failure is not None:
        raise failure
    raise AssertionError("unreachable")


def proposal_from_model(
    value: dict[str, Any], checkpoint: dict[str, Any], *, repeated_search: bool = False
) -> dict[str, Any]:
    """Expand a six-field model proposal without repairing its policy conclusions."""
    _shape(value, persisted=False)
    sources = support_sources(checkpoint, repeated_search=repeated_search)
    proposal = copy.deepcopy(value)
    refs: list[str] = []
    for claim in proposal["claims"]:
        expanded: list[dict[str, str]] = []
        require(any(POLICY_ALIAS.fullmatch(ref) is not None for ref in claim["refs"]))
        for ref in claim["refs"]:
            require(ref in sources.aliases)
            source = sources.aliases[ref]
            if source not in expanded:
                expanded.append(copy.deepcopy(source))
        for event in event_ids(claim):
            require(event in sources.events)
            source = sources.events[event]
            if source not in expanded:
                expanded.append(copy.deepcopy(source))
        claim["refs"] = expanded
        for source in expanded:
            if source["evidence_ref"] not in refs:
                refs.append(source["evidence_ref"])
    current = checkpoint["snapshot"]["ticket_binding_json"]["status"]
    target = {
        "request_information": "awaiting_information",
        "escalate": "escalated",
    }.get(proposal["action"], current)
    require(proposal["target_ticket_status"] == target)
    proposal.update(summary=render_summary(proposal), evidence_refs=refs)
    validate_persisted_shape(proposal)
    return proposal


def parse_model_proposal(raw: bytes, checkpoint: dict[str, Any]) -> dict[str, Any]:
    """Apply the same raw JSON rules as the provider adapter and shared Go vectors."""
    if len(raw) > 16384:
        raise DispatchError("OUTPUT_INVALID", fact="size_limit", stop=True)
    value: Any = None
    try:
        value = strict_json(raw)
    except ToolError:
        pass
    require(isinstance(value, dict))
    return proposal_from_model(value, checkpoint)


def validate_persisted_proposal(
    value: dict[str, Any], checkpoint: dict[str, Any], *, repeated_search: bool = False
) -> None:
    """Reconstruct model aliases to verify stored rendering and full provenance."""
    validate_persisted_shape(value)
    sources = support_sources(checkpoint, repeated_search=repeated_search)
    reverse = {
        (source["evidence_ref"], source["source_pointer"]): alias
        for alias, source in sources.aliases.items()
    }
    model = {key: copy.deepcopy(value[key]) for key in MODEL_FIELDS}
    for claim in model["claims"]:
        event_sources = []
        for event in event_ids(claim):
            require(event in sources.events)
            event_sources.append(sources.events[event])
        aliases = []
        for source in claim["refs"]:
            alias = reverse.get((source["evidence_ref"], source["source_pointer"]))
            if alias is not None:
                aliases.append(alias)
            else:
                require(source in event_sources)
        claim["refs"] = aliases
    require(
        proposal_from_model(model, checkpoint, repeated_search=repeated_search) == value
    )
