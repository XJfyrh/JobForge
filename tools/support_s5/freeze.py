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
from tools.support_evaluation.s5_policy import FORMAL_POLICY, POLICY, THRESHOLDS
from tools.support_evaluation.validate_data import DATASET_VERSION
from tools.support_evaluation.validate_data import ROOT as DATA_ROOT
from tools.support_s5.package import DATASET_V3

ROOT = Path(__file__).resolve().parents[2]

# Independently reviewed (Agent, Fixed) deployment schema pairs. ADR-0034 adds
# the schema 13 candidate against the unchanged schema 12 Fixed baseline; every
# pair listed before it keeps its exact acceptance and rejection behaviour. The
# schema 11/12 pair is deliberately absent: admitting it would change existing
# rejection behaviour for schema 11, and ADR-0034 supersedes that candidate.
SCHEMA_PAIRS = frozenset(
    {(6, 5), (7, 8), (9, 10), (13, 12), (14, 12), (15, 12), (16, 17), (18, 17)}
)


def source_hashes(schema_version: int | None = None) -> dict[str, str]:
    """Bind installed execution, preparation, UI, telemetry and operator sources."""
    result = approval_sources()
    directories: tuple[str, ...] = (
        "tools/support_s5",
        "tools/support_lifecycle",
        "internal/observability",
        "internal/run/httpapi/ui",
        "cmd/agent-control",
        "api/support/s5-fixed-v1",
        "api/support/s5-agent-v1",
        "api/support/s5-agent-v2",
        "api/support/s5-fixed-v2",
        "api/support/s5-agent-v3",
        "api/support/s5-fixed-v3",
        "api/support/s5-agent-v4",
        "api/support/s5-fixed-v4",
        # ADR-0034 introduces only the schema 13 Agent deployment definition;
        # the Fixed baseline stays schema 12 (api/support/s5-fixed-v4).
        "api/support/s5-agent-v5",
        "api/support/s5-agent-v6",
        "api/support/s5-agent-v7",
        "api/support/s5-agent-v8",
        "api/support/s5-agent-v9",
        "api/support/s5-fixed-v5",
        "api/support/agent-v2",
    )
    if schema_version in {16, 17, 18}:
        directories += (
            "internal/runprotocol/v2",
            "internal/runinput",
            "api/executor/v2",
            "proto/jobforge/agent/v1",
        )
    for directory in directories:
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


def validate_current_regressions(
    review: dict[str, Any],
    definitions: dict[str, dict[str, Any]],
    reports: dict[str, dict[str, Any]],
) -> None:
    """Enforce the existing v9 development and three known-case freeze gates."""

    def bound(reference: dict[str, Any]) -> dict[str, Any]:
        need(set(reference) == {"path", "sha256"}, "REGRESSION_RECEIPT_REQUIRED")
        raw = Path(reference["path"]).read_bytes()
        need(sha(raw) == reference["sha256"], "REGRESSION_RECEIPT_CHANGED")
        value: dict[str, Any] = json.loads(raw)
        return value

    def complete(
        report: dict[str, Any], count: int, minimum: tuple[int, int] | None
    ) -> None:
        rows = report.get("cases", [])
        need(
            report.get("denominator") == count
            and len(rows) == count
            and len({r["case_id"] for r in rows}) == count
            and all(r.get("status") == "run" for r in rows)
            and report.get("proposal_audit_complete") is True
            and report.get("usage_complete") is True
            and bool(report.get("hard_failure_cases"))
            and not any(report["hard_failure_cases"].values()),
            "REGRESSION_INCOMPLETE",
        )
        if minimum is not None:
            need(
                report.get("correct_count") == sum(r["correct"] for r in rows)
                and report.get("full_case_evidence_count")
                == sum(r["full_case_evidence"] for r in rows)
                and report["correct_count"] >= minimum[0]
                and report["full_case_evidence_count"] >= minimum[1],
                "REGRESSION_QUALITY_GATE",
            )

    def registration(
        reference: dict[str, Any], report: dict[str, Any], strategy: str
    ) -> None:
        registered = bound(reference)
        need(
            reference["sha256"] == report["registration_sha256"]
            and registered["profile"]["strategy"] == strategy
            and registered["dataset_version"] == report["dataset_version"]
            and registered["strategy_blueprint"] == blueprint(definitions[strategy]),
            "REGRESSION_CANDIDATE_CHANGED",
        )

    registrations = review.get("development_registrations", {})
    need(set(registrations) == set(definitions), "REGRESSION_RECEIPT_REQUIRED")
    for strategy, report in reports.items():
        need(report["dataset_version"] == DATASET_VERSION, "DEVELOPMENT_DATASET")
        registration(registrations[strategy], report, strategy)
        complete(
            report,
            POLICY["case_count"],
            (POLICY["minimum_correct"], POLICY["minimum_full_evidence"])
            if strategy == "support_agent_v1"
            else None,
        )
    seen = review.get("seen_regression", {})
    need(set(seen) == {"report", "registration"}, "REGRESSION_RECEIPT_REQUIRED")
    report = bound(seen["report"])
    need(
        report["kind"] == "s5-seen-diagnostic"
        and report["dataset_version"] == DATASET_V3
        and report["strategy"] == "support_agent_v1"
        and report["thresholds_met"] is False,
        "SEEN_REGRESSION_SCOPE",
    )
    registration(seen["registration"], report, "support_agent_v1")
    complete(
        report,
        FORMAL_POLICY["case_count"],
        (FORMAL_POLICY["minimum_correct"], FORMAL_POLICY["minimum_full_evidence"]),
    )
    for selected, case_ids in (
        (reports["support_agent_v1"], ("DEV-020",)),
        (report, ("S5V3-004", "S5V3-018")),
    ):
        rows = {r["case_id"]: r for r in selected["cases"]}
        for case_id in case_ids:
            row = rows.get(case_id, {})
            stages = row.get("adjudication_stages", {})
            final = stages.get("final_input_closure") or {}
            need(
                row.get("correct") is True
                and row.get("full_case_evidence") is True
                and stages.get("retrieval_closure", {}).get("adjudication_recoverable")
                is True
                and stages.get("definition_closure", {}).get(
                    "investigation_definition_complete"
                )
                is True
                and final.get("retrieval_closure", {}).get("adjudication_recoverable")
                is True
                and final.get("definition_closure", {}).get("definition_complete")
                is True,
                "KNOWN_CASE_FREEZE_GUARD",
            )


def validate(value: dict[str, Any]) -> None:
    """A changed strategy/scorer/build source requires a new candidate freeze."""
    need(
        value["status"] == "frozen_before_unseen_creation"
        and value["thresholds"] == THRESHOLDS
        and value["sources"]
        == source_hashes(value.get("candidate_blueprint", {}).get("schema_version")),
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
    seen_formal_manifest: Path | None = None,
    seen_formal_v2_manifest: Path | None = None,
    seen_formal_v3_manifest: Path | None = None,
) -> dict[str, Any]:
    """Freeze verified source and regressions before opening new unseen cases."""
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
    reports = {}
    for path in review["reports"]:
        report_raw = Path(path).read_bytes()
        report = json.loads(report_raw)
        strategies.add(report["strategy"])
        reports[report["strategy"]] = report
        need(
            sha(report_raw) == review["reports"][path]
            and report["kind"] == "s5-development-regression"
            and report["denominator"] == POLICY["case_count"]
            and report["proposal_audit_complete"],
            "ACTUAL_DEVELOPMENT_REGRESSION_REQUIRED",
        )
    need(
        len(review["reports"]) == 2
        and strategies == {"support_agent_v1", "support_fixed_v1"},
        "BOTH_DEVELOPMENT_STRATEGIES_REQUIRED",
    )
    agent, fixed = (
        read(p)["profiles"][0]["definition"] for p in (agent_config, fixed_config)
    )
    sources = source_hashes(agent["schema_version"])
    build = read(build_receipt)
    need(
        build["source_commit"] == head
        and build["sources"] == sources
        and build["production_registry"] == ["support-fixed-v1", "support-agent-v1"]
        and build["gold_present"] is False,
        "BUILD_RECEIPT_MISMATCH",
    )
    need(
        (agent["schema_version"], fixed["schema_version"]) in SCHEMA_PAIRS
        and agent["model"] == fixed["model"]
        and agent["resources"] == fixed["resources"],
        "COMPARISON_PROFILE_MISMATCH",
    )
    if agent["schema_version"] == 18:
        validate_current_regressions(
            review,
            {"support_agent_v1": agent, "support_fixed_v1": fixed},
            reports,
        )
    # Each schema binds exactly the formal cohorts already seen when it was
    # proposed. ADR-0034's schema 13 therefore binds v1+v2+v3: the third formal
    # package is seen too, so it can never serve as the unseen acceptance set.
    agent_schema: int = agent["schema_version"]
    seen = {}
    if agent_schema in {7, 9, 13, 14, 15, 16, 18}:
        need(seen_formal_manifest is not None, "SEEN_FORMAL_PROVENANCE_REQUIRED")
        assert seen_formal_manifest is not None
        seen = seen_package(seen_formal_manifest, "support-s5-2026-10-07-v1")
    need(
        agent_schema in {9, 13, 14, 15, 16, 18} or seen_formal_v2_manifest is None,
        "SEEN_FORMAL_PROVENANCE_SCOPE",
    )
    need(
        agent_schema in {13, 14, 15, 16, 18} or seen_formal_v3_manifest is None,
        "SEEN_FORMAL_PROVENANCE_SCOPE",
    )
    seen_v2 = {}
    if agent_schema in {9, 13, 14, 15, 16, 18}:
        need(seen_formal_v2_manifest is not None, "SEEN_FORMAL_PROVENANCE_REQUIRED")
        assert seen_formal_v2_manifest is not None
        seen_v2 = seen_package(seen_formal_v2_manifest, "support-s5-2026-10-07-v2")
    seen_v3 = {}
    if agent_schema in {13, 14, 15, 16, 18}:
        need(seen_formal_v3_manifest is not None, "SEEN_FORMAL_PROVENANCE_REQUIRED")
        assert seen_formal_v3_manifest is not None
        seen_v3 = seen_package(seen_formal_v3_manifest, "support-s5-2026-10-08-v3")
    result = {
        "schema_version": 1,
        "status": "frozen_before_unseen_creation",
        "frozen_at": datetime.now(UTC).isoformat(),
        "source_commit": head,
        "sources": sources,
        "thresholds": THRESHOLDS,
        "candidate_blueprint": blueprint(agent),
        "baseline_blueprint": blueprint(fixed),
        "development_manifest_sha256": sha((DATA_ROOT / "manifest.json").read_bytes()),
        "development_review_sha256": sha(development_review.read_bytes()),
        "build_receipt_sha256": sha(build_receipt.read_bytes()),
        "images": build["images"],
        "model": agent["model"],
    }
    if seen_v3:
        result["seen_formal_packages"] = [seen, seen_v2, seen_v3]
    elif seen_v2:
        result["seen_formal_packages"] = [seen, seen_v2]
    elif seen:
        result["seen_formal_package"] = seen
    return result


def seen_package(path: Path, expected_dataset: str) -> dict[str, Any]:
    """Bind one actual previously reviewed package, preserving its raw bytes."""
    manifest_raw = path.read_bytes()
    manifest = json.loads(manifest_raw)
    review_raw = (path.parent / "evaluation/family-review.json").read_bytes()
    prior_review = json.loads(review_raw)
    need(
        manifest["dataset_id"] == expected_dataset
        and sha(review_raw)
        == manifest["artifact_sha256"]["evaluation/family-review.json"]
        and prior_review["status"] == "independently_accepted",
        "SEEN_FORMAL_PROVENANCE",
    )
    return {
        "dataset_id": manifest["dataset_id"],
        "manifest_sha256": sha(manifest_raw),
        "template_families": sorted(
            {c["template_family"] for c in prior_review["cases"]}
        ),
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
    parser.add_argument("--seen-formal-manifest", type=Path)
    parser.add_argument("--seen-formal-v2-manifest", type=Path)
    parser.add_argument("--seen-formal-v3-manifest", type=Path)
    args = parser.parse_args()
    save_new(
        args.out,
        create(
            args.agent_config,
            args.fixed_config,
            args.development_review,
            args.build_receipt,
            args.seen_formal_manifest,
            args.seen_formal_v2_manifest,
            args.seen_formal_v3_manifest,
        ),
    )


if __name__ == "__main__":
    main()
