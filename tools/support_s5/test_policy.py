"""The freeze, scorer, host and preparation use the same S5 policy source."""

from pathlib import Path

from tools.support_evaluation.s5_policy import FORMAL_POLICY, POLICY, THRESHOLDS
from tools.support_recovery import plan
from tools.support_s5 import freeze, guard, score
from tools.support_s5.host import config


def test_shared_quality_and_exposure_contract() -> None:
    """Numeric policy changes cannot leave independent Python consumers behind."""
    assert config.POLICY is guard.POLICY is plan.POLICY is freeze.POLICY is POLICY
    assert score.THRESHOLDS is freeze.THRESHOLDS is THRESHOLDS
    assert config.FORMAL_POLICY is FORMAL_POLICY
    assert THRESHOLDS == {
        "proposal_correct": FORMAL_POLICY["minimum_correct"],
        "full_case_evidence": FORMAL_POLICY["minimum_full_evidence"],
        "denominator": FORMAL_POLICY["case_count"],
    }
    assert "tools/support_evaluation/s5_policy.py" in freeze.source_hashes(18)


def test_current_acceptance_and_host_documentation_match_policy() -> None:
    """Check normative current gates without relabelling historical outcomes."""
    root = Path(__file__).resolve().parents[2]
    acceptance = (root / "docs/agent-v3/s5-acceptance.md").read_text(encoding="utf-8")
    assert f"正确至少{POLICY['minimum_correct']}/{POLICY['case_count']}" in acceptance
    assert (
        f"完整案例证据至少{POLICY['minimum_full_evidence']}/{POLICY['case_count']}"
        in acceptance
    )
    formal = next(
        line for line in acceptance.splitlines() if line.startswith("| S5-11 |")
    )
    assert (
        f"Agent≥{FORMAL_POLICY['minimum_correct']}/{FORMAL_POLICY['case_count']}"
        in formal
    )
    assert (
        f"证据≥{100 * FORMAL_POLICY['minimum_full_evidence'] / FORMAL_POLICY['case_count']:g}%"
        in formal
    )
    host = (root / "tools/support_s5/host/README.md").read_text(encoding="utf-8")
    assert (
        f"每批 ¥{POLICY['batch_cap_microyuan'] // 1_000_000}/累计 ¥{POLICY['stage_cap_microyuan'] // 1_000_000}"
        in host
    )
