"""Freeze the eleven S3 Runs offline without submitting or enabling execution."""

from __future__ import annotations

import argparse
import copy
import hashlib
import json
from pathlib import Path
from typing import Any

from tools.support_evaluation.assemble import register, save_new
from tools.support_evaluation.driver import instant
from tools.support_evaluation.evidence import fingerprint, load_package, read_json
from tools.support_evaluation.export import json_bytes

ROOT = Path(__file__).resolve().parents[2]
SPECS = (
    ("DEV-002", "C", "first_search_commit", "worker"),
    ("DEV-002", "H0", "first_search_commit", "worker"),
    ("DEV-002", "H1", "none", "none"),
    ("DEV-027", "C", "first_read_commit", "worker"),
    ("DEV-027", "H0", "first_read_commit", "worker"),
    ("DEV-027", "H1", "none", "none"),
    ("DEV-035", "C", "second_search_decision", "worker"),
    ("DEV-035", "H0", "second_search_decision", "worker"),
    ("DEV-035", "H1", "none", "none"),
    ("DEV-002", "F03", "first_chat_observation", "worker"),
    ("DEV-035", "F07", "first_chat_observation", "step"),
)


def source_hashes() -> dict[str, str]:
    """Bind reviewed scoring, proxy, driver and runtime sources to this release."""
    files = (
        list((ROOT / "tools/support_recovery").glob("*.py"))
        + list((ROOT / "tools/support_evaluation").glob("*.py"))
        + list((ROOT / "python/jobforge_agent").glob("*.py"))
        + [ROOT / "tools/supportrecoveryproxy/main.go"]
        + [ROOT / "api/support/recovery-v1/profile-schema.json"]
        + list((ROOT / "internal/run").rglob("*.go"))
        + list((ROOT / "internal/runworker").glob("*.go"))
        + list((ROOT / "internal/runexecutor").glob("*.go"))
        + list((ROOT / "cmd/agent-control").glob("*.go"))
        + list((ROOT / "cmd/agent-worker").glob("*.go"))
        + list((ROOT / "sdk/python/jobforge").glob("*.py"))
        + [ROOT / "deploy/Dockerfile.agent-worker"]
        + [ROOT / "deploy/Dockerfile.support-recovery"]
        + [ROOT / "tools/support_recovery/control_audit.sql"]
        + [ROOT / "tools/support_evaluation/business_audit.sql"]
    )
    return {
        file.relative_to(ROOT).as_posix(): hashlib.sha256(file.read_bytes()).hexdigest()
        for file in sorted(files)
        if not file.name.startswith("test_")
        and not file.name.endswith("_test.go")
        and file.name != "fixtures.py"
    }


def freeze(config: Path, replacement: str, *, s5: bool = False) -> dict[str, Any]:
    """Reuse the original source review, then register only these new intentions."""
    registered = register(config, s5=s5)
    profile = registered["profile"]
    control, _ = read_json(config / "control.disabled.json", 1 << 20)
    launch, _ = read_json(config / "launch.json", 1 << 20)
    definition = control["profiles"][0]["definition"]
    version = definition["schema_version"]
    if (
        version not in ({7, 9} if s5 else {3})
        or s5
        and definition["program"]["prompt_version"]
        != ("support-agent-prompt-v3" if version == 9 else "support-agent-prompt-v2")
        or definition["program"]["recovery_policy"] != "confirmed_uncommitted_v1"
        or profile["executor_version"]
        != ("linux-v2-approval-runtime-1" if s5 else "linux-v2-recovery-runtime-1")
        or s5
        and version == 7
        and next(row for row in control["budgets"] if row["scope"] == "batch")[
            "limits"
        ]["chat"]
        > 132
        or any(
            row["limits"]["cost_microyuan"] > (4_000_000 if s5 else 5_000_000)
            for row in control["budgets"]
        )
        or (
            instant(launch["valid_until"]) - instant(launch["valid_from"])
        ).total_seconds()
        > 6 * 3600
        or replacement == launch["worker_id"]
        or not replacement
    ):
        raise ValueError("S3_SCOPE_MISMATCH")
    cases = {row["case_id"]: row for row in registered["bindings"]}
    rows = []
    for ordinal, (case_id, arm, boundary, target) in enumerate(SPECS, 1):
        binding = copy.deepcopy(cases[case_id])
        identity = f"s3-{launch['batch_account_id']}-{ordinal:02d}"
        binding["business_request_key"] = identity
        rows.append(
            {
                "ordinal": ordinal,
                "experiment": f"{case_id}-{arm}",
                "case_id": case_id,
                "arm": arm,
                "boundary": boundary,
                "target": target,
                "idempotency_key": f"submit-{identity}",
                "binding": binding,
            }
        )
    return {
        "schema_version": (3 if version == 9 else 2) if s5 else 1,
        "max_runs": 11,
        "max_cost_microyuan": next(
            row for row in control["budgets"] if row["scope"] == "batch"
        )["limits"]["cost_microyuan"]
        if s5
        else 5_000_000,
        "max_seconds": 6 * 3600,
        "workers": [launch["worker_id"], replacement],
        "profile": profile,
        "definition": definition,
        "control_sha256": {
            mode: hashlib.sha256(
                json_bytes(replacement_config(config, replacement, mode))
            ).hexdigest()
            for mode in ("disabled", "enabled")
        },
        "batch_account_id": launch["batch_account_id"],
        "batch_key": launch["batch_key"],
        "valid_from": launch["valid_from"],
        "valid_until": launch["valid_until"],
        "registration_sha256": fingerprint(
            "jobforge.support.s3.registration.v1",
            json.dumps(registered, sort_keys=True, separators=(",", ":")),
        ),
        "preparation_sha256": hashlib.sha256(
            (config / "launch.json").read_bytes()
        ).hexdigest(),
        "config_sha256": launch["config_sha256"],
        "sources": source_hashes(),
        "package_sha256": load_package().hashes,
        "runs": rows,
    }


def validate(plan: dict[str, Any], *, check_sources: bool = True) -> None:
    """Fail closed on changed source or execution scope before any POST."""
    if (
        plan["schema_version"] not in {1, 2, 3}
        or plan["max_runs"] != 11
        or (
            not (
                type(plan["max_cost_microyuan"]) is int
                and 0 < plan["max_cost_microyuan"] <= 4_000_000
            )
            if plan["schema_version"] in {2, 3}
            else plan["max_cost_microyuan"] != 5_000_000
        )
        or plan["max_seconds"] != 6 * 3600
        or (check_sources and plan["sources"] != source_hashes())
        or (check_sources and plan["package_sha256"] != load_package().hashes)
        or len(plan["workers"]) != 2
        or len(set(plan["workers"])) != 2
        or not all(plan["workers"])
        or len(plan["runs"]) != len(SPECS)
        or plan["profile"]["executor_version"]
        != (
            "linux-v2-approval-runtime-1"
            if plan["schema_version"] in {2, 3}
            else "linux-v2-recovery-runtime-1"
        )
        or plan["definition"]["schema_version"]
        != {1: 3, 2: 7, 3: 9}[plan["schema_version"]]
        or plan["schema_version"] in {2, 3}
        and plan["definition"]["program"]["prompt_version"]
        != (
            "support-agent-prompt-v3"
            if plan["schema_version"] == 3
            else "support-agent-prompt-v2"
        )
        or plan["definition"]["program"]["recovery_policy"]
        != "confirmed_uncommitted_v1"
        or (instant(plan["valid_until"]) - instant(plan["valid_from"])).total_seconds()
        > 6 * 3600
        or instant(plan["valid_until"]) <= instant(plan["valid_from"])
    ):
        raise ValueError("S3_PLAN_CHANGED")
    for ordinal, (row, spec) in enumerate(zip(plan["runs"], SPECS, strict=True), 1):
        identity = f"s3-{plan['batch_account_id']}-{ordinal:02d}"
        if (
            (row["case_id"], row["arm"], row["boundary"], row["target"]) != spec
            or row["ordinal"] != ordinal
            or row["experiment"] != f"{spec[0]}-{spec[1]}"
            or row["idempotency_key"] != f"submit-{identity}"
            or row["binding"]["business_request_key"] != identity
        ):
            raise ValueError("S3_EXECUTION_LIST_CHANGED")


def replacement_config(config: Path, replacement: str, mode: str) -> dict[str, Any]:
    """Create a separate S3 configuration; preserve all original prepared files."""
    control, _ = read_json(config / f"control.{mode}.json", 1 << 20)
    if len(control["workers"]) != 1:
        raise ValueError("S3_PREPARATION_WORKERS_INVALID")
    second = copy.deepcopy(control["workers"][0])
    second["worker_id"] = replacement
    control["workers"].append(second)
    return control


def main() -> None:
    """Produce a new private plan; launch lives in a separate release-only CLI."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path, required=True)
    parser.add_argument("--replacement-worker", required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--check-plan", type=Path)
    parser.add_argument(
        "--s5",
        action="store_true",
        help="schema 7/9 candidate; stop at saved proposal without approving effects",
    )
    args = parser.parse_args()
    if args.out.resolve().is_relative_to(ROOT):
        raise ValueError("OUTPUT_MUST_BE_OUTSIDE_REPOSITORY")
    if args.check_plan:
        raw = args.check_plan.read_bytes()
        plan = json.loads(raw)
        validate(plan)
        if freeze(args.config, args.replacement_worker, s5=args.s5) != plan:
            raise ValueError("S3_PREPARATION_CHANGED")
        save_new(
            args.out,
            {
                "schema_version": 1,
                "execution_list_sha256": hashlib.sha256(raw).hexdigest(),
                "source_check_passed": True,
            },
        )
    else:
        save_new(args.out, freeze(args.config, args.replacement_worker, s5=args.s5))
        for mode in ("disabled", "enabled"):
            save_new(
                args.out.parent / f"control.s3.{mode}.json",
                replacement_config(args.config, args.replacement_worker, mode),
            )


if __name__ == "__main__":
    main()
