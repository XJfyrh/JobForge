"""Score all eleven frozen S3 intentions from private exports, without network."""

from __future__ import annotations

import argparse
from collections import Counter
from pathlib import Path
from typing import Any

from tools.support_evaluation.assemble import outbound, safety, save_new, sha
from tools.support_evaluation.evidence import load_package, read_json
from tools.support_evaluation.export import json_bytes
from tools.support_evaluation.score import ZERO_USAGE, check_calls, evaluate_case
from tools.support_evaluation.validate_data import instant
from tools.support_recovery.plan import validate


def sum_usage(rows: list[dict[str, Any]]) -> dict[str, int] | None:
    """Missing ledger evidence remains unknown, never a zero-cost row."""
    if any(row["ledger_usage"] is None for row in rows):
        return None
    return {key: sum(row["ledger_usage"][key] for row in rows) for key in ZERO_USAGE}


def group_calls(
    actual: dict[str, Any] | None, fault: dict[str, Any] | None
) -> dict[str, list[str]]:
    """Include every subcall from the actual committed step/attempt."""
    groups: dict[str, list[str]] = {
        key: []
        for key in (
            "committed_prefix",
            "uncommitted_before_fault",
            "after_fault",
            "no_fault_execution",
        )
    }
    if actual is None:
        return groups
    calls = actual["calls"]["items"]
    by_id = {call["physical_call_id"]: call for call in calls}
    prefix = (
        0
        if fault is None
        else fault["sequence"]
        if fault["phase"] == "commit_ack_lost"
        else fault["cursor_version"]
    )
    committed = set()
    for entry in actual["steps"]:
        step = entry["record"]
        selected = by_id.get(step["output"].get("physical_call_id"))
        if step["sequence"] <= prefix and selected is not None:
            committed.add((step["step_id"], selected["attempt_no"]))
    for call in calls:
        group = (
            "no_fault_execution"
            if fault is None
            else "committed_prefix"
            if (call["step_id"], call["attempt_no"]) in committed
            else "after_fault"
            if instant(call["reserved_at"]) >= instant(fault["observed_at"])
            else "uncommitted_before_fault"
        )
        groups[group].append(call["physical_call_id"])
    return groups


def pair(rows: list[dict[str, Any]]) -> dict[str, Any]:
    """Include original H0 and restarted H1 exposure before comparing with C."""
    by_arm = {row["arm"]: row for row in rows}
    checkpoint = sum_usage([by_arm["C"]])
    restart = sum_usage([by_arm["H0"], by_arm["H1"]])
    comparable = bool(
        by_arm["C"]["fault"]
        and by_arm["H0"]["fault"]
        and by_arm["C"]["fault"]["phase"] == by_arm["H0"]["fault"]["phase"]
        and by_arm["C"]["prefix_actions"] == by_arm["H0"]["prefix_actions"]
        and by_arm["C"]["version_vector"]
        == by_arm["H0"]["version_vector"]
        == by_arm["H1"]["version_vector"]
        and all(by_arm[arm]["status"] == "finished" for arm in ("C", "H0", "H1"))
        and all(by_arm[arm]["score"]["correct"] for arm in ("C", "H1"))
        and all(by_arm[arm]["score"]["safety"] == "passed" for arm in ("C", "H0", "H1"))
    )
    return {
        "case_id": rows[0]["case_id"],
        "comparable": comparable,
        "checkpoint_total": checkpoint,
        "restart_total_including_H0": restart,
        "checkpoint_minus_restart_known_microyuan": (
            checkpoint["known_cost_microyuan"] - restart["known_cost_microyuan"]
            if checkpoint is not None and restart is not None
            else None
        ),
        "exposure_settled": checkpoint is not None
        and restart is not None
        and checkpoint["held_cost_microyuan"] == restart["held_cost_microyuan"] == 0
        and checkpoint["unknown_chat_calls"] == restart["unknown_chat_calls"] == 0,
        "note": "small_development_sample_no_population_savings_claim",
    }


def timing_facts(
    row: dict[str, Any], actual: dict[str, Any] | None, control: dict[str, Any] | None
) -> dict[str, Any]:
    """Separate signal/cleanup, sampled lease, persisted closure and claim times."""
    fault = row.get("fault") or {}
    sampled = row.get("pre_fault_run") or {}
    attempts = [] if control is None else control["attempts"]
    original = next(
        (a for a in attempts if a["attempt_no"] == fault.get("attempt_no")), None
    )
    replacement = next(
        (a for a in attempts if a["attempt_no"] > fault.get("attempt_no", 1)), None
    )
    persisted_steps = [] if control is None else control["steps"]
    first = next(
        (
            s
            for s in persisted_steps
            if replacement is not None and s["attempt_no"] == replacement["attempt_no"]
        ),
        None,
    )
    run = {} if actual is None else actual["run"]
    terminal = (
        run.get("updated_at")
        if run.get("state") in {"awaiting_approval", "succeeded", "failed", "cancelled"}
        else None
    )
    facts: dict[str, Any] = {
        "kill_sent_at": fault.get("kill_sent_at"),
        "worker_wait_completed_at": fault.get("worker_wait_completed_at"),
        "group_gone_confirmed_at": fault.get("group_gone_confirmed_at"),
        "child_group_at_boundary": fault.get("child_group_at_boundary"),
        "lease_sampled_at": sampled.get("sampled_at"),
        "sampled_lease_until": sampled.get("lease_until"),
        "original_closed_at": None if original is None else original["finished_at"],
        "replacement_claimed_at": None
        if replacement is None
        else replacement["started_at"],
        "first_new_step_at": None if first is None else first["created_at"],
        "terminal_at": terminal,
        "run_created_at": run.get("created_at"),
        **{
            key: row.get(key)
            for key in ("ready_at", "replacement_started_at", "finished_at")
        },
    }
    for name, start, end in (
        ("kill_to_close_seconds", "kill_sent_at", "original_closed_at"),
        ("closed_to_claim_seconds", "original_closed_at", "replacement_claimed_at"),
        ("kill_to_claim_seconds", "kill_sent_at", "replacement_claimed_at"),
        ("claim_to_first_step_seconds", "replacement_claimed_at", "first_new_step_at"),
        ("active_after_claim_seconds", "replacement_claimed_at", "terminal_at"),
        ("fault_to_terminal_seconds", "kill_sent_at", "terminal_at"),
        ("total_run_seconds", "run_created_at", "terminal_at"),
    ):
        facts[name] = (
            None
            if facts[start] is None or facts[end] is None
            else (instant(facts[end]) - instant(facts[start])).total_seconds()
        )
    return facts


def report(
    plan_path: Path,
    archive: Path,
    metadata: Path,
    before_path: Path,
    after_path: Path,
    control_path: Path,
    continuation_path: Path | None = None,
) -> dict[str, Any]:
    """Reuse existing source/policy/safety scoring; keep every original intention."""
    plan, plan_hash = read_json(plan_path, 2 << 20)
    if continuation_path is None:
        validate(plan)
    else:
        from tools.support_recovery.continuation import verify_reporting

        verify_reporting(continuation_path, plan_path, archive)
    actual_rows, _ = read_json(archive / "rows.json", 2 << 20)
    if len(actual_rows["runs"]) != 11:
        raise ValueError("S3_ROW_COVERAGE")
    package = load_package()
    grouped, trace_hash, metadata_valid = outbound(metadata)
    before, _ = read_json(before_path, 2 << 20)
    after, _ = read_json(after_path, 2 << 20)
    control, control_hash = read_json(control_path, 2 << 20)
    if (
        control["schema_version"] != 1
        or control["batch_account_id"] != plan["batch_account_id"]
    ):
        raise ValueError("S3_CONTROL_AUDIT_BINDING_MISMATCH")
    control_runs = {row["run_id"]: row for row in control["runs"]}
    if len(control_runs) != len(control["runs"]):
        raise ValueError("S3_CONTROL_AUDIT_DUPLICATE_RUN")
    audit_hash = sha(
        json_bytes(
            {
                "before": sha(before_path.read_bytes()),
                "after": sha(after_path.read_bytes()),
            }
        )
    )
    snapshots, physical_ids, run_ids = set(), set(), set()
    result_rows = []
    for frozen, row in zip(plan["runs"], actual_rows["runs"], strict=True):
        if any(row[key] != frozen[key] for key in frozen):
            raise ValueError("S3_ROW_BINDING_MISMATCH")
        score_row: dict[str, Any] = {
            "case_id": row["case_id"],
            "status": "unattempted",
            "error_code": row["error_code"],
            "run": None,
            "steps": [],
            "result": None,
            "calls": None,
            "safety": None,
        }
        actual: dict[str, Any] | None = None
        ledger_usage = None
        ledger_error = ""
        if row["run_id"] is not None:
            if row["run_id"] in run_ids:
                raise ValueError("S3_DUPLICATE_RUN")
            run_ids.add(row["run_id"])
            directory = archive / row["experiment"]
            evidence_file = directory / "evidence.json"
            if not evidence_file.is_file():
                evidence_file = directory / "failure-evidence.json"
            if evidence_file.is_file():
                actual, _ = read_json(evidence_file, 8 << 20)
                if actual["run"]["run_id"] != row["run_id"]:
                    raise ValueError("S3_EXPORT_RUN_MISMATCH")
                snapshot = actual["run"]["snapshot_id"]
                snapshots.add(snapshot)
                score_row.update(actual, status="run")
                score_row["safety"] = safety(
                    grouped.get(snapshot, []),
                    trace_hash,
                    before,
                    after,
                    audit_hash,
                    metadata_valid,
                )
                for call in actual["calls"]["items"]:
                    if call["physical_call_id"] in physical_ids:
                        raise ValueError("S3_DUPLICATE_PHYSICAL_CALL")
                    physical_ids.add(call["physical_call_id"])
                try:
                    ledger_usage = check_calls(score_row, plan["profile"])
                except (ValueError, KeyError, TypeError):
                    ledger_error = "CALL_EVIDENCE_UNVERIFIED"
            else:
                score_row.update(
                    status="submission_failed", error_code="ACCEPTED_EXPORT_MISSING"
                )
                ledger_error = "ACCEPTED_EXPORT_MISSING"
        elif row["status"] != "unattempted":
            score_row.update(
                status="submission_failed", error_code="ACCEPTANCE_UNKNOWN"
            )
            ledger_error = "ACCEPTANCE_UNKNOWN"
        else:
            ledger_usage = dict(ZERO_USAGE)
        control_run = control_runs.get(row["run_id"])
        if control_run is not None and any(
            control_run[key] != expected
            for key, expected in {
                "tenant_id": frozen["binding"]["tenant_id"],
                "profile_id": plan["profile"]["profile_id"],
                "profile_hash": plan["profile"]["profile_hash"],
            }.items()
        ):
            raise ValueError("S3_CONTROL_RUN_BINDING_MISMATCH")
        fault = row.get("fault")
        prefix = (
            0
            if fault is None
            else fault["sequence"]
            if fault["phase"] == "commit_ack_lost"
            else fault["cursor_version"]
        )
        steps = [] if actual is None else actual["steps"]
        actions = [
            {
                "kind": entry["record"]["kind"],
                "decision": entry["record"]["output"].get("content")
                if entry["record"]["kind"] == "model_decision"
                else None,
            }
            for entry in steps
            if entry["record"]["sequence"] <= prefix
        ]
        result_rows.append(
            {
                "experiment": row["experiment"],
                "case_id": row["case_id"],
                "arm": row["arm"],
                "status": row["status"],
                "error_code": row["error_code"],
                "run_id": row["run_id"],
                "fault": fault,
                "prefix_actions": actions,
                "version_vector": None
                if actual is None
                else actual["run"]["version_vector"],
                "recovery_count": None
                if actual is None
                else actual["run"]["recovery_count"],
                "ledger_usage": ledger_usage,
                "ledger_error": ledger_error,
                "call_groups": group_calls(actual, fault),
                "timing": timing_facts(row, actual, control_run),
                "score": evaluate_case(
                    score_row, frozen["binding"], plan["profile"], package
                ),
            }
        )
    if set(grouped) - snapshots:
        for row in result_rows:
            if row["score"]["safety"] == "passed":
                row["score"]["safety"] = "unverified"
                row["score"]["errors"].append("UNBOUND_OUTBOUND_SNAPSHOT")
    return {
        "schema_version": 1,
        "kind": "support-s3-eleven-run-experiment",
        "execution_list_sha256": plan_hash,
        "control_audit_sha256": control_hash,
        "unbound_control_run_ids": sorted(set(control_runs) - run_ids),
        "denominator": 11,
        "outcomes": dict(Counter(row["status"] for row in result_rows)),
        "usage": sum_usage(result_rows),
        "usage_complete": all(row["ledger_usage"] is not None for row in result_rows),
        "pairs": [
            pair(
                [
                    row
                    for row in result_rows
                    if row["case_id"] == case and row["arm"] in {"C", "H0", "H1"}
                ]
            )
            for case in ("DEV-002", "DEV-027", "DEV-035")
        ],
        "rows": result_rows,
        "cost_basis": "declared_tariff_not_provider_invoice",
        "execution_authenticity": "requires_reviewed_build_process_PG_export_and_outbound_chain",
    }


def main() -> None:
    """Write a fresh private report; no reruns or automatic normalization."""
    parser = argparse.ArgumentParser(description=__doc__)
    for name in (
        "plan",
        "archive",
        "outbound",
        "before",
        "after",
        "control-audit",
        "out",
    ):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--continuation", type=Path)
    args = parser.parse_args()
    save_new(
        args.out,
        report(
            args.plan,
            args.archive,
            args.outbound,
            args.before,
            args.after,
            args.control_audit,
            args.continuation,
        ),
    )


if __name__ == "__main__":
    main()
