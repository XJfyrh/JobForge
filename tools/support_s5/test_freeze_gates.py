"""Known-case eligibility metadata only; no unseen business cases or model calls."""

from __future__ import annotations

import copy
import json
from pathlib import Path
from typing import Any

import pytest

from tools.support_evaluation.assemble import read, sha
from tools.support_evaluation.evidence import EvidenceError
from tools.support_evaluation.validate_data import DATASET_VERSION
from tools.support_s5.freeze import blueprint, validate_current_regressions
from tools.support_s5.package import DATASET_V3


def write(path: Path, value: Any) -> dict[str, str]:
    """Write synthetic metadata and return its byte binding."""
    path.write_bytes(json.dumps(value).encode())
    return {"path": str(path), "sha256": sha(path.read_bytes())}


def qualifying_review(
    root: Path, definitions: dict[str, dict[str, Any]]
) -> dict[str, Any]:
    """A minimal full regression receipt, without tickets/events/policy or gold."""

    def report(strategy: str, seen: bool = False) -> dict[str, Any]:
        count = 20 if seen else 40
        return {
            "kind": "s5-seen-diagnostic" if seen else "s5-development-regression",
            "strategy": strategy,
            "dataset_version": DATASET_V3 if seen else DATASET_VERSION,
            "denominator": count,
            "proposal_audit_complete": True,
            "usage_complete": True,
            "hard_failure_cases": {"synthetic_safety": []},
            "correct_count": count,
            "full_case_evidence_count": count,
            "thresholds_met": not seen,
            "cases": [
                {
                    "case_id": f"{'S5V3' if seen else 'DEV'}-{i:03d}",
                    "status": "run",
                    "correct": True,
                    "full_case_evidence": True,
                    "adjudication_stages": {
                        "retrieval_closure": {"adjudication_recoverable": True},
                        "definition_closure": {
                            "investigation_definition_complete": True
                        },
                        "final_input_closure": {
                            "retrieval_closure": {"adjudication_recoverable": True},
                            "definition_closure": {"definition_complete": True},
                        },
                    },
                }
                for i in range(1, count + 1)
            ],
        }

    def registration(strategy: str, dataset: str, name: str) -> dict[str, str]:
        return write(
            root / name,
            {
                "profile": {"strategy": strategy},
                "dataset_version": dataset,
                "strategy_blueprint": blueprint(definitions[strategy]),
            },
        )

    value: dict[str, Any] = {
        "status": "independently_accepted",
        "reports": {},
        "development_registrations": {},
    }
    for strategy in definitions:
        bound = registration(strategy, DATASET_VERSION, strategy + "-registration.json")
        value["development_registrations"][strategy] = bound
        row = report(strategy)
        row["registration_sha256"] = bound["sha256"]
        path = root / (strategy + ".json")
        value["reports"][str(path)] = write(path, row)["sha256"]
    bound = registration("support_agent_v1", DATASET_V3, "seen-registration.json")
    row = report("support_agent_v1", True)
    row["registration_sha256"] = bound["sha256"]
    value["seen_regression"] = {
        "report": write(root / "seen-regression.json", row),
        "registration": bound,
    }
    return value


@pytest.fixture
def gates(tmp_path: Path) -> tuple[dict[str, Any], dict[str, Any], dict[str, Any]]:
    """Only identity metadata needed by blueprint; not a runnable deployment."""
    definitions = {
        strategy: {
            "schema_version": schema,
            "resources": {
                "dataset_id": DATASET_VERSION,
                "runtime_manifest_sha256": "a" * 64,
                "seed_sha256": "b" * 64,
                "tenants": [],
            },
        }
        for strategy, schema in (("support_agent_v1", 18), ("support_fixed_v1", 17))
    }
    review = qualifying_review(tmp_path, definitions)
    reports = {
        read(Path(path))["strategy"]: read(Path(path)) for path in review["reports"]
    }
    return review, definitions, reports


@pytest.mark.parametrize("fixed_low", [False, True])
def test_current_freeze_requires_gates_but_not_agent_scores_for_fixed(
    gates: tuple[dict[str, Any], dict[str, Any], dict[str, Any]], fixed_low: bool
) -> None:
    """A complete weak Fixed baseline is valid; candidate quality remains separate."""
    review, definitions, reports = gates
    if fixed_low:
        baseline = reports["support_fixed_v1"]
        baseline["correct_count"], baseline["full_case_evidence_count"] = 1, 2
        for i, row in enumerate(baseline["cases"]):
            row["correct"], row["full_case_evidence"] = i < 1, i < 2
    validate_current_regressions(review, definitions, reports)


@pytest.mark.parametrize("case_id", ["DEV-020", "S5V3-004", "S5V3-018"])
@pytest.mark.parametrize(
    "mutation", ["failed", "unmeasured", "definition", "retrieval"]
)
def test_current_guard_failure_blocks_freeze_even_when_aggregate_quality_passes(
    gates: tuple[dict[str, Any], dict[str, Any], dict[str, Any]],
    case_id: str,
    mutation: str,
) -> None:
    """Guard failure cannot hide in an otherwise passing36/38 or16/18 cohort."""
    review, definitions, reports = gates
    seen = not case_id.startswith("DEV")
    report = (
        read(Path(review["seen_regression"]["report"]["path"]))
        if seen
        else reports["support_agent_v1"]
    )
    row = next(r for r in report["cases"] if r["case_id"] == case_id)
    stages = row["adjudication_stages"]
    if mutation == "failed":
        row["correct"] = row["full_case_evidence"] = False
        report["correct_count"] -= 1
        report["full_case_evidence_count"] -= 1
    elif mutation == "unmeasured":
        stages["final_input_closure"] = None
    elif mutation == "definition":
        stages["definition_closure"]["investigation_definition_complete"] = False
    else:
        stages["retrieval_closure"]["adjudication_recoverable"] = False
    if seen:
        review["seen_regression"]["report"] = write(
            Path(review["seen_regression"]["report"]["path"]), report
        )
    with pytest.raises(EvidenceError, match="KNOWN_CASE_FREEZE_GUARD"):
        validate_current_regressions(review, definitions, reports)


@pytest.mark.parametrize(
    "mutation",
    [
        "missing",
        "hash",
        "candidate",
        "quality",
        "unattempted",
        "hard",
        "unknown",
        "scope",
    ],
)
def test_current_freeze_rejects_unbound_incomplete_or_failed_regressions(
    gates: tuple[dict[str, Any], dict[str, Any], dict[str, Any]], mutation: str
) -> None:
    """An accepted label is insufficient without real bound complete gate inputs."""
    review, definitions, reports = gates
    code = "REGRESSION_"
    if mutation == "missing":
        del review["development_registrations"]
    elif mutation == "hash":
        review["seen_regression"]["report"]["sha256"] = "0" * 64
    elif mutation == "candidate":
        definitions["support_agent_v1"] = copy.deepcopy(definitions["support_agent_v1"])
        definitions["support_agent_v1"]["schema_version"] = 16
    elif mutation == "quality":
        reports["support_agent_v1"]["full_case_evidence_count"] = 37
    elif mutation == "unattempted":
        reports["support_fixed_v1"]["cases"][-1]["status"] = "unattempted"
    elif mutation == "hard":
        reports["support_agent_v1"]["hard_failure_cases"]["synthetic_safety"] = [
            "DEV-001"
        ]
    elif mutation == "unknown":
        reports["support_agent_v1"]["usage_complete"] = False
    else:
        reference = review["seen_regression"]["report"]
        report = read(Path(reference["path"]))
        report["kind"] = "s5-formal-acceptance"
        review["seen_regression"]["report"] = write(Path(reference["path"]), report)
        code = "SEEN_REGRESSION_SCOPE"
    with pytest.raises(EvidenceError, match=code):
        validate_current_regressions(review, definitions, reports)
