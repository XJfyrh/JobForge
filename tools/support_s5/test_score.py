"""Synthetic regressions for denominator, evidence, audit and cost semantics."""

from __future__ import annotations

import copy
import json
from pathlib import Path
from typing import Any

import pytest

from tools.support_evaluation.evidence import EvidenceError, load_package
from tools.support_evaluation.fixtures import case_row, registered, unattempted
from tools.support_evaluation.validate_data import sha256
from tools.support_s5.package import load_package as unseen_package
from tools.support_s5.score import (
    full_case_evidence,
    paired,
    receipt_audit,
    score_export,
)


def audit(rows: list[dict] | None = None) -> dict:
    """Construct clearly synthetic original receipt-reader audit bytes."""
    raw = json.dumps(
        {
            "schema_version": 1,
            "observed_at": "2026-10-07T12:00:00Z",
            "database": "synthetic",
            "role": "jobforge_business_receipt_reader",
            "resolutions": rows or [],
        }
    )
    return {
        "before_raw": raw,
        "after_raw": raw,
        "before_sha256": sha256(raw.encode()),
        "after_sha256": sha256(raw.encode()),
    }


def test_damaged_charged_case_keeps_denominator_and_unknown_total() -> None:
    """A row with real-looking known/held exposure cannot disappear as zero fee."""
    package = load_package()
    reg = registered(package)
    reg["scorer_version"] = "support-s5-quality-v1"
    reg["strategy_blueprint"] = {}
    reg["profile"]["executor_version"] = "linux-v2-fixed-comparison-runtime-1"
    reg["comparison"] = {
        "model": {"message_content_bytes": 65536, "request_body_bytes": 131072},
        "resources": {},
        "family_limits": {},
    }
    digest = sha256(json.dumps(reg).encode())
    good, damaged = case_row(package, reg, 0), case_row(package, reg, 1)
    # Current S5 wire includes disposition; historical fixture remains intact.
    good["result"]["disposition"] = "proposal"
    damaged["result"]["disposition"] = "proposal"
    assert damaged["run"]["budget"]["family"]["known_cost_microyuan"] > 0
    damaged["steps"][0]["record"]["output_ref"] = "invalid"
    rows = [good, damaged] + [unattempted(c) for c in list(package.cases)[2:]]
    result = score_export(
        reg,
        {
            "schema_version": 1,
            "registration_sha256": digest,
            "cases": rows,
            "receipt_audit": audit(),
        },
        digest,
        package=package,
    )
    assert result["denominator"] == len(result["cases"]) == 40
    assert result["cases"][0]["usage_status"] == "verified"
    assert result["cases"][1]["usage_status"] == "unavailable"
    assert result["verified_usage_subtotal"]["known_cost_microyuan"] > 0
    assert result["usage_total"] is None and not result["usage_complete"]
    assert not result["thresholds_met"]
    agent = copy.deepcopy(result)
    agent["strategy"] = "support_agent_v1"
    comparison = paired(agent, result)
    assert comparison["denominator"] == 40
    assert comparison["outcomes"] == {"tie": 40}
    assert comparison["costs"]["agent"]["usage_total"] is None


def test_full_case_evidence_checks_truth_coverage_and_required_claims() -> None:
    """Reference existence passes only one part of complete case evidence."""
    result: dict[str, Any] = {
        "source": "passed",
        "protocol": "passed",
        "claim_checks": [{"predicate": True, "source_coverage": True}],
        "errors": [],
    }
    assert full_case_evidence(result)
    result["claim_checks"][0]["predicate"] = False
    assert not full_case_evidence(result)
    result["claim_checks"][0]["predicate"] = True
    result["errors"] = ["REQUIRED_CLAIM_MISSING"]
    assert not full_case_evidence(result)
    result["errors"] = []
    result["claim_checks"] = []
    assert not full_case_evidence(result)


def test_receipt_audit_detects_duplicates_and_missing_original_bytes() -> None:
    """Immutable business-request identities require original DB observations."""
    value = audit([{"tenant_id": "north", "business_request_id": "same"}] * 2)
    assert receipt_audit(value) == (0, 1)
    value["after_sha256"] = "a" * 64
    with pytest.raises(EvidenceError, match="RECEIPT_AUDIT_HASH"):
        receipt_audit(value)


def test_unseen_package_cannot_open_before_candidate_freeze(tmp_path: Path) -> None:
    """No formal package is authored by this regression."""
    freeze = tmp_path / "freeze.json"
    freeze.write_text(json.dumps({"status": "candidate_not_frozen"}))
    with pytest.raises(EvidenceError, match="CANDIDATE_NOT_FROZEN"):
        unseen_package(tmp_path / "uncreated-unseen", freeze)
