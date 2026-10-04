"""Explicit continuation of only the four never-submitted S3 intentions."""

from __future__ import annotations

import argparse
import copy
import hashlib
import json
import shutil
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from jobforge import RunClient

from tools.support_evaluation.assemble import save_new
from tools.support_evaluation.evidence import load_package
from tools.support_evaluation.export import CaptureTransport, atomic_json, export_run
from tools.support_recovery.plan import source_hashes, validate

KIND = "support-s3-interrupted-continuation-v1"
ORDINALS = [8, 9, 10, 11]
CHANGED = {
    "tools/support_recovery/driver.py",
    "tools/support_recovery/continuation.py",
    "tools/support_recovery/report.py",
    "deploy/Dockerfile.support-recovery",
}
ORIGINAL_FILES = (
    "execution-list.json",
    "release.json",
    "build-source-receipt.json",
    "preflight.json",
    "report.json",
    "setup-after.json",
    "control-audit-after.json",
    "experiment-exit.json",
    "experiment-container.json",
    "business-before.json",
    "business-after.json",
    "stop-diagnosis.json",
)
CLOSED = {"awaiting_approval", "succeeded", "failed", "cancelled"}


def digest(path: Path) -> str:
    """Hash exact bytes without exposing private response content."""
    return hashlib.sha256(path.read_bytes()).hexdigest()


def read(path: Path) -> dict[str, Any]:
    """Read a private JSON receipt, never interpreting it as an instruction."""
    value: dict[str, Any] = json.loads(path.read_bytes())
    return value


def original_files(root: Path) -> dict[str, str]:
    """Keep every original SDK/outbound response and failure receipt immutable."""
    paths = [root / name for name in ORIGINAL_FILES]
    for name in ("exports/run-01", "outbound"):
        paths.extend(path for path in (root / name).rglob("*") if path.is_file())
    if any(path.is_symlink() for path in paths):
        raise ValueError("S3_ORIGINAL_SYMLINK")
    return {path.relative_to(root).as_posix(): digest(path) for path in sorted(paths)}


def checked_rows(plan: dict[str, Any], root: Path) -> list[dict[str, Any]]:
    """Reject Submit uncertainty or any deviation from the original eleven rows."""
    rows: list[dict[str, Any]] = read(root / "exports/run-01/rows.json")["runs"]
    if len(rows) != 11:
        raise ValueError("S3_CONTINUATION_ROWS")
    for index, (frozen, actual) in enumerate(zip(plan["runs"], rows, strict=True)):
        if any(actual[key] != value for key, value in frozen.items()):
            raise ValueError("S3_CONTINUATION_BINDING")
        if index < 7:
            if actual["status"] != "finished" or not actual["run_id"]:
                raise ValueError("S3_PRIOR_SUBMIT_UNRESOLVED")
        elif actual != dict(frozen, status="unattempted", run_id=None, error_code=""):
            raise ValueError("S3_REMAINING_ALREADY_ATTEMPTED")
    if len({row["run_id"] for row in rows[:7]}) != 7:
        raise ValueError("S3_DUPLICATE_PRIOR_RUN")
    return rows


def closed_history(
    plan: dict[str, Any],
    rows: list[dict[str, Any]],
    audit: dict[str, Any],
    evidence: dict[str, dict[str, Any]],
) -> None:
    """Check actual PG step attempts against complete, already committed calls."""
    from tools.support_recovery.driver import chat_barrier

    if audit["batch_account_id"] != plan["batch_account_id"]:
        raise ValueError("S3_CONTINUATION_AUDIT_BATCH")
    by_id = {run["run_id"]: run for run in audit["runs"]}
    if len(by_id) != 7 or set(by_id) != {row["run_id"] for row in rows[:7]}:
        raise ValueError("S3_CONTINUATION_HISTORY")
    for row in rows[:7]:
        run = by_id[row["run_id"]]
        exported = evidence[row["experiment"]]
        current = exported["run"]
        if (
            run["state"] not in CLOSED
            or current["state"] != run["state"]
            or current["run_id"] != row["run_id"]
            or any(
                current[key] != expected
                for key, expected in {
                    "tenant_id": row["binding"]["tenant_id"],
                    "ticket_id": row["binding"]["ticket_id"],
                    "business_request_key": row["binding"]["business_request_key"],
                    "profile_id": plan["profile"]["profile_id"],
                    "profile_hash": plan["profile"]["profile_hash"],
                    "budget_batch_id": plan["batch_key"],
                }.items()
            )
            or any(
                run[key] != current[key]
                for key in ("tenant_id", "profile_id", "profile_hash")
            )
            or any(attempt["finished_at"] is None for attempt in run["attempts"])
            or not chat_barrier(exported, committed=True)
        ):
            raise ValueError("S3_PRIOR_RUN_NOT_CLOSED")
        pg_steps = {step["step_id"]: step for step in run["steps"]}
        sdk_steps = {
            entry["record"]["step_id"]: entry["record"] for entry in exported["steps"]
        }
        if len(pg_steps) != len(sdk_steps) or set(pg_steps) != set(sdk_steps):
            raise ValueError("S3_PRIOR_CHECKPOINT_MISMATCH")
        for identity, step in pg_steps.items():
            if any(
                step[key] != sdk_steps[identity][key] for key in ("kind", "sequence")
            ):
                raise ValueError("S3_PRIOR_CHECKPOINT_MISMATCH")
        for call in exported["calls"]["items"]:
            if (
                call["observed_at"] is None
                or call["held_tokens"]
                or call["held_cost_microyuan"]
                or call["measurement_anomaly"]
                or call["report_conflict"]
            ):
                raise ValueError("S3_PRIOR_CALL_ACTIVE")
            if call["subcall"] == "chat" and (
                call["step_id"] not in pg_steps
                or pg_steps[call["step_id"]]["attempt_no"] != call["attempt_no"]
            ):
                raise ValueError("S3_PRIOR_CALL_ATTEMPT_MISMATCH")


def setup_valid(setup: dict[str, Any], plan: dict[str, Any]) -> None:
    """Keep the original consumed accounts and shared five-yuan cap intact."""
    if (
        not setup["matches_config"]
        or not setup["migrations_ready"]
        or setup["profile"]["profile_id"] != plan["profile"]["profile_id"]
        or setup["profile"]["profile_hash"] != plan["profile"]["profile_hash"]
        or setup["history"]["runs"] != 7
        or setup["history"]["business_requests"] != 7
        or len(setup["accounts"]) != 3
    ):
        raise ValueError("S3_CONTINUATION_SETUP")
    for account in setup["accounts"]:
        if (
            account["frozen"]
            or account["batch_stop_code"]
            or account["held_tokens"]
            or account["held_cost_microyuan"]
            or account["used"]["cost_microyuan"] != account["known_cost_microyuan"]
            or account["used"]["tokens"] != account["known_tokens"]
            or account["limits"]["cost_microyuan"] > plan["max_cost_microyuan"]
            or account["known_cost_microyuan"] > account["limits"]["cost_microyuan"]
        ):
            raise ValueError("S3_CONTINUATION_EXPOSURE_UNRESOLVED")
    batch = [a for a in setup["accounts"] if a["scope"] == "batch"]
    if len(batch) != 1 or batch[0]["account_id"] != plan["batch_account_id"]:
        raise ValueError("S3_CONTINUATION_BATCH")


def checked_sources(plan: dict[str, Any]) -> tuple[dict[str, str], dict[str, Any]]:
    """Permit only the reviewed experiment-tool delta, never production edits."""
    sources = source_hashes()
    changes = {
        path: {"before": plan["sources"].get(path), "after": sources.get(path)}
        for path in sorted(set(plan["sources"]) | set(sources))
        if plan["sources"].get(path) != sources.get(path)
    }
    if not set(changes) <= CHANGED or plan["package_sha256"] != load_package().hashes:
        raise ValueError("S3_CONTINUATION_SOURCE_SCOPE")
    return sources, changes


def waiting_expiry(previous: dict[str, Any], current: dict[str, Any]) -> bool:
    """Allow only the existing waiting-state Run deadline transition."""
    before, after = copy.deepcopy(previous), copy.deepcopy(current)
    before.pop("budget", None)
    after.pop("budget", None)
    if before == after:
        return True
    try:
        deadline = datetime.fromisoformat(before["run_deadline"])
        updated = datetime.fromisoformat(after["updated_at"])
        if (
            before["state"] != "awaiting_approval"
            or after["state"] != "failed"
            or after["error"]
            != {"code": "RUN_DEADLINE_EXCEEDED", "message": "RUN_DEADLINE_EXCEEDED"}
            or updated < deadline
            or updated < datetime.fromisoformat(before["updated_at"])
        ):
            return False
        expected = dict(
            before, state="failed", error=after["error"], updated_at=after["updated_at"]
        )
        return expected == after
    except (ValueError, KeyError, TypeError):
        return False


def expiry_snapshot(
    plan: dict[str, Any],
    rows: list[dict[str, Any]],
    root: Path,
    setup: dict[str, Any],
    original: dict[str, dict[str, Any]],
) -> dict[str, str]:
    """Bind separately observed natural expiry without rewriting execution results."""
    directory = root / "continuation/natural-expiry"
    observed_setup = read(directory / "setup.json")
    if any(
        observed_setup[key] != setup[key]
        for key in ("accounts", "bindings", "history", "profile")
    ):
        raise ValueError("S3_EXPIRY_LEDGER_CHANGED")
    audit = read(directory / "control-audit.json")
    prior_pg = {r["run_id"]: r for r in read(root / "control-audit-after.json")["runs"]}
    actual_pg = {r["run_id"]: r for r in audit["runs"]}
    exported = {
        row["experiment"]: read(directory / row["experiment"] / "evidence.json")
        for row in rows[:7]
    }
    closed_history(plan, rows, audit, exported)
    for row in rows[:7]:
        before, after = original[row["experiment"]], exported[row["experiment"]]
        pg_before, pg_after = prior_pg[row["run_id"]], actual_pg[row["run_id"]]
        expected_pg = dict(
            pg_before, state=pg_after["state"], updated_at=pg_after["updated_at"]
        )
        if (
            not waiting_expiry(before["run"], after["run"])
            or pg_after != expected_pg
            or datetime.fromisoformat(pg_after["updated_at"])
            != datetime.fromisoformat(after["run"]["updated_at"])
            or before["steps"] != after["steps"]
            or before["result"] != after["result"]
            or before["calls"]["items"] != after["calls"]["items"]
        ):
            raise ValueError("S3_EXPIRY_SNAPSHOT_CHANGED")
        old_budget, new_budget = before["run"]["budget"], after["run"]["budget"]
        if set(old_budget) != set(new_budget) or any(
            old_budget[key] != new_budget[key] for key in ("family", "run_usage")
        ):
            raise ValueError("S3_EXPIRY_LEDGER_CHANGED")
        for scope in ("tenant", "batch"):
            account = next(
                a
                for a in setup["accounts"]
                if a["account_id"] == old_budget[scope]["id"]
            )
            expected_account = {
                key: account["account_id"] if key == "id" else account[key]
                for key in old_budget[scope]
            }
            if new_budget[scope] != expected_account:
                raise ValueError("S3_EXPIRY_LEDGER_CHANGED")
        previous_events = read(
            root / "exports/run-01" / row["experiment"] / "events.json"
        )["events"]
        events = read(directory / row["experiment"] / "events.json")["events"]
        expected_events = list(previous_events)
        if before["run"]["state"] != after["run"]["state"]:
            expected_events.append(
                {
                    "sequence": previous_events[-1]["sequence"] + 1,
                    "event_type": "state_changed",
                    "state": "failed",
                    "attempt_no": before["run"]["attempt_no"],
                    "cursor_version": before["run"]["cursor_version"],
                    "created_at": after["run"]["updated_at"],
                }
            )
        if events != expected_events:
            raise ValueError("S3_EXPIRY_EVENT_CHANGED")
    return {
        path.relative_to(root).as_posix(): digest(path)
        for path in sorted(directory.rglob("*"))
        if path.is_file()
    }


def prepare(plan: dict[str, Any], root: Path) -> dict[str, Any]:
    """Freeze a narrowly reviewed tool delta; never produce launch approval."""
    validate(plan, check_sources=False)
    rows = checked_rows(plan, root)
    setup = read(root / "setup-after.json")
    setup_valid(setup, plan)
    evidence = {
        row["experiment"]: read(
            root / "exports/run-01" / row["experiment"] / "evidence.json"
        )
        for row in rows[:7]
    }
    closed_history(plan, rows, read(root / "control-audit-after.json"), evidence)
    expired = expiry_snapshot(plan, rows, root, setup, evidence)
    sources, changes = checked_sources(plan)
    batch = next(a for a in setup["accounts"] if a["scope"] == "batch")
    calls = [
        call for exported in evidence.values() for call in exported["calls"]["items"]
    ]
    if (
        len(calls) != setup["history"]["calls"]
        or sum(call["known_cost_microyuan"] for call in calls)
        != batch["known_cost_microyuan"]
        or sum(call["known_tokens"] for call in calls) != batch["known_tokens"]
    ):
        raise ValueError("S3_PRIOR_LEDGER_MISMATCH")
    return {
        "schema_version": 1,
        "kind": KIND,
        "remaining_ordinals": ORDINALS,
        "execution_list_sha256": digest(root / "execution-list.json"),
        "original_artifacts": original_files(root),
        "natural_expiry_artifacts": expired,
        "reviewed_sources": sources,
        "source_changes": changes,
        "prior_run_ids": [row["run_id"] for row in rows[:7]],
        "known_cost_microyuan_before": batch["known_cost_microyuan"],
        "additional_cost_microyuan_max": plan["max_cost_microyuan"]
        - batch["known_cost_microyuan"],
        "original_errors_preserved": {
            row["experiment"]: row["error_code"] for row in rows[:7]
        },
        "approved": False,
    }


def verify(
    manifest: dict[str, Any],
    plan: dict[str, Any],
    root: Path,
    *,
    check_sources: bool,
) -> None:
    """Reject changed originals, a different suffix or unreviewed source edits."""
    validate(plan, check_sources=False)
    if (
        manifest["schema_version"] != 1
        or manifest["kind"] != KIND
        or manifest["remaining_ordinals"] != ORDINALS
        or manifest["execution_list_sha256"] != digest(root / "execution-list.json")
    ):
        raise ValueError("S3_CONTINUATION_CHANGED")
    if check_sources:
        expected = prepare(plan, root)
        # A continuation may append new outbound files, but cannot replace any
        # original response. The released original file set stays fixed.
        expected["original_artifacts"] = manifest["original_artifacts"]
        if manifest != expected:
            raise ValueError("S3_CONTINUATION_CHANGED")
    artifacts = dict(
        manifest["original_artifacts"], **manifest.get("natural_expiry_artifacts", {})
    )
    for name, expected in artifacts.items():
        path = root / name
        if (
            path.is_symlink()
            or not path.resolve().is_relative_to(root.resolve())
            or digest(path) != expected
        ):
            raise ValueError("S3_ORIGINAL_EVIDENCE_CHANGED")


def preflight(
    manifest: dict[str, Any],
    plan: dict[str, Any],
    settings: dict[str, Any],
    setup: dict[str, Any],
    output: Path,
) -> list[dict[str, Any]]:
    """Read the seven prior Runs before starting any Worker or making any POST."""
    root = Path(settings["original_root"])
    verify(manifest, plan, root, check_sources=False)
    original_setup = read(root / "setup-after.json")
    setup_valid(setup, plan)
    if any(
        setup[key] != original_setup[key]
        for key in ("accounts", "bindings", "history", "profile")
    ):
        raise ValueError("S3_CONTINUATION_LEDGER_CHANGED")
    audit = read(Path(settings["continuation_control_audit"]))
    age = (
        datetime.now(UTC) - datetime.fromisoformat(audit["sampled_at"])
    ).total_seconds()
    if (
        not 0 <= age <= 120
        or audit["runs"]
        != read(root / "continuation/natural-expiry/control-audit.json")["runs"]
    ):
        raise ValueError("S3_CONTINUATION_PG_CHANGED")
    rows = checked_rows(plan, root)
    current_evidence = {}
    for row in rows[:7]:
        archive = output / "preflight" / row["experiment"]
        archive.mkdir(mode=0o700, parents=True)
        capture = CaptureTransport(archive)
        with RunClient(
            settings["control_origin"],
            settings["driver_tokens"][row["binding"]["tenant_id"]],
            timeout=5,
            transport=capture,
        ) as client:
            exported, events = export_run(client, capture, row["run_id"])
        atomic_json(archive / "evidence.json", exported)
        atomic_json(archive / "events.json", {"events": events})
        previous = read(
            root / "continuation/natural-expiry" / row["experiment"] / "evidence.json"
        )
        if (
            exported["steps"] != previous["steps"]
            or exported["calls"]["items"] != previous["calls"]["items"]
            or exported["result"] != previous["result"]
            or exported["run"] != previous["run"]
            or events
            != read(
                root / "continuation/natural-expiry" / row["experiment"] / "events.json"
            )["events"]
        ):
            raise ValueError("S3_PRIOR_SDK_EVIDENCE_CHANGED")
        current_evidence[row["experiment"]] = exported
    closed_history(plan, rows, audit, current_evidence)
    atomic_json(
        output / "continuation-preflight.json",
        {
            "kind": KIND,
            "remaining_ordinals": ORDINALS,
            "execution_list_sha256": manifest["execution_list_sha256"],
            "control_audit_sha256": digest(
                Path(settings["continuation_control_audit"])
            ),
            "known_cost_microyuan_before": manifest["known_cost_microyuan_before"],
            "seven_closed_runs_verified": True,
        },
    )
    return copy.deepcopy(rows)


def merge(manifest: dict[str, Any], root: Path, suffix: Path, output: Path) -> None:
    """Copy into a fresh reporting archive; keep original seven rows verbatim."""
    plan = read(root / "execution-list.json")
    verify(manifest, plan, root, check_sources=True)
    rows = checked_rows(plan, root)
    current = read(suffix / "rows.json")["runs"]
    if len(current) != 11 or current[:7] != rows[:7]:
        raise ValueError("S3_PRESERVED_ROWS_CHANGED")
    for frozen, row in zip(plan["runs"][7:], current[7:], strict=True):
        if any(row[key] != value for key, value in frozen.items()):
            raise ValueError("S3_SUFFIX_BINDING_CHANGED")
    output.mkdir(mode=0o700)
    for index, row in enumerate(current):
        source = (root / "exports/run-01" if index < 7 else suffix) / row["experiment"]
        if source.exists():
            shutil.copytree(source, output / row["experiment"])
    atomic_json(output / "rows.json", {"schema_version": 1, "runs": current})
    atomic_json(
        output / "continuation-origin.json",
        {
            "original_artifacts": manifest["original_artifacts"],
            "original_seven_rows_verbatim": True,
        },
    )


def verify_reporting(manifest_path: Path, plan_path: Path, archive: Path) -> None:
    """Require byte-identical prior exports in the new combined report archive."""
    manifest, root = read(manifest_path), plan_path.parent
    plan = read(plan_path)
    verify(manifest, plan, root, check_sources=True)
    rows = checked_rows(plan, root)
    if read(archive / "rows.json")["runs"][:7] != rows[:7]:
        raise ValueError("S3_PRESERVED_ROWS_CHANGED")
    prefix = "exports/run-01/"
    experiments = {row["experiment"] for row in rows[:7]}
    for name, expected in manifest["original_artifacts"].items():
        if name.startswith(prefix) and name[len(prefix) :].split("/")[0] in experiments:
            if digest(archive / name[len(prefix) :]) != expected:
                raise ValueError("S3_REPORT_ORIGINAL_CHANGED")


def main() -> None:
    """Prepare a reviewable continuation without Submit, Worker or provider calls."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--original-root", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    save_new(
        args.out,
        prepare(read(args.original_root / "execution-list.json"), args.original_root),
    )


if __name__ == "__main__":
    main()
