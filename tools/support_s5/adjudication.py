"""Separate investigation diagnostics from the original final acceptance score."""

from __future__ import annotations

from collections.abc import Iterable
from typing import Any

from jobforge_agent.dispatch import DispatchError
from jobforge_agent.support_agent_v5 import DEFINITION_ALIASES
from jobforge_agent.support_contract import proposal_from_model, support_sources

from tools.support_evaluation.evidence import Package, need, validate_run_sources
from tools.support_evaluation.predicates import (
    Facts,
    claim_sources,
    claim_truth,
    coverage_key,
    derive_facts,
)
from tools.support_evaluation.validate_data import instant

# Offline measurement constants for the frozen final-adjudication envelope:
# the thinking model_proposal call reserves 16,384 completion tokens under a
# 120-second deadline. None of this decides acceptance; it only reports how
# close the accepted run was to losing its output or its required paragraphs.
FINAL_COMPLETION_CAPACITY = 16384
FINAL_DEADLINE_SECONDS = 120
TRUNCATION_RATIO = 0.9
TRUNCATION_WALL_SECONDS = 96

# Policy aliases claim_sources accepts for one required semantic class. This
# mirrors the frozen scorer so an offline report can state whether ANY legal
# final proposal could have been fully sourced from the retrieved paragraphs.
# It never decides whether a condition holds; claim_sources remains the only
# acceptance predicate, and the mirror is two-way tested against it.
CLASS_ALIASES: dict[str, frozenset[str]] = {
    "dispute": frozenset({"P04.1", "P04.2"}),
    "critical": frozenset({"P06.1", "P10.1"}),
    "conflict": frozenset({"P05.1", "P05.2", "P10.1"}),
    "correction": frozenset({"P05.2", "P06.2"}),
}
TIMING_ALIASES: dict[str, frozenset[str]] = {
    # claim_sources replaces the P02 base set for this test: only P07.1.
    "outstanding_overdue_ge48": frozenset({"P07.1"}),
    "outstanding_overdue_lt48": frozenset({"P02.1", "P02.2", "P07.1"}),
    "delivered_late": frozenset({"P02.1", "P02.2", "P07.2"}),
    "delivered_not_late": frozenset({"P02.1", "P02.2"}),
    "outstanding_not_overdue": frozenset({"P02.1", "P02.2"}),
}

# Required semantic class -> the navigation topic whose DEFINING paragraph the
# investigation had to retrieve. This asks a different question than the
# accepted-alias mirror above, and neither notion implies the other: for most
# classes the defining aliases are a subset of the accepted ones, but not for
# timing under outstanding_overdue_ge48 (definition P02.1/P02.2, accepted only
# P07.1) nor for ticket_status:informational_no_action (definition P08.1/P08.2/
# P09.1, accepted only P08.1). A topic's alias set can also contain paragraphs
# shared with a sibling topic, so retrieving one of them does not prove the class
# definition was ever read. The alias table itself is imported from the v5 prompt
# module and never duplicated here; ``extended_delay`` is navigation for the
# timing branch and no required class maps to it.
CLASS_DEFINITION_TOPICS: dict[str, str] = {
    "conflict": "structured_conflict",
    "correction": "carrier_correction",
    "critical": "critical_exception",
    "dispute": "delivery_dispute",
    "missing": "missing_facts",
    "ticket_status": "ticket_status",
    "timing": "timing",
}


def definition_topic(required: str) -> str:
    """Map one required semantic class to the topic that defines it.

    The class set is closed by the frozen ``derive_facts``, so an unmapped class
    means the contract grew and this measurement can no longer be computed; that
    is reported as a fixed code rather than as a silent pass.
    """
    kind = required.partition(":")[0]
    topic = CLASS_DEFINITION_TOPICS.get(kind)
    need(topic is not None, "DEFINITION_TOPIC_UNMAPPED")
    assert topic is not None
    need(topic in DEFINITION_ALIASES, "DEFINITION_TOPIC_UNBOUND")
    return topic


def stage_usage(calls: list[dict[str, Any]]) -> dict[str, Any]:
    """Elapsed time spans reservation through observation, not provider-only time."""
    elapsed: list[float] = []
    for call in calls:
        if call["observed_at"] is None:
            elapsed = []
            break
        elapsed.append(
            (
                instant(call["observed_at"]) - instant(call["reserved_at"])
            ).total_seconds()
        )
    return {
        "chat_calls": len(calls),
        "known_cost_microyuan": sum(c["known_cost_microyuan"] for c in calls),
        "held_cost_microyuan": sum(c["held_cost_microyuan"] for c in calls),
        "known_tokens": sum(c["known_tokens"] for c in calls),
        "held_tokens": sum(c["held_tokens"] for c in calls),
        "reservation_to_observation_seconds": sum(elapsed)
        if len(elapsed) == len(calls)
        else None,
    }


def marker_index(checkpoint: dict[str, Any]) -> int | None:
    """Locate the first committed closed investigation marker, if any."""
    for index, step in enumerate(checkpoint["steps"]):
        content = step["result_json"]["content"]
        if (
            isinstance(content, dict)
            and content.get("type") == "investigation_complete"
        ):
            return index
    return None


def accepted_policy_aliases(required: str, timing: str | None) -> frozenset[str]:
    """Return the alias set claim_sources accepts for one required class.

    A valid timing claim asserts ``claim["test"] == facts.timing``, so the
    timing entry is keyed on the derived timing value. When no timing is
    derivable, no valid timing claim exists and no retrieved alias can satisfy
    the requirement, which is reported as an empty accepted set.
    """
    if required == "timing":
        return TIMING_ALIASES.get(timing or "", frozenset())
    kind, _, variant = required.partition(":")
    if kind == "missing":
        aliases = frozenset({"P03.1", "P03.2"})
        if variant == "delivery.delivered_event":
            return aliases | {"P02.2"}
        return aliases
    if kind == "ticket_status":
        return (
            frozenset({"P08.1"})
            if variant == "informational_no_action"
            else frozenset({"P08.2", "P09.1"})
        )
    return CLASS_ALIASES.get(kind, frozenset())


def draft_score(
    row: dict[str, Any],
    binding: dict[str, Any],
    profile: dict[str, Any],
    package: Package,
    *,
    checkpoint: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """Score the raw marker once against only sources committed before it.

    ``claim_checks`` mirrors the accepted final proposal's per-claim scoring
    (``claim_truth`` AND ``claim_sources`` against the sources reconstructed
    from the steps preceding the marker) and ``claim_keys`` carries the
    matching ``coverage_key`` per claim, so the paired delta compares
    identical structures. Both are measurement plumbing for
    ``claim_level_delta`` and stay out of the published investigation block.
    """
    result: dict[str, Any] = {
        "available": False,
        "correct": False,
        "full_case_evidence": False,
        "claim_checks": [],
        "claim_keys": [],
    }
    if checkpoint is None:
        checkpoint, _ = validate_run_sources(row, binding, profile, package)
    marker = marker_index(checkpoint)
    if marker is None:
        return result
    result["available"] = True
    prior = {**checkpoint, "steps": checkpoint["steps"][:marker]}
    try:
        proposal = proposal_from_model(
            checkpoint["steps"][marker]["result_json"]["content"]["draft"],
            prior,
            repeated_search=True,
        )
    except (DispatchError, ValueError, TypeError, KeyError):
        result["error"] = "INVALID_DRAFT_OR_SOURCE"
        return result
    sources = support_sources(prior, repeated_search=True)
    facts = derive_facts(
        prior["snapshot"]["ticket_binding_json"],
        sources.contents.get("get_order", {}).get("order"),
        sources.contents.get("get_delivery", {}).get("delivery"),
        package.anchors,
    )
    reverse = {
        (s["evidence_ref"], s["source_pointer"]): alias
        for alias, s in sources.aliases.items()
    }
    coverage = set()
    checks: list[dict[str, Any]] = []
    keys: list[str] = []
    for index, claim in enumerate(proposal["claims"]):
        refs = {
            reverse.get((r["evidence_ref"], r["source_pointer"]), "")
            for r in claim["refs"]
        }
        truth = claim_truth(claim, facts)
        support = claim_sources(claim, facts, refs)
        checks.append(
            {
                "index": index,
                "kind": claim["kind"],
                "predicate": truth,
                "source_coverage": support,
            }
        )
        key = coverage_key(claim)
        keys.append(key)
        if truth and support:
            coverage.add(key)
    result["claim_checks"] = checks
    result["claim_keys"] = keys
    result["full_case_evidence"] = (
        bool(checks)
        and all(c["predicate"] and c["source_coverage"] for c in checks)
        and facts.required <= coverage
    )
    result["correct"] = result["full_case_evidence"] and all(
        (
            sorted(proposal[k]) == sorted(v)
            if k == "requested_fields"
            else proposal[k] == v
        )
        for k, v in facts.expected.items()
    )
    return result


def _marker_context(
    checkpoint: dict[str, Any], package: Package
) -> tuple[Facts, set[str]] | None:
    """Recompute the pre-marker facts and the paragraph aliases retrieved.

    Both closure notions answer different questions over exactly this context, so
    they can never disagree about what the investigation had actually read.
    """
    marker = marker_index(checkpoint)
    if marker is None:
        return None
    prior = {**checkpoint, "steps": checkpoint["steps"][:marker]}
    sources = support_sources(prior, repeated_search=True)
    facts = derive_facts(
        prior["snapshot"]["ticket_binding_json"],
        sources.contents.get("get_order", {}).get("order"),
        sources.contents.get("get_delivery", {}).get("delivery"),
        package.anchors,
    )
    available: set[str] = set()
    for step in prior["steps"]:
        if step["step"]["kind"] == "search_policy":
            for match in step["result_json"]["content"]["matches"]:
                available.add(match["chunk_id"])
    return facts, available


def retrieval_closure(
    checkpoint: dict[str, Any], package: Package
) -> dict[str, Any] | None:
    """Separate unreachable required evidence from wrong final reasoning.

    The final stage has no tools, so it can only cite paragraphs that committed
    search_policy steps retrieved before the marker. When a required class has
    no retrieved alias inside the set claim_sources accepts, no legal final
    proposal can reach full case evidence regardless of reasoning quality.

    This is the accepted-alias notion only. It is a necessary condition, not a
    guarantee: claim_sources also requires fact-pointer groups that this view
    does not model, and an alias shared with a sibling topic satisfies a class
    whose defining paragraph was never read. ``definition_closure`` answers that
    second question and the two are never merged.
    """
    context = _marker_context(checkpoint, package)
    if context is None:
        return None
    facts, available = context
    required = []
    for entry in sorted(facts.required):
        accepted = accepted_policy_aliases(entry, facts.timing)
        present = sorted(accepted & available)
        required.append(
            {
                "required": entry,
                "accepted_aliases": sorted(accepted),
                "present_aliases": present,
                "satisfied": bool(present),
            }
        )
    unsatisfied = [item["required"] for item in required if not item["satisfied"]]
    return {
        "available": sorted(available),
        "required": required,
        "adjudication_recoverable": not unsatisfied,
        "unsatisfied": unsatisfied,
    }


def definition_entries(
    required: Iterable[str], available: set[str]
) -> list[dict[str, Any]]:
    """Report, per required class, whether its DEFINING paragraph was retrieved.

    ``definition_present`` lists the defining aliases of the class's topic that
    are actually in ``available``; ``definition_satisfied`` is whether that list
    is non-empty. A sibling alias retrieved by an unrelated search leaves the
    class unsatisfied here even though ``retrieval_closure`` reports it satisfied.
    """
    entries = []
    for entry in sorted(required):
        topic = definition_topic(entry)
        aliases = set(DEFINITION_ALIASES[topic])
        present = sorted(aliases & available)
        entries.append(
            {
                "required": entry,
                "topic": topic,
                "definition_aliases": sorted(aliases),
                "definition_present": present,
                "definition_satisfied": bool(present),
            }
        )
    return entries


def definition_closure(
    checkpoint: dict[str, Any], package: Package
) -> dict[str, Any] | None:
    """Measure whether the investigation read the rule each required class needs.

    Distinct from and never conflated with ``retrieval_closure``: that one asks
    whether any legal terminal proposal could have been fully sourced, this one
    asks whether the paragraph that DEFINES each required class was retrieved
    before the marker. The S5V3-004 shape separates them -- P05.2 arrived via a
    correction search, so the accepted-alias view reported the conflict class
    reachable while P05.1, the only paragraph defining the five conflict types,
    was never read. The separation runs both ways (see CLASS_DEFINITION_TOPICS),
    so both views are emitted side by side and never merged into one flag.
    Marker-anchored like the accepted-alias view, so a Fixed row without a closed
    marker yields None rather than a passing measurement.
    """
    context = _marker_context(checkpoint, package)
    if context is None:
        return None
    facts, available = context
    required = definition_entries(facts.required, available)
    unsatisfied = [
        item["required"] for item in required if not item["definition_satisfied"]
    ]
    return {
        "required": required,
        "definition_unsatisfied": unsatisfied,
        "investigation_definition_complete": not unsatisfied,
    }


def _claim_validity(
    claims: list[dict[str, Any]], keys: list[str]
) -> tuple[set[str], set[str]]:
    """Group per-claim results by semantic class as the coverage sets do."""
    present: set[str] = set()
    valid: set[str] = set()
    for claim, key in zip(claims, keys, strict=True):
        present.add(key)
        if claim["predicate"] and claim["source_coverage"]:
            valid.add(key)
    return present, valid


def claim_level_delta(
    draft: dict[str, Any],
    draft_claims: list[dict[str, Any]],
    draft_keys: list[str],
    final_claims: list[dict[str, Any]],
    final_keys: list[str],
    final_full_evidence: bool,
) -> dict[str, Any]:
    """Compare the paired stages per claim class, not only by case-level flags.

    A class is valid when any claim carrying its coverage key passes both the
    truth predicate and source coverage, matching the coverage semantics both
    scorers already use. ``evidence_regressed`` keeps a lost full-evidence
    draft visible even when the case-level correct flags never moved, which is
    exactly the regression a correct-only paired comparison reports as
    unchanged.
    """
    draft_present, draft_valid = _claim_validity(draft_claims, draft_keys)
    final_present, final_valid = _claim_validity(final_claims, final_keys)
    draft_full = bool(draft["full_case_evidence"])
    return {
        "draft_claims": draft_claims,
        "final_claims": final_claims,
        "added": sorted(
            key for key in final_present - draft_present if key in final_valid
        ),
        "removed": sorted(
            key for key in draft_present - final_present if key in draft_valid
        ),
        "flipped": [
            {"kind": key, "from": key in draft_valid, "to": key in final_valid}
            for key in sorted(draft_present & final_present)
            if (key in draft_valid) != (key in final_valid)
        ],
        "evidence_regressed": draft_full and not final_full_evidence,
        "evidence_improved": final_full_evidence and not draft_full,
    }


def reasoning_budget(
    calls: list[dict[str, Any]], usage: dict[str, Any] | None
) -> dict[str, Any] | None:
    """Measure the final stage against its frozen completion envelope.

    Reasoning tokens come from the provider audit already carried on each
    final call record; wall time is the existing reservation-to-observation
    sum. ``truncation_risk`` is a measurement flag, never a scored failure,
    and no budget exists without an observed final call.
    """
    if not calls or usage is None:
        return None
    reasoning_tokens = sum(
        (call["provider_audit"] or {}).get("reasoning_tokens") or 0 for call in calls
    )
    wall = usage["reservation_to_observation_seconds"]
    ratio = reasoning_tokens / FINAL_COMPLETION_CAPACITY
    return {
        "reasoning_tokens": reasoning_tokens,
        "completion_capacity": FINAL_COMPLETION_CAPACITY,
        "ratio": ratio,
        "wall_seconds": wall,
        "deadline_seconds": FINAL_DEADLINE_SECONDS,
        "deadline_headroom_seconds": (
            FINAL_DEADLINE_SECONDS - wall if wall is not None else None
        ),
        "truncation_risk": ratio > TRUNCATION_RATIO
        or (wall is not None and wall > TRUNCATION_WALL_SECONDS),
    }


def measurement_summary(results: list[dict[str, Any]]) -> dict[str, Any]:
    """Aggregate the paired delta, both closure notions and budget flags.

    Cases without a measured group are skipped and never counted as passing, so
    each rate keeps an explicit denominator: ``retrieval_closure_measured`` for
    the accepted-alias notion and ``definition_closure_measured`` for the
    defining-paragraph notion. The two counts are reported separately because
    they answer different questions and can legitimately differ per case.
    """
    regressed: list[str] = []
    improved: list[str] = []
    unrecoverable: list[str] = []
    truncated: list[str] = []
    incomplete: list[str] = []
    measured = 0
    definition_measured = 0
    for result in results:
        detail = result.get("adjudication_stages")
        if detail is None:
            continue
        delta = detail["claim_level_delta"]
        if delta is not None:
            if delta["evidence_regressed"]:
                regressed.append(result["case_id"])
            if delta["evidence_improved"]:
                improved.append(result["case_id"])
        closure = detail["retrieval_closure"]
        if closure is not None:
            measured += 1
            if not closure["adjudication_recoverable"]:
                unrecoverable.append(result["case_id"])
        definition = detail["definition_closure"]
        if definition is not None:
            definition_measured += 1
            if not definition["investigation_definition_complete"]:
                incomplete.append(result["case_id"])
        budget = detail["reasoning_budget"]
        if budget is not None and budget["truncation_risk"]:
            truncated.append(result["case_id"])
    return {
        "evidence_regressed": len(regressed),
        "evidence_regressed_cases": regressed,
        "evidence_improved": len(improved),
        "evidence_improved_cases": improved,
        "retrieval_closure_measured": measured,
        "adjudication_recoverable_false": len(unrecoverable),
        "adjudication_recoverable_false_cases": unrecoverable,
        "definition_closure_measured": definition_measured,
        "investigation_definition_incomplete": len(incomplete),
        "investigation_definition_incomplete_cases": incomplete,
        "truncation_risk": len(truncated),
        "truncation_risk_cases": truncated,
    }


def stages(
    row: dict[str, Any],
    result: dict[str, Any],
    binding: dict[str, Any],
    profile: dict[str, Any],
    package: Package,
) -> dict[str, Any]:
    """Never replace final acceptance fields with the better of two outputs."""
    verified = result["usage_status"] in {"verified", "not_attempted"}
    calls = row["calls"]["items"] if result["usage_status"] == "verified" else []
    final = [
        c
        for c in calls
        if c["subcall"] == "chat" and c["step_kind"] == "model_proposal"
    ]
    investigation = [
        c
        for c in calls
        if c["subcall"] == "chat" and c["step_kind"] != "model_proposal"
    ]
    draft: dict[str, Any] = {
        "available": False,
        "correct": False,
        "full_case_evidence": False,
    }
    checkpoint: dict[str, Any] | None = None
    proposal: dict[str, Any] = {}
    if (
        profile["strategy"] == "support_agent_v1"
        and result["source"] == result["protocol"] == "passed"
    ):
        checkpoint, proposal = validate_run_sources(row, binding, profile, package)
        draft = draft_score(row, binding, profile, package, checkpoint=checkpoint)
    draft_claims = draft.pop("claim_checks", [])
    draft_keys = draft.pop("claim_keys", [])
    final_usage = stage_usage(final) if verified else None
    final_keys = [coverage_key(claim) for claim in proposal.get("claims", [])]
    # _claim_validity zips the accepted checks with their coverage keys strictly;
    # a length mismatch means the paired structures are not comparable, which is
    # reported as an absent delta rather than raising inside scoring.
    comparable = draft["available"] and len(final_keys) == len(result["claim_checks"])
    return {
        "investigation": {
            **draft,
            "usage": stage_usage(investigation) if verified else None,
        },
        "final": {
            "correct": result["correct"],
            "full_case_evidence": result["full_case_evidence"],
            "usage": final_usage,
            "no_reasoning_signal_calls": sum(
                c["provider_audit"] is not None
                and c["provider_audit"]["mode_state"] == "nonthinking"
                for c in final
            )
            if verified
            else None,
        },
        # Offline measurement only. The paired delta exists solely when an
        # actual draft was scored; the accepted final claim_checks are threaded
        # through from the case result and never recomputed a second way, and
        # their coverage keys come from the same accepted proposal.
        "claim_level_delta": claim_level_delta(
            draft,
            draft_claims,
            draft_keys,
            result["claim_checks"],
            final_keys,
            bool(result["full_case_evidence"]),
        )
        if comparable
        else None,
        "retrieval_closure": (
            retrieval_closure(checkpoint, package) if checkpoint is not None else None
        ),
        # Second, strictly stronger closure notion, emitted side by side with the
        # accepted-alias one and never merged into it.
        "definition_closure": (
            definition_closure(checkpoint, package) if checkpoint is not None else None
        ),
        "reasoning_budget": reasoning_budget(final, final_usage),
    }
