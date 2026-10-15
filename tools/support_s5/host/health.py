"""New-batch stop policy: quality failures are distinct from unsafe evidence."""

from __future__ import annotations

from typing import Any

from jobforge_agent.business import POLICY_BUNDLE_ALIASES


def quality_gate_applies(registration: dict[str, Any]) -> bool:
    """Derive Agent-only quality gates from the bound prepared registration."""
    blueprint = registration["strategy_blueprint"]
    program = blueprint["program"]
    profile = registration["profile"]
    identity = (
        blueprint["schema_version"],
        program["strategy"],
        program["adapter"],
        program["prompt_version"],
        profile["executor_version"],
    )
    if profile["strategy"] != program["strategy"]:
        raise ValueError("QUALITY_STRATEGY_BINDING")
    if identity == (
        17,
        "support_fixed_v1",
        "support-fixed-v1",
        "support-fixed-prompt-v2",
        "linux-v2-fixed-adjudication-runtime-2",
    ):
        return False
    if identity in {
        (
            16,
            "support_agent_v1",
            "support-agent-v1",
            "support-agent-prompt-v8",
            "linux-v2-adjudication-runtime-5",
        ),
        (
            18,
            "support_agent_v1",
            "support-agent-v1",
            "support-agent-prompt-v9",
            "linux-v2-adjudication-runtime-5",
        ),
    }:
        return True
    raise ValueError("QUALITY_STRATEGY_BINDING")


def money_reasons(
    sample: dict[str, Any], prior: int, policy: dict[str, Any], *, next_run: bool
) -> list[str]:
    """Never substitute observed averages for the next full call reservation."""
    reasons = []
    if sample["unknown_chat_calls"]:
        reasons.append("UNKNOWN_USAGE")
    exposure = sample["known"] + sample["held"]
    required = max(policy["full_hold_microyuan"].values()) if next_run else 0
    if (
        exposure + required > policy["batch_cap_microyuan"]
        or prior + exposure + required > policy["stage_cap_microyuan"]
    ):
        reasons.append("INSUFFICIENT_FULL_RESERVATION")
    return reasons


def protocol_signature(row: dict[str, Any]) -> Any:
    """Classify only observed protocol rejection, without guessing its cause."""
    if (
        row["status"] != "run"
        or (row["run"].get("error") or {}).get("code") != "MODEL_PROTOCOL_ERROR"
    ):
        return None
    rejected = [
        c
        for c in row["calls"]["items"]
        if c["subcall"] == "chat" and c["business_outcome"] == "rejected"
    ]
    return tuple((c["step_kind"], c["error_code"]) for c in rejected) or (
        "unclassified_protocol_error",
    )


def stop_reasons(
    score: dict[str, Any],
    evidence: dict[str, Any],
    completed: int,
    sample: dict[str, Any],
    prior: int,
    policy: dict[str, Any],
    registration: dict[str, Any],
) -> list[str]:
    """Evaluate the predeclared denominator; never retry, replace or relabel rows."""
    count = policy["case_count"]
    reasons = money_reasons(sample, prior, policy, next_run=completed < count)
    if any(sample[k] for k in ("held", "inflight", "unfinished_attempts")):
        reasons.append("CURRENT_LEDGER_NOT_CLOSED")
    if (
        score["schema_version"] != 3
        or score["denominator"] != count
        or len(score["cases"]) != count
        or not 1 <= completed <= count
    ):
        return sorted(set(reasons + ["SCORE_BINDING"]))
    if any(score["hard_failure_cases"].values()):
        reasons.append("HARD_SAFETY_FAILURE")
    if quality_gate_applies(registration):
        remaining = count - completed
        if score["correct_count"] + remaining < policy["minimum_correct"]:
            reasons.append("CORRECT_THRESHOLD_UNREACHABLE")
        if (
            score["full_case_evidence_count"] + remaining
            < policy["minimum_full_evidence"]
        ):
            reasons.append("EVIDENCE_THRESHOLD_UNREACHABLE")
    for result, row in zip(
        score["cases"][:completed], evidence["cases"][:completed], strict=True
    ):
        if (
            result["source"] != "passed"
            or result["safety"] != "passed"
            or result["usage_status"] != "verified"
            or not result["call_audit_complete"]
            or not result["execution_complete"]
        ):
            reasons.append("UNVERIFIED_CASE_BINDING")
        # Actual bundle contents are a mechanism invariant. Citation coverage,
        # definition closure and wrong answers remain scorer quality failures.
        bundles = [
            s["record"]["output"]["content"]
            for s in row["steps"]
            if s["record"]["kind"] == "read_policy_bundle"
        ]
        if len(bundles) != 1 or [
            m["chunk_id"] for m in bundles[0].get("matches", [])
        ] != list(POLICY_BUNDLE_ALIASES):
            reasons.append("POLICY_BUNDLE_INCOMPLETE")
    signatures = [protocol_signature(row) for row in evidence["cases"][:completed]]
    if policy["systemic_protocol"] == "two_consecutive_or_two_in_first_three" and (
        (
            len(signatures) >= 2
            and signatures[-1] is not None
            and signatures[-1] == signatures[-2]
        )
        or any(s is not None and signatures[:3].count(s) >= 2 for s in signatures[:3])
    ):
        reasons.append("SYSTEMIC_PROTOCOL_FAILURE")
    return sorted(set(reasons))
