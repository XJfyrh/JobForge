"""Reuse the existing preparation, SDK, exports and scorer for one reviewed batch."""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import time
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from tools.support_evaluation.assemble import assemble, read, register, save_new, sha
from tools.support_evaluation.export import atomic_json
from tools.support_evaluation.launcher import validate_setup
from tools.support_evaluation.score import usage_cost
from tools.support_s5.freeze import ROOT
from tools.support_s5.host import ledger
from tools.support_s5.host.config import (
    SEEN_V3_GUARDS,
    command,
    digest,
    evaluation_files,
    evaluation_package,
    load,
    validate_review,
    validate_sources,
    validate_worker_scope,
    verify_bound,
)
from tools.support_s5.host.health import (
    money_reasons,
    quality_gate_applies,
    stop_reasons,
)
from tools.support_s5.score import score_export


def mount(path: Path, target: str, *, writable: bool = False) -> list[str]:
    """Only explicit prepared inputs and evidence directories enter the operator."""
    return [
        "--mount",
        f"type=bind,source={path.as_posix()},target={target}"
        + ("" if writable else ",readonly"),
    ]


def commands(config: dict[str, Any], batch: Path) -> dict[str, list[str]]:
    """Generate fixed Docker commands; configuration never provides executable code."""
    image = config["images"]["s5_operator"]
    prefix = config["container_prefix"]
    secrets = Path(config["secrets"])
    common = ["docker", "run", "--init", "--network", config["network"]]
    control = common + ["--env-file", str(secrets / "control.env")]
    control += mount(batch / "prepared", "/etc/jobforge/cloud")
    disabled = control + [
        "--rm",
        "-e",
        "JOBFORGE_AGENT_CONFIG=/etc/jobforge/cloud/control.disabled.json",
        "--entrypoint",
        "/usr/local/bin/agent-control",
        image,
    ]
    prepare = (
        control
        + ["--rm"]
        + mount(batch / "state", "/var/lib/jobforge/batches", writable=True)
    )
    prepare += [
        "--entrypoint",
        "python",
        image,
        "-m",
        "tools.support_s5.launcher",
        "prepare-state",
    ]
    serve = control + [
        "-d",
        "--name",
        prefix + "-control",
        "--network-alias",
        "control",
        "-e",
        "JOBFORGE_AGENT_CONFIG=/etc/jobforge/cloud/control.enabled.json",
        "--entrypoint",
        "/usr/local/bin/agent-control",
        image,
        "serve",
    ]
    business = common + [
        "-d",
        "--name",
        prefix + "-business",
        "--network-alias",
        "business",
        "--env-file",
        str(secrets / "business.env"),
        config["images"]["business"],
        "serve",
    ]
    launch = common + [
        "--name",
        prefix + "-paid",
        "--env-file",
        str(secrets / "control.env"),
    ]
    for key, value in {
        "JOBFORGE_AGENT_CONFIG": "/etc/jobforge/cloud/control.enabled.json",
        "JOBFORGE_AGENT_WORKER_CONFIG": "/etc/jobforge/cloud/worker.json",
        "JOBFORGE_AGENT_WORKER_CREDENTIALS_FILE": "/run/secrets/worker.json",
        "JOBFORGE_AGENT_GATEWAY": "control:9093",
        "JOBFORGE_AGENT_GRPC_TLS": "false",
    }.items():
        launch += ["-e", key + "=" + value]
    for path, target, writable in (
        (batch / "guard", "/var/lib/jobforge/guard", True),
        (batch / "prepared", "/etc/jobforge/cloud", False),
        (batch / "prepared/executor.json", "/etc/jobforge/executor.json", False),
        (secrets / "worker.json", "/run/secrets/worker.json", False),
        (secrets / "driver.json", "/run/secrets/driver.json", False),
        (batch / "state", "/var/lib/jobforge/batches", True),
        (batch / "exports", "/var/lib/jobforge/exports", True),
        (batch / "outbound", "/var/lib/jobforge/outbound", True),
    ):
        launch += mount(path, target, writable=writable)
    launch += [
        "--read-only",
        "--tmpfs",
        "/tmp:size=16m",
        "--cap-drop",
        "ALL",
        "--security-opt",
        "no-new-privileges:true",
        "--pids-limit",
        "96",
        "--memory",
        "512m",
        "--cpus",
        "2",
        image,
    ]
    return {
        "bootstrap": disabled + ["bootstrap"],
        "inspect": disabled + ["inspect-support"],
        "prepare_state": prepare,
        "business": business,
        "control": serve,
        "launch": launch,
    }


def prepare(config_path: Path, batch: Path) -> dict[str, Any]:
    """Create a fresh disabled batch and a reviewable manifest; never start a Worker."""
    config_path, batch = config_path.resolve(), batch.resolve()
    if batch.is_relative_to(ROOT):
        raise ValueError("EVIDENCE_OUTSIDE_REPOSITORY")
    config = load(config_path)
    build = validate_sources(config)
    batch.mkdir(parents=True, exist_ok=False)
    for name in ("state", "exports", "outbound", "guard", "checkpoints"):
        (batch / name).mkdir()
    args = [
        "go",
        "run",
        "./cmd/agent-control",
        "prepare-support",
        "--repo",
        str(ROOT),
        "--source",
        config["source"],
        "--out",
        str(batch / "prepared"),
    ]
    for name, value in config["identity"].items():
        if name not in {
            "profile_id",
            "worker_id",
            "batch_id",
            "batch_key",
            "north_account_id",
            "south_account_id",
            "valid_from",
        }:
            raise ValueError("PREPARATION_IDENTITY")
        args += ["--" + name.replace("_", "-"), str(value)]
    args += ["--batch-cost-microyuan", str(config["policy"]["batch_cap_microyuan"])]
    command(args)
    prepared = batch / "prepared"
    launch = read(prepared / "launch.json")
    if [c["case_id"] for c in launch["cases"]] != config["case_ids"] or read(
        prepared / "control.disabled.json"
    )["enabled_profiles"]:
        raise ValueError("PREPARED_SCOPE")
    save_new(
        prepared / "health-gate.json",
        {"policy": "startup-review-v1", "human_pause_ordinals": []},
    )
    launch["config_sha256"]["health-gate.json"] = digest(prepared / "health-gate.json")
    atomic_json(prepared / "launch.json", launch)
    registration = register(prepared, package=evaluation_package(config), s5=True)
    profile = registration["profile"]
    for name, output_tokens in (("investigation", 1024), ("final", 16384)):
        expected = usage_cost(
            profile["pricing"],
            {
                "input_tokens": profile["max_input_tokens"],
                "cached_input_tokens": 0,
                "output_tokens": output_tokens,
            },
        )
        if expected != config["policy"]["full_hold_microyuan"][name]:
            raise ValueError("FULL_RESERVATION_MISMATCH")
    save_new(batch / "registration.json", registration)
    validate_worker_scope(
        read(prepared / "worker.json"),
        read(Path(config["secrets"]) / "worker.json"),
    )
    generated = commands(config, batch)
    save_new(batch / "commands.json", generated)
    current = next(
        p for p in config["inventory"] if p["database"] == config["control_database"]
    )
    # createdb refuses an existing database. No drop/reset/resume operation exists.
    command(
        [
            "docker",
            "exec",
            current["container"],
            "createdb",
            "-U",
            current["user"],
            current["database"],
        ]
    )
    command(generated["bootstrap"])
    setup = json.loads(command(generated["inspect"]))
    validate_setup(setup, launch)
    save_new(batch / "setup-disabled.json", setup)
    command(generated["prepare_state"])
    accounts = ledger.collect(config["inventory"])
    ledger.validate_history(read(Path(config["prior_ledger"])), accounts)
    save_new(batch / "preparation-ledger.json", accounts)
    paths = [
        config_path,
        Path(config["source"]),
        Path(config["build_receipt"]),
        Path(config["prior_ledger"]),
    ]
    source = read(Path(config["source"]))
    paths += [
        Path(source[k]["path"])
        for k in ("data_review", "scoring_review", "price_snapshot")
    ]
    paths += [
        Path(config["secrets"]) / name
        for name in ("control.env", "business.env", "driver.json", "worker.json")
    ]
    paths += [p for p in prepared.rglob("*") if p.is_file()]
    paths += [
        batch / name
        for name in (
            "commands.json",
            "registration.json",
            "setup-disabled.json",
            "preparation-ledger.json",
        )
    ]
    paths += list((batch / "state").rglob("prepared.json"))
    paths += evaluation_files(config)
    plan = {
        "schema_version": 1,
        "workflow": "startup-review-v1",
        "paid_release": False,
        "question": config["question"],
        "hypothesis": config["hypothesis"],
        "config": str(config_path),
        "batch_root": str(batch),
        "case_ids": config["case_ids"],
        "policy": config["policy"],
        "source_commit": build["source_commit"],
        "images": config["images"],
        "profile_hash": launch["profile_hash"],
        "batch_account_id": launch["batch_account_id"],
        "model": source["definition"]["model"],
        "final_model": source["definition"]["final_model"],
        "engineering_status": "startup_review_pending",
        "model_quality_passed": False,
        "bound_files": {str(p.resolve()): digest(p) for p in paths},
    }
    if config["schema_version"] == 2:
        plan["scope"] = "seen_v3_regression_not_unseen_acceptance"
    save_new(batch / "manifest.json", plan)
    return plan


def audit(config: dict[str, Any], target: Path, phase: str) -> None:
    """Retain original read-only business and receipt audit bytes."""
    for name, role, sql in (
        (
            "business",
            "jobforge_business_reader",
            "tools/support_evaluation/business_audit.sql",
        ),
        (
            "receipts",
            "jobforge_business_receipt_reader",
            "tools/support_approval/actions_audit.sql",
        ),
    ):
        raw = command(
            [
                "docker",
                "exec",
                "-i",
                "-e",
                "PGPASSWORD=" + role,
                config["business_container"],
                "psql",
                "-h",
                "127.0.0.1",
                "-U",
                role,
                "-d",
                config["business_database"],
                "-X",
                "-q",
                "-At",
                "-v",
                "ON_ERROR_STOP=1",
            ],
            raw=(ROOT / sql).read_bytes(),
        )
        if json.loads(raw)["database"] != config["business_database"]:
            raise ValueError("BUSINESS_AUDIT_BINDING")
        with (target / f"{name}-{phase}.json").open("xb") as output:
            output.write(raw)


def archive(batch: Path) -> Path:
    """Require the sole SDK attempt, retaining every original list entry."""
    paths = list((batch / "exports").glob("*/rows.json"))
    if len(paths) != 1:
        raise ValueError("SINGLE_EXPORT_REQUIRED")
    return paths[0].parent


def snapshot(
    config: dict[str, Any], batch: Path, target: Path
) -> tuple[dict[str, Any], dict[str, Any]]:
    """Use the existing assembler and strict scorer without quality rule changes."""
    audit(config, target, "after")
    package = evaluation_package(config)
    evidence = assemble(
        batch / "registration.json",
        archive(batch),
        batch / "outbound",
        batch / "business-before.json",
        target / "business-after.json",
        package=package,
        s5=True,
    )
    before, after = (
        (batch / "receipts-before.json").read_bytes(),
        (target / "receipts-after.json").read_bytes(),
    )
    evidence["receipt_audit"] = {
        "before_raw": before.decode(),
        "after_raw": after.decode(),
        "before_sha256": sha(before),
        "after_sha256": sha(after),
    }
    save_new(target / "evidence.json", evidence)
    raw = (batch / "registration.json").read_bytes()
    score = score_export(
        json.loads(raw),
        evidence,
        sha(raw),
        package=package,
        seen_diagnostic=config["schema_version"] == 2,
    )
    save_new(target / "score.json", score)
    return evidence, score


def advance(config: dict[str, Any], batch: Path, prior: int) -> None:
    """Automatically release all healthy ordinals, including ten."""
    guard = batch / "guard"
    for ready_path in sorted(guard.glob("*-ready.json")):
        ready = read(ready_path)
        ordinal = ready["ordinal"]
        release = guard / f"{ordinal:02d}-continue.json"
        if release.exists():
            continue
        current = next(
            p
            for p in config["inventory"]
            if p["database"] == config["control_database"]
        )
        sample = ledger.query(current)
        if sample["inflight"] or sample["unfinished_attempts"]:
            return
        stage = ledger.collect(config["inventory"])
        others = [
            p for p in stage["phases"] if p["database"] != config["control_database"]
        ]
        if sum(p["known"] for p in others) != prior or any(
            p[k]
            for p in others
            for k in ("held", "unknown_chat_calls", "inflight", "unfinished_attempts")
        ):
            atomic_json(
                guard / "stop.json",
                {
                    "decision": "stop",
                    "reasons": ["STAGE_LEDGER_CHANGED"],
                    "ledger": stage,
                },
            )
            return
        launch = read(batch / "prepared/launch.json")
        original = archive(batch)
        rows = read(original / "rows.json")["cases"]
        if (
            not 1 <= ordinal <= len(rows)
            or sample["runs"] != ordinal
            or ready["profile_hash"] != launch["profile_hash"]
            or ready["batch_account_id"] != launch["batch_account_id"]
            or ready["launch_sha256"] != digest(batch / "prepared/launch.json")
            or ready["case_id"] != launch["cases"][ordinal - 1]["case_id"]
            or ready["run_id"] != rows[ordinal - 1]["run_id"]
            or ready["rows_sha256"] != digest(original / "rows.json")
            or ready["evidence_sha256"]
            != digest(original / ready["case_id"] / "evidence.json")
            or [r["case_id"] for r in rows] != config["case_ids"]
            or any(r["status"] != "finished" for r in rows[:ordinal])
            or any(r["status"] != "unattempted" for r in rows[ordinal:])
        ):
            raise ValueError("CHECKPOINT_BINDING")
        target = batch / "checkpoints" / f"{ordinal:02d}"
        target.mkdir()
        evidence, score = snapshot(config, batch, target)
        save_new(target / "stage-ledger.json", stage)
        if [r["case_id"] for r in score["cases"]] != config["case_ids"]:
            raise ValueError("SCORE_CASE_ORDER")
        reasons = stop_reasons(
            score,
            evidence,
            ordinal,
            sample,
            prior,
            config["policy"],
            read(batch / "registration.json"),
        )
        save_new(
            target / "decision.json",
            {
                "ready_sha256": digest(ready_path),
                "score_sha256": digest(target / "score.json"),
                "reasons": reasons,
                "pg_sample": sample,
            },
        )
        if reasons:
            atomic_json(guard / "stop.json", {"decision": "stop", "reasons": reasons})
            return
        atomic_json(
            release,
            {**ready, "ready_sha256": digest(ready_path), "decision": "continue"},
        )


def cleanup(process: subprocess.Popen[bytes] | None, started: list[str]) -> None:
    """Try every owned stop even if one Docker operation fails."""
    errors = []
    existing = None
    try:
        existing = (
            command(["docker", "ps", "-a", "--format", "{{.Names}}"])
            .decode()
            .splitlines()
        )
    except Exception as error:
        errors.append(type(error).__name__)
    for name in reversed(started):
        try:
            if existing is not None and name not in existing:
                continue
            state = json.loads(
                command(["docker", "inspect", name, "--format", "{{json .State}}"])
            )
            if state["Running"]:
                command(["docker", "stop", "--time", "10", name])
            state = json.loads(
                command(["docker", "inspect", name, "--format", "{{json .State}}"])
            )
            if state["Running"] or state["Pid"]:
                raise ValueError("PROCESS_NOT_REAPED")
        except Exception as error:
            errors.append(type(error).__name__)
    if process is not None:
        try:
            process.wait(timeout=30)
        except Exception as error:
            errors.append(type(error).__name__)
    if errors:
        raise RuntimeError("CLEANUP_REQUIRES_REVIEW")


def run(batch: Path, review: Path) -> None:
    """Execute only a reviewed fresh batch, then preserve results for one review."""
    batch = batch.resolve()
    plan = read(batch / "manifest.json")
    validate_review(batch / "manifest.json", review)
    verify_bound(plan, batch)
    config = load(Path(plan["config"]))
    if (
        (batch / "attempted.json").exists()
        or any((batch / "state").rglob("attempted.json"))
        or any((batch / "guard").iterdir())
    ):
        raise ValueError("BATCH_ALREADY_ATTEMPTED")
    lock = Path(config["stage_lock"])
    with lock.open("x", encoding="utf-8") as output:
        output.write(
            json.dumps(
                {"pid": os.getpid(), "manifest_sha256": digest(batch / "manifest.json")}
            )
        )
    started: list[str] = []
    process: subprocess.Popen[bytes] | None = None
    result: dict[str, Any] | None = None
    head_path = Path(str(lock) + ".inventory.json")
    registered = False
    paid = config["container_prefix"] + "-paid"
    try:
        active = command(["docker", "ps", "--format", "{{.Names}}\t{{.Image}}"])
        if any(
            "jobforge-s5-" in line and ("-paid" in line or "worker" in line)
            for line in active.decode().splitlines()
        ):
            raise ValueError("CONCURRENT_PAID_BATCH")
        existing = (
            command(["docker", "ps", "-a", "--format", "{{.Names}}"])
            .decode()
            .splitlines()
        )
        if any(
            config["container_prefix"] + "-" + name in existing
            for name in ("business", "control", "paid")
        ):
            raise ValueError("CONTAINER_NAME_ALREADY_USED")
        baseline = ledger.collect(config["inventory"])
        ledger.validate_history(read(Path(config["prior_ledger"])), baseline)
        if head_path.exists():
            ledger.validate_stage(read(head_path), baseline, config["control_database"])
        totals = baseline["totals"]
        if any(
            totals[k]
            for k in ("unknown_chat_calls", "inflight", "unfinished_attempts", "held")
        ):
            raise ValueError("LEDGER_NOT_CLOSED")
        current_db = config["control_database"]
        current = next(p for p in baseline["phases"] if p["database"] == current_db)
        if any(current[k] for k in ledger.COUNTS):
            raise ValueError("BATCH_HAS_HISTORY")
        prior = totals["known"]
        if prior != read(batch / "preparation-ledger.json")["totals"]["known"]:
            raise ValueError("STARTUP_LEDGER_CHANGED")
        if money_reasons(current, prior, config["policy"], next_run=True):
            raise ValueError("INSUFFICIENT_FULL_RESERVATION")
        save_new(batch / "launch-ledger.json", baseline)
        save_new(batch / "startup-review.json", read(review))
        save_new(
            batch / "attempted.json",
            {
                "manifest_sha256": digest(batch / "manifest.json"),
                "recorded_at": datetime.now(UTC).isoformat(),
            },
        )
        # Persist inventory under the same exclusive stage lock before a Worker
        # can start. Another already-prepared batch must include this database.
        atomic_json(head_path, baseline)
        registered = True
        generated = commands(config, batch)
        if generated != read(batch / "commands.json"):
            raise ValueError("COMMANDS_CHANGED")
        for name in ("business", "control"):
            started.append(config["container_prefix"] + "-" + name)
            command(generated[name])
        # Inspect the actual deployment again before the paid parent can start.
        actual = json.loads(
            command(
                [
                    "docker",
                    "exec",
                    started[-1],
                    "/usr/local/bin/agent-control",
                    "inspect-support",
                ]
            )
        )
        validate_setup(actual, read(batch / "prepared/launch.json"))
        ready_deadline = time.monotonic() + 30
        while True:
            try:
                command(
                    [
                        "docker",
                        "exec",
                        started[-1],
                        "python",
                        "-c",
                        "from urllib.request import urlopen; urlopen('http://127.0.0.1:8093/health/ready', timeout=2).close()",
                    ]
                )
                break
            except RuntimeError:
                if time.monotonic() >= ready_deadline:
                    raise ValueError("CONTROL_NOT_READY") from None
                time.sleep(0.25)
        audit(config, batch, "before")
        with (
            (batch / "launcher-stdout.log").open("xb") as out,
            (batch / "launcher-stderr.log").open("xb") as err,
        ):
            started.append(paid)
            process = subprocess.Popen(generated["launch"], stdout=out, stderr=err)
            while process.poll() is None:
                sample = ledger.query(current)
                with (batch / "host-monitor.jsonl").open(
                    "a", encoding="utf-8"
                ) as output:
                    output.write(json.dumps(sample) + "\n")
                reasons = money_reasons(sample, prior, config["policy"], next_run=False)
                if reasons:
                    atomic_json(
                        batch / "guard/stop.json",
                        {"decision": "stop", "reasons": reasons},
                    )
                else:
                    verify_bound(plan, batch)
                    advance(config, batch, prior)
                if (batch / "guard/stop.json").exists():
                    command(["docker", "stop", "--time", "10", paid])
                    break
                time.sleep(2)
            process.wait(timeout=30)
        state = json.loads(
            command(["docker", "inspect", paid, "--format", "{{json .State}}"])
        )
        save_new(
            batch / "host-launch-exit.json",
            {"launcher_exit": process.returncode, "state": state},
        )
        if state["Running"] or state["Pid"]:
            raise ValueError("PROCESS_NOT_REAPED")
        deadline = time.monotonic() + 90
        while ledger.query(current)["unfinished_attempts"]:
            if time.monotonic() >= deadline:
                raise ValueError("ATTEMPT_CLOSURE_REQUIRED")
            time.sleep(2)
        if process.returncode and not list((batch / "exports").glob("*/rows.json")):
            raise ValueError("LAUNCHER_FAILED_BEFORE_EXPORT")
        _, score = snapshot(config, batch, batch)
        closed = ledger.collect(config["inventory"])
        quality_applies = quality_gate_applies(read(batch / "registration.json"))
        result = {
            "engineering_complete": True,
            "model_quality_applicable": quality_applies,
            "model_quality_passed": (
                score["correct_count"] >= config["policy"]["minimum_correct"]
                and score["full_case_evidence_count"]
                >= config["policy"]["minimum_full_evidence"]
                and not any(score["hard_failure_cases"].values())
                and score["proposal_audit_complete"]
                and not any(
                    closed["totals"][k]
                    for k in (
                        "unknown_chat_calls",
                        "held",
                        "inflight",
                        "unfinished_attempts",
                    )
                )
            )
            if quality_applies
            else None,
            "scope": (
                "seen_v3_regression_not_unseen_acceptance"
                if config["schema_version"] == 2
                else "development_regression_not_s5_overall_acceptance"
            ),
            "score_sha256": digest(batch / "score.json"),
            "denominator": score["denominator"],
            "correct": score["correct_count"],
            "full_evidence": score["full_case_evidence_count"],
            "ledger": closed["totals"],
            "post_batch_review": "pending",
        }
        if config["schema_version"] == 2:
            result["freeze_guard_cases"] = {
                row["case_id"]: row["correct"] and row["full_case_evidence"]
                for row in score["cases"]
                if row["case_id"] in SEEN_V3_GUARDS
            }
            result["freeze_guard_cases_passed"] = set(
                result["freeze_guard_cases"]
            ) == set(SEEN_V3_GUARDS) and all(result["freeze_guard_cases"].values())
        if not quality_applies:
            result["baseline_collection_complete"] = (
                score["denominator"] == config["policy"]["case_count"]
                and ledger.query(current)["runs"] == config["policy"]["case_count"]
                and score["proposal_audit_complete"]
                and not any(score["hard_failure_cases"].values())
            )
    except Exception as error:
        atomic_json(
            batch / "workflow-error.json",
            {
                "error_category": type(error).__name__,
                "reason": str(error)
                if re.fullmatch(r"[A-Z][A-Z0-9_]{0,95}", str(error))
                else None,
                "evidence_preserved": True,
            },
        )
        raise
    finally:
        try:
            cleanup(process, started)
            if registered:
                final_ledger = ledger.collect(config["inventory"])
                ledger.validate_history(read(head_path), final_ledger)
                atomic_json(head_path, final_ledger)
                save_new(batch / "closed-ledger.json", final_ledger)
                if result is not None:
                    result["ledger"] = final_ledger["totals"]
                    exposure_closed = not any(
                        final_ledger["totals"][k]
                        for k in (
                            "held",
                            "unknown_chat_calls",
                            "inflight",
                            "unfinished_attempts",
                        )
                    )
                    if result["model_quality_applicable"]:
                        result["model_quality_passed"] &= exposure_closed
                    else:
                        result["baseline_collection_complete"] &= exposure_closed
        except Exception:
            save_new(
                batch / "cleanup-error.json",
                {"code": "CLEANUP_REQUIRES_REVIEW", "stage_lock_retained": True},
            )
            raise
        lock.unlink()
    if result is not None:
        save_new(batch / "result.json", result)


def main() -> None:
    """Separate free preparation/checks from the explicit paid run command."""
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="mode", required=True)
    prep = sub.add_parser("prepare")
    prep.add_argument("--config", type=Path, required=True)
    prep.add_argument("--output", type=Path, required=True)
    for mode in ("check", "run"):
        entry = sub.add_parser(mode)
        entry.add_argument("--batch", type=Path, required=True)
        entry.add_argument("--review", type=Path, required=True)
    args = parser.parse_args()
    if args.mode == "prepare":
        prepare(args.config, args.output)
    elif args.mode == "check":
        validate_review(args.batch / "manifest.json", args.review)
        verify_bound(read(args.batch / "manifest.json"), args.batch)
    else:
        run(args.batch, args.review)


if __name__ == "__main__":
    main()
