"""Host-only v6 development stop policy; never send scoring data to a Worker."""

from __future__ import annotations

from typing import Any

from tools.support_evaluation.s5_policy import POLICY


def stop_reasons(
    score: dict[str, Any],
    completed: int,
    sample: dict[str, Any],
    prior_cost: int,
    protocol_signatures: list[Any],
) -> list[str]:
    """Apply ADR-0036 final-input closure and the existing development gates."""
    reasons = []
    if any(
        sample[key]
        for key in ("unknown_chat_calls", "inflight", "held", "unfinished_attempts")
    ):
        reasons.append("CURRENT_LEDGER_NOT_CLOSED")
    exposure = sample["known"] + sample["held"]
    if (
        exposure > POLICY["batch_cap_microyuan"]
        or prior_cost + exposure > POLICY["stage_cap_microyuan"]
    ):
        reasons.append("COST_CAP")
    if any(score["hard_failure_cases"].values()):
        reasons.append("HARD_SAFETY_FAILURE")
    if (
        score["denominator"] != POLICY["case_count"]
        or len(score["cases"]) != POLICY["case_count"]
        or not 1 <= completed <= POLICY["case_count"]
    ):
        return sorted(set(reasons + ["DENOMINATOR"]))
    if score["schema_version"] != 3:
        reasons.append("SCORE_VERSION")
    remaining = POLICY["case_count"] - completed
    if score["correct_count"] + remaining < POLICY["minimum_correct"]:
        reasons.append("CORRECT_THRESHOLD_UNREACHABLE")
    if score["full_case_evidence_count"] + remaining < POLICY["minimum_full_evidence"]:
        reasons.append("EVIDENCE_REGRESSION_UNAVOIDABLE")
    for case in score["cases"][:completed]:
        if (
            case["source"] not in {"passed", "unavailable"}
            or case["safety"] != "passed"
            or case["usage_status"] != "verified"
            or not case["call_audit_complete"]
            or not case["execution_complete"]
        ):
            reasons.append("UNVERIFIED_CASE_BINDING")
        closure = (case.get("adjudication_stages") or {}).get("final_input_closure")
        if closure is None:
            # A failed attempt is still a quality failure, not closure success.
            if case["run_state"] in {"awaiting_approval", "succeeded"}:
                reasons.append("FINAL_CLOSURE_UNMEASURED")
            continue
        retrieval = closure.get("retrieval_closure")
        definition = closure.get("definition_closure")
        if retrieval is None or definition is None:
            reasons.append("FINAL_CLOSURE_UNMEASURED")
            continue
        if retrieval.get("adjudication_recoverable") is not True:
            reasons.append("FINAL_RETRIEVAL_CLOSURE_GAP")
        if definition.get("definition_complete") is not True:
            reasons.append("FINAL_DEFINITION_CLOSURE_GAP")
    signatures = protocol_signatures
    if (
        len(signatures) >= 2
        and signatures[-1] is not None
        and signatures[-1] == signatures[-2]
    ) or any(
        sig is not None and signatures[:3].count(sig) >= 2 for sig in signatures[:3]
    ):
        reasons.append("SYSTEMIC_PROTOCOL_FAILURE")
    return sorted(set(reasons))
