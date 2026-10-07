"""Freeze code, prototypes and thresholds after actual development regression."""

from __future__ import annotations

import argparse
import copy
import json
import subprocess
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from tools.support_approval.plan import source_hashes as approval_sources
from tools.support_evaluation.assemble import read, save_new, sha
from tools.support_evaluation.evidence import need
from tools.support_evaluation.validate_data import ROOT as DATA_ROOT
from tools.support_s5.score import THRESHOLDS

ROOT = Path(__file__).resolve().parents[2]


def source_hashes() -> dict[str, str]:
    """Bind installed execution, preparation, UI, telemetry and operator sources."""
    result = approval_sources()
    for directory in (
        "tools/support_s5",
        "tools/support_lifecycle",
        "internal/observability",
        "internal/run/httpapi/ui",
        "cmd/agent-control",
        "api/support/s5-fixed-v1",
        "api/support/s5-agent-v1",
    ):
        for file in (ROOT / directory).rglob("*"):
            if (
                file.is_file()
                and file.suffix
                in {".go", ".py", ".sql", ".json", ".js", ".html", ".css", ".svg"}
                and not file.name.startswith("test_")
                and not file.name.endswith("_test.go")
            ):
                result[file.relative_to(ROOT).as_posix()] = sha(file.read_bytes())
    result["deploy/Dockerfile.support-s5"] = sha(
        (ROOT / "deploy/Dockerfile.support-s5").read_bytes()
    )
    return dict(sorted(result.items()))


def blueprint(definition: dict[str, Any]) -> dict[str, Any]:
    """Only separately reviewed dataset/index identities may bind after freeze."""
    value = copy.deepcopy(definition)
    resources = value["resources"]
    for key in ("dataset_id", "runtime_manifest_sha256", "seed_sha256"):
        del resources[key]
    for tenant in resources["tenants"]:
        del tenant["index_id"]
        del tenant["index_content_hash"]
    return value


def validate(value: dict[str, Any]) -> None:
    """A changed strategy/scorer/build source requires a new candidate freeze."""
    need(
        value["status"] == "frozen_before_unseen_creation"
        and value["thresholds"] == THRESHOLDS
        and value["sources"] == source_hashes(),
        "CANDIDATE_FREEZE_CHANGED",
    )


def validate_blueprint(
    value: dict[str, Any], strategy: str, actual: dict[str, Any]
) -> None:
    """Bind new data identities without reopening prompt, capability or limits."""
    validate(value)
    key = {
        "support_agent_v1": "candidate_blueprint",
        "support_fixed_v1": "baseline_blueprint",
    }.get(strategy)
    need(key is not None and actual == value[key], "STRATEGY_BLUEPRINT_CHANGED")


def create(
    agent_config: Path,
    fixed_config: Path,
    development_review: Path,
    build_receipt: Path,
) -> dict[str, Any]:
    """No formal cases are read, generated or executed by candidate freeze."""
    status = subprocess.run(
        ["git", "status", "--porcelain"], cwd=ROOT, capture_output=True, check=True
    ).stdout
    need(not status, "FREEZE_REQUIRES_CLEAN_COMMITTED_SOURCE")
    head = (
        subprocess.run(
            ["git", "rev-parse", "HEAD"], cwd=ROOT, capture_output=True, check=True
        )
        .stdout.decode()
        .strip()
    )
    review = read(development_review)
    need(review["status"] == "independently_accepted", "DEVELOPMENT_REVIEW_REQUIRED")
    strategies = set()
    for path in review["reports"]:
        report_raw = Path(path).read_bytes()
        report = json.loads(report_raw)
        strategies.add(report["strategy"])
        need(
            sha(report_raw) == review["reports"][path]
            and report["kind"] == "s5-development-regression"
            and report["denominator"] == 40
            and report["proposal_audit_complete"],
            "ACTUAL_DEVELOPMENT_REGRESSION_REQUIRED",
        )
    need(
        len(review["reports"]) == 2
        and strategies == {"support_agent_v1", "support_fixed_v1"},
        "BOTH_DEVELOPMENT_STRATEGIES_REQUIRED",
    )
    build = read(build_receipt)
    need(
        build["source_commit"] == head
        and build["sources"] == source_hashes()
        and build["production_registry"] == ["support-fixed-v1", "support-agent-v1"]
        and build["gold_present"] is False,
        "BUILD_RECEIPT_MISMATCH",
    )
    agent, fixed = (
        read(p)["profiles"][0]["definition"] for p in (agent_config, fixed_config)
    )
    need(
        agent["schema_version"] == 6
        and fixed["schema_version"] == 5
        and agent["model"] == fixed["model"]
        and agent["resources"] == fixed["resources"],
        "COMPARISON_PROFILE_MISMATCH",
    )
    return {
        "schema_version": 1,
        "status": "frozen_before_unseen_creation",
        "frozen_at": datetime.now(UTC).isoformat(),
        "source_commit": head,
        "sources": source_hashes(),
        "thresholds": THRESHOLDS,
        "candidate_blueprint": blueprint(agent),
        "baseline_blueprint": blueprint(fixed),
        "development_manifest_sha256": sha((DATA_ROOT / "manifest.json").read_bytes()),
        "development_review_sha256": sha(development_review.read_bytes()),
        "build_receipt_sha256": sha(build_receipt.read_bytes()),
        "images": build["images"],
        "model": agent["model"],
    }


def main() -> None:
    """Write an immutable external receipt only when all prior gates are met."""
    parser = argparse.ArgumentParser(description=__doc__)
    for name in (
        "agent-config",
        "fixed-config",
        "development-review",
        "build-receipt",
        "out",
    ):
        parser.add_argument("--" + name, type=Path, required=True)
    args = parser.parse_args()
    save_new(
        args.out,
        create(
            args.agent_config,
            args.fixed_config,
            args.development_review,
            args.build_receipt,
        ),
    )


if __name__ == "__main__":
    main()
