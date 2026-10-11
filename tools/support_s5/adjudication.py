"""Separate investigation diagnostics from the original final acceptance score."""

from __future__ import annotations

from typing import Any

from jobforge_agent.dispatch import DispatchError
from jobforge_agent.support_contract import proposal_from_model, support_sources

from tools.support_evaluation.evidence import Package, validate_run_sources
from tools.support_evaluation.predicates import (
    claim_sources,
    claim_truth,
    coverage_key,
    derive_facts,
)
from tools.support_evaluation.validate_data import instant


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


def draft_score(
    row: dict[str, Any],
    binding: dict[str, Any],
    profile: dict[str, Any],
    package: Package,
) -> dict[str, Any]:
    """Score the raw marker once against only sources committed before it."""
    result: dict[str, Any] = {
        "available": False,
        "correct": False,
        "full_case_evidence": False,
    }
    checkpoint, _ = validate_run_sources(row, binding, profile, package)
    for index, step in enumerate(checkpoint["steps"]):
        content = step["result_json"]["content"]
        if (
            not isinstance(content, dict)
            or content.get("type") != "investigation_complete"
        ):
            continue
        result["available"] = True
        prior = {**checkpoint, "steps": checkpoint["steps"][:index]}
        try:
            proposal = proposal_from_model(
                content["draft"], prior, repeated_search=True
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
        checks = []
        for claim in proposal["claims"]:
            refs = {
                reverse.get((r["evidence_ref"], r["source_pointer"]), "")
                for r in claim["refs"]
            }
            valid = claim_truth(claim, facts) and claim_sources(claim, facts, refs)
            checks.append(valid)
            if valid:
                coverage.add(coverage_key(claim))
        result["full_case_evidence"] = (
            bool(checks) and all(checks) and facts.required <= coverage
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
    return result


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
    draft = {"available": False, "correct": False, "full_case_evidence": False}
    if (
        profile["strategy"] == "support_agent_v1"
        and result["source"] == result["protocol"] == "passed"
    ):
        draft = draft_score(row, binding, profile, package)
    return {
        "investigation": {
            **draft,
            "usage": stage_usage(investigation) if verified else None,
        },
        "final": {
            "correct": result["correct"],
            "full_case_evidence": result["full_case_evidence"],
            "usage": stage_usage(final) if verified else None,
            "no_reasoning_signal_calls": sum(
                c["provider_audit"] is not None
                and c["provider_audit"]["mode_state"] == "nonthinking"
                for c in final
            )
            if verified
            else None,
        },
    }
