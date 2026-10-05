"""Freeze a separate finite S4 list; S1–S3 registrations remain unchanged."""

from __future__ import annotations

import argparse
import copy
from pathlib import Path
from typing import Any

from tools.support_evaluation.assemble import read, save_new, sha
from tools.support_evaluation.driver import instant
from tools.support_evaluation.evidence import load_package
from tools.support_evaluation.export import json_bytes
from tools.support_evaluation.validate_data import DATASET_VERSION, POLICY_VERSION
from tools.support_recovery.plan import source_hashes as recovery_sources

ROOT = Path(__file__).resolve().parents[2]
SPECS = (
    ("DEV-001", "ordinary_record"),
    ("DEV-016", "request_information"),
    ("DEV-011", "escalate"),
    ("DEV-002", "reject_valid_proposal"),
    ("DEV-003", "ticket_revision_conflict"),
    ("DEV-004", "commit_loss_natural_recovery"),
    ("DEV-005", "commit_cancel_reconcile"),
    ("DEV-006", "commit_cancel_retry"),
    ("DEV-007", "authorization_cancel_retry"),
    ("DEV-035", "hard_quality_observation"),
)
RETRIES = ("DEV-006", "DEV-007")


def registration(config: Path) -> dict[str, Any]:
    """Build a real registration from prepared deployment, never test fixtures."""
    manifest = read(config / "launch.json")
    for name, digest in manifest["config_sha256"].items():
        if sha((config / name).read_bytes()) != digest:
            raise ValueError("CONFIG_MISMATCH")
    control = read(config / "control.disabled.json")
    worker = read(config / "worker.json")
    profile = control["profiles"][0]
    resources = profile["definition"]["resources"]
    tenants = {row["tenant_id"]: row for row in resources["tenants"]}
    accounts = {row["account_id"]: row for row in control["budgets"]}
    bindings = {row["tenant_id"]: row for row in control["bindings"]}
    package = load_package()
    family = {
        "chat": 12,
        "logical_tools": 8,
        "query_embedding": 8,
        "profile_metadata_http": 16,
        "business_tool_http": 8,
        "physical_http": 44,
        "protocol_corrections": 1,
        "tokens": profile["family_token_limit"],
        "cost_microyuan": profile["family_cost_microyuan"],
    }
    endpoints = worker["tenants"]["tenant-north"]
    result = {
        "schema_version": 1,
        "scorer_version": "support-approval-quality-v1",
        "evidence_origin": "run_api_export",
        "dataset_version": DATASET_VERSION,
        "policy_version": POLICY_VERSION,
        "gold_sha256": package.hashes["evaluation/dev_gold.jsonl"],
        "scoring_sha256": package.hashes["evaluation/scoring-proposal.json"],
        "anchors_sha256": package.hashes["evaluation/semantic-anchors.json"],
        "corpus_sha256": package.corpus_hash,
        "profile": {
            **{
                key: profile[key]
                for key in (
                    "profile_id",
                    "profile_hash",
                    "strategy",
                    "executor_version",
                    "expected_response_model",
                    "provider_audit_policy",
                    "max_input_tokens",
                    "max_output_tokens",
                )
            },
            "proposal_schema": profile["definition"]["program"]["proposal_schema"],
            "price_hash": profile["pricing"]["hash"],
            "budget_batch_id": manifest["batch_key"],
            "pricing": {k: v for k, v in profile["pricing"].items() if k != "hash"},
            "origins": {
                "business": endpoints["business_origin"],
                "ollama": endpoints["ollama_origin"],
                "deepseek": profile["definition"]["model"]["origin"],
            },
        },
        "bindings": [
            {
                **{
                    key: row[key]
                    for key in (
                        "case_id",
                        "tenant_id",
                        "ticket_id",
                        "business_request_key",
                    )
                },
                "as_of": resources["observed_at"],
                "index_id": tenants[row["tenant_id"]]["index_id"],
                "index_profile_hash": resources["index_profile_hash"],
                "index_content_hash": tenants[row["tenant_id"]]["index_content_hash"],
                "budget_limits": {
                    "family": family,
                    "tenant": accounts[bindings[row["tenant_id"]]["tenant_account_id"]][
                        "limits"
                    ],
                    "batch": accounts[manifest["batch_account_id"]]["limits"],
                },
            }
            for row in manifest["cases"]
        ],
    }
    return result


def source_hashes() -> dict[str, str]:
    """Bind new action sources, migrations and operator tools in addition to S3."""
    result = recovery_sources()
    files = (
        list((ROOT / "tools/support_approval").glob("*.py"))
        + list((ROOT / "tools/support_approval").glob("*.sql"))
        + list((ROOT / "tools/supportapprovalproxy").glob("*.go"))
        + list((ROOT / "internal/business").glob("*.go"))
        + list((ROOT / "cmd/support-business").glob("*.go"))
        + list((ROOT / "api/business-action").rglob("*.json"))
        + list((ROOT / "api/support/approval-v1").glob("*.json"))
        + list((ROOT / "migrations").rglob("*.sql"))
        + [ROOT / "proto/jobforge/agent/v1/agent.proto"]
        + [ROOT / "deploy/Dockerfile.support-approval"]
    )
    for file in files:
        if not file.name.startswith("test_") and not file.name.endswith("_test.go"):
            result[file.relative_to(ROOT).as_posix()] = sha(file.read_bytes())
    return dict(sorted(result.items()))


def control_config(config: Path, replacement: str, mode: str) -> dict[str, Any]:
    """Keep preparation immutable and add one separately registered principal."""
    control = read(config / f"control.{mode}.json")
    if (
        len(control["workers"]) != 1
        or replacement == control["workers"][0]["worker_id"]
    ):
        raise ValueError("S4_WORKERS_INVALID")
    second = copy.deepcopy(control["workers"][0])
    second["worker_id"] = replacement
    control["workers"].append(second)
    return control


def freeze(config: Path, replacement: str) -> dict[str, Any]:
    """Declare ten new source intents and two bounded receipt-only successors."""
    registered = registration(config)
    launch = read(config / "launch.json")
    control = control_config(config, replacement, "disabled")
    profile = control["profiles"][0]
    cases = {row["case_id"]: row for row in registered["bindings"]}
    rows = []
    for ordinal, (case, intent) in enumerate(SPECS, 1):
        identity = f"s4-{launch['batch_account_id']}-{ordinal:02d}"
        binding = {**cases[case], "business_request_key": identity}
        rows.append(
            {
                "ordinal": ordinal,
                "case_id": case,
                "intent": intent,
                "idempotency_key": "submit-" + identity,
                "binding": binding,
            }
        )
    result = {
        "schema_version": 1,
        "stage": "S4",
        "max_runs": 12,
        "max_cost_microyuan": 5_000_000,
        "max_seconds": 6 * 3600,
        "cap_origin": "planning_chat_scope_choice_not_human_spoken_amount",
        "workers": [launch["worker_id"], replacement],
        "profile": registered["profile"],
        "definition": profile["definition"],
        "batch_account_id": launch["batch_account_id"],
        "batch_key": launch["batch_key"],
        "valid_from": launch["valid_from"],
        "valid_until": launch["valid_until"],
        "control_sha256": {
            mode: sha(json_bytes(control_config(config, replacement, mode)))
            for mode in ("disabled", "enabled")
        },
        "config_sha256": launch["config_sha256"],
        "preparation_sha256": sha((config / "launch.json").read_bytes()),
        "sources": source_hashes(),
        "package_sha256": load_package().hashes,
        "runs": rows,
        "retry_sources": list(RETRIES),
        "approval_rule": "original_pending_protocol_sources_frozen_predicates_complete_safety",
    }
    validate(result)
    if any(row["limits"]["cost_microyuan"] > 5_000_000 for row in control["budgets"]):
        raise ValueError("S4_BUDGET_SCOPE_MISMATCH")
    return result


def validate(plan: dict[str, Any], *, check_sources: bool = True) -> None:
    """Fail closed on changed source, capability, list, budget or public signer."""
    definition = plan["definition"]
    if (
        plan["schema_version"] != 1
        or plan["stage"] != "S4"
        or plan["max_runs"] != 12
        or plan["max_cost_microyuan"] != 5_000_000
        or plan["max_seconds"] != 6 * 3600
        or len(plan["runs"]) != 10
        or plan["retry_sources"] != list(RETRIES)
        or plan["approval_rule"]
        != "original_pending_protocol_sources_frozen_predicates_complete_safety"
        or plan["cap_origin"] != "planning_chat_scope_choice_not_human_spoken_amount"
        or plan["profile"]["executor_version"] != "linux-v2-approval-runtime-1"
        or definition["schema_version"] != 4
        or definition["program"]["approval_policy"] != "ticket_resolution_v1"
        or definition["action"]["operation"] != "apply_ticket_resolution"
        or len(plan["workers"]) != 2
        or len(set(plan["workers"])) != 2
        or not all(plan["workers"])
        or not 0
        < (instant(plan["valid_until"]) - instant(plan["valid_from"])).total_seconds()
        <= 21600
        or (check_sources and plan["sources"] != source_hashes())
        or (check_sources and plan["package_sha256"] != load_package().hashes)
    ):
        raise ValueError("S4_PLAN_CHANGED")
    for ordinal, (row, spec) in enumerate(zip(plan["runs"], SPECS, strict=True), 1):
        identity = f"s4-{plan['batch_account_id']}-{ordinal:02d}"
        case = load_package().cases[spec[0]]
        if (
            (row["case_id"], row["intent"]) != spec
            or row["ordinal"] != ordinal
            or row["idempotency_key"] != "submit-" + identity
            or row["binding"]["business_request_key"] != identity
            or any(
                row["binding"][key] != case[key] for key in ("tenant_id", "ticket_id")
            )
        ):
            raise ValueError("S4_EXECUTION_LIST_CHANGED")


def main() -> None:
    """Freeze outside the repository; this command never enables or submits."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path, required=True)
    parser.add_argument("--replacement-worker", required=True)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    if args.out.resolve().is_relative_to(ROOT):
        raise ValueError("OUTPUT_MUST_BE_OUTSIDE_REPOSITORY")
    save_new(args.out, freeze(args.config, args.replacement_worker))


if __name__ == "__main__":
    main()
