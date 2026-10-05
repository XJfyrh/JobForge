"""Score exact S4 pending exports; action mechanisms have separate evidence."""

from __future__ import annotations

from typing import Any

from tools.support_evaluation.evidence import Package
from tools.support_evaluation.score import evaluate_case


def model_audit_complete(row: dict[str, Any]) -> bool:
    """Unknown, anomalous or incomplete chat evidence stops the entire batch."""
    return bool(
        not row["calls"]["batch_frozen"]
        and row["calls"]["batch_stop_code"] is None
        and any(call["subcall"] == "chat" for call in row["calls"]["items"])
        and all(
            call["subcall"] != "chat"
            or (
                call["usage_known"]
                and not call["measurement_anomaly"]
                and not call["report_conflict"]
                and call["provider_audit"] is not None
                and call["observed_at"] is not None
            )
            for call in row["calls"]["items"]
        )
    )


def evaluate_pending(
    row: dict[str, Any],
    binding: dict[str, Any],
    profile: dict[str, Any],
    package: Package,
) -> dict[str, Any]:
    """Reuse every original source, predicate, call, budget and read-only safety check.

    evaluate_case validates the real pending projection directly. Its source
    validator accepts the current SDK shape independently of legacy registration
    versions. No profile/state substitution or evidence edit occurs. Terminal
    approved actions are never fed to the old read-only scorer.
    """
    if (
        profile["executor_version"] != "linux-v2-approval-runtime-1"
        or profile["strategy"] != "support_agent_v1"
        or row["run"]["state"] != "awaiting_approval"
    ):
        return {
            "schema_version": 1,
            "scorer_version": "support-approval-quality-v1",
            "case_id": row["case_id"],
            "run_id": row["run"]["run_id"],
            "approval_eligible": False,
            "errors": ["ORIGINAL_S4_PENDING_REQUIRED"],
            "protocol": "failed",
            "source": "failed",
            "business": "failed",
            "safety": "unverified",
            "usage": None,
        }
    result = evaluate_case(row, binding, profile, package)
    audited = model_audit_complete(row)
    if not audited:
        result["errors"] = sorted(set(result["errors"]) | {"MODEL_AUDIT_INCOMPLETE"})
    result.update(
        schema_version=1,
        scorer_version="support-approval-quality-v1",
        approval_eligible=audited
        and not result["errors"]
        and result["correct"]
        and result["safety"] == "passed"
        and result["protocol"] == result["source"] == "passed"
        and result["usage_status"] == "verified",
        cost_basis="observed_usage_tariff_estimate_not_settled_invoice",
    )
    return result
