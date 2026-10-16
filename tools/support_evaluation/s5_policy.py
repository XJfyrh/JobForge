"""Shared S5 quality gates and authorized maximum exposure (microyuan)."""

from typing import Any

POLICY: dict[str, Any] = {
    "case_count": 40,
    "minimum_correct": 36,
    "minimum_full_evidence": 38,
    "batch_cap_microyuan": 4_000_000,
    "stage_cap_microyuan": 28_000_000,
    "full_hold_microyuan": {"investigation": 2_105_344, "final": 2_228_224},
    "systemic_protocol": "two_consecutive_or_two_in_first_three",
}
FORMAL_POLICY = dict(
    POLICY, case_count=20, minimum_correct=16, minimum_full_evidence=18
)
THRESHOLDS = {
    "proposal_correct": FORMAL_POLICY["minimum_correct"],
    "full_case_evidence": FORMAL_POLICY["minimum_full_evidence"],
    "denominator": FORMAL_POLICY["case_count"],
}
