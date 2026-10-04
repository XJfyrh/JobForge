"""S3 small-sample accounting keeps failed and original control exposure."""

from __future__ import annotations

from typing import Any

from tools.support_evaluation.score import ZERO_USAGE
from tools.support_recovery.report import pair, sum_usage


def test_restart_comparison_includes_old_H0_and_never_zeros_unknown() -> None:
    """A low-cost H1 alone cannot create artificial savings or release a hold."""
    rows: list[dict[str, Any]] = []
    for arm, cost in (("C", 800), ("H0", 500), ("H1", 1000)):
        usage = dict(ZERO_USAGE, known_cost_microyuan=cost)
        rows.append(
            {
                "case_id": "DEV-002",
                "arm": arm,
                "ledger_usage": usage,
                "fault": {"phase": "commit_ack_lost"},
                "prefix_actions": [],
                "version_vector": {},
                "status": "finished",
                "score": {"correct": True, "safety": "passed"},
            }
        )
    comparison = pair(rows)
    assert comparison["restart_total_including_H0"]["known_cost_microyuan"] == 1500
    assert comparison["checkpoint_minus_restart_known_microyuan"] == -700
    assert comparison["exposure_settled"] and comparison["comparable"]
    rows[1]["ledger_usage"]["held_cost_microyuan"] = 2_105_344
    rows[1]["ledger_usage"]["unknown_chat_calls"] = 1
    assert not pair(rows)["exposure_settled"]
    totals = sum_usage(rows)
    assert totals is not None and totals["held_cost_microyuan"] == 2_105_344
    rows[1]["ledger_usage"] = None
    assert sum_usage(rows) is None and pair(rows)["restart_total_including_H0"] is None
    assert pair(rows)["checkpoint_minus_restart_known_microyuan"] is None


def test_changed_actual_path_or_missing_boundary_stays_in_report() -> None:
    """Different model choices cannot be stitched into a favorable pair."""
    rows: list[dict[str, Any]] = [
        {
            "case_id": "DEV-035",
            "arm": arm,
            "ledger_usage": dict(ZERO_USAGE),
            "fault": {"phase": "commit_ack_lost"},
            "prefix_actions": [],
            "version_vector": {},
            "status": "finished",
            "score": {"correct": True, "safety": "passed"},
        }
        for arm in ("C", "H0", "H1")
    ]
    rows[1]["prefix_actions"] = [{"kind": "different"}]
    assert not pair(rows)["comparable"]
    rows[1]["prefix_actions"] = []
    rows[0]["fault"] = None
    assert not pair(rows)["comparable"] and len(rows) == 3
