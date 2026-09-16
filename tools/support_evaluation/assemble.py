"""Freeze actual deployment registration and assemble protected offline evidence.

This CLI never submits work or creates model results. It joins the SDK archive,
the actual outbound metadata and separately captured read-only database facts.
"""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path
from typing import Any

from tools.support_evaluation.driver import instant
from tools.support_evaluation.evidence import load_package, registration
from tools.support_evaluation.export import json_bytes
from tools.support_evaluation.validate_data import DATASET_VERSION, POLICY_VERSION

REQUEST_KEYS = {
    "physical_call_id",
    "tenant_id",
    "snapshot_id",
    "profile_hash",
    "subcall",
    "method",
    "endpoint_alias",
    "origin",
    "resource_id",
    "model",
    "thinking",
    "max_tokens",
}
FACT_TABLES = {
    "dataset_imports",
    "policy_versions",
    "tickets",
    "orders",
    "deliveries",
    "policy_indexes",
    "policy_chunks",
}


def read(path: Path) -> dict[str, Any]:
    """Read one explicit trusted local artifact."""
    value: dict[str, Any] = json.loads(path.read_bytes())
    return value


def sha(raw: bytes) -> str:
    """Hash source bytes without rewriting their contents."""
    return hashlib.sha256(raw).hexdigest()


def save_new(path: Path, value: dict[str, Any]) -> None:
    """Keep registration and historical exports immutable on rerun."""
    with path.open("xb") as output:
        output.write(json_bytes(value))
    path.chmod(0o600)


def register(config: Path) -> dict[str, Any]:
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
        "scorer_version": "support-offline-v1",
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
    registration(result, package)
    return result


def outbound(directory: Path) -> tuple[dict[str, list[dict[str, Any]]], str, bool]:
    """Retain every observed send, including repeated and incomplete sends."""
    grouped: dict[str, list[dict[str, Any]]] = {}
    receipts = []
    valid = True
    for path in sorted(directory.glob("*.jsonl")):
        raw = path.read_bytes()
        receipts.append({"file": path.name, "sha256": sha(raw)})
        for line in raw.splitlines():
            try:
                event = json.loads(line)
                if (
                    set(event)
                    != REQUEST_KEYS
                    | {
                        "schema_version",
                        "event",
                        "time",
                        "http_status",
                        "response_complete",
                    }
                    or event["schema_version"] != 1
                ):
                    raise ValueError("OUTBOUND_SHAPE")
                grouped.setdefault(event["snapshot_id"], []).append(event)
            except (ValueError, KeyError, TypeError):
                valid = False
    return grouped, sha(json_bytes(receipts)), valid


def safety(
    events: list[dict[str, Any]],
    trace_hash: str,
    before: dict[str, Any],
    after: dict[str, Any],
    audit_hash: str,
    complete: bool,
) -> dict[str, Any]:
    """Combine send metadata with actual role and before/after state checks.

    Equal fact hashes alone do not rule out temporary changes. The read-only
    role and fixed business API are required, and snapshots are deliberately
    excluded because control admission is allowed to insert them.
    """
    requests = []
    by_call: dict[str, list[dict[str, Any]]] = {}
    for event in events:
        by_call.setdefault(event["physical_call_id"], []).append(event)
        if event["event"] == "dispatch_attempt":
            requests.append({key: event[key] for key in REQUEST_KEYS})
    for records in by_call.values():
        records.sort(key=lambda record: record["time"])
        complete = (
            complete
            and [record["event"] for record in records]
            == [
                "dispatch_attempt",
                "http_response",
                "finish",
            ]
            and records[-1]["response_complete"] is True
        )
        complete = complete and all(
            {key: record[key] for key in REQUEST_KEYS}
            == {key: records[0][key] for key in REQUEST_KEYS}
            for record in records
        )
    writes = []
    expected_privileges = {name: False for name in FACT_TABLES}
    for sample in (before, after):
        complete = complete and (
            sample["database"] == before["database"]
            and sample["role"] == "jobforge_business_reader"
            and sample["write_privileges"] == expected_privileges
            and set(sample["facts"]) == FACT_TABLES
        )
    for name in FACT_TABLES:
        if before["facts"].get(name) != after["facts"].get(name):
            writes.append({"table": name, "code": "BUSINESS_FACTS_CHANGED"})
    if events:
        complete = complete and (
            instant(before["observed_at"]) <= min(instant(e["time"]) for e in events)
            and instant(after["observed_at"]) >= max(instant(e["time"]) for e in events)
        )
    return {
        "complete": complete,
        "trace_sha256": trace_hash,
        "business_audit_sha256": audit_hash,
        "requests": requests,
        "writes": writes,
    }


def assemble(
    registered: Path,
    archive: Path,
    metadata: Path,
    before_path: Path,
    after_path: Path,
) -> dict[str, Any]:
    """Join original case rows to SDK evidence without constructing missing results."""
    registration_raw = registered.read_bytes()
    registered_value = json.loads(registration_raw)
    registration(registered_value, load_package())
    rows = read(archive / "rows.json")["cases"]
    if [row["case_id"] for row in rows] != [
        row["case_id"] for row in registered_value["bindings"]
    ]:
        raise ValueError("CASE_COVERAGE")
    grouped, trace_hash, metadata_valid = outbound(metadata)
    before, after = read(before_path), read(after_path)
    audit_hash = sha(
        json_bytes(
            {
                "before": sha(before_path.read_bytes()),
                "after": sha(after_path.read_bytes()),
            }
        )
    )
    known_snapshots = set()
    results = []
    for row in rows:
        value: dict[str, Any] = {
            "case_id": row["case_id"],
            "status": "unattempted",
            "error_code": "",
            "run": None,
            "steps": [],
            "result": None,
            "calls": None,
            "safety": None,
        }
        if row["run_id"] is not None:
            # A known accepted Run with missing export must be exported again;
            # do not relabel it a failed submission or fabricate usage/results.
            actual = read(archive / row["case_id"] / "evidence.json")
            if actual["run"]["run_id"] != row["run_id"]:
                raise ValueError("RUN_BINDING")
            snapshot = actual["run"]["snapshot_id"]
            known_snapshots.add(snapshot)
            value.update(actual, status="run")
            value["safety"] = safety(
                grouped.get(snapshot, []),
                trace_hash,
                before,
                after,
                audit_hash,
                metadata_valid,
            )
        elif row["status"] != "unattempted":
            value.update(status="submission_failed", error_code="ACCEPTANCE_UNKNOWN")
        results.append(value)
    if set(grouped) - known_snapshots:
        for value in results:
            if value["safety"] is not None:
                value["safety"]["complete"] = False
    return {
        "schema_version": 1,
        "registration_sha256": sha(registration_raw),
        "cases": results,
    }


def main() -> None:
    """Expose separate pre-submit registration and post-run assembly commands."""
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="mode", required=True)
    prepare = commands.add_parser("register")
    prepare.add_argument("--config", type=Path, required=True)
    prepare.add_argument("--out", type=Path, required=True)
    collect = commands.add_parser("collect")
    for name in ("registration", "export", "outbound", "before", "after", "out"):
        collect.add_argument("--" + name, type=Path, required=True)
    args = parser.parse_args()
    value = (
        register(args.config)
        if args.mode == "register"
        else assemble(
            args.registration,
            args.export,
            args.outbound,
            args.before,
            args.after,
        )
    )
    save_new(args.out, value)


if __name__ == "__main__":
    main()
