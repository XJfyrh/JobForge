"""Execute one released finite S4 list with installed SDK and formal Linux Workers."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import signal
import subprocess
import time
from datetime import UTC, datetime
from pathlib import Path
from typing import Any
from urllib.parse import parse_qs, unquote, urlsplit

from jobforge import RunClient

from tools.support_approval.plan import validate
from tools.support_approval.quality import evaluate_pending, model_audit_complete
from tools.support_evaluation.assemble import outbound, safety
from tools.support_evaluation.driver import instant
from tools.support_evaluation.evidence import fingerprint, load_package
from tools.support_evaluation.export import CaptureTransport, atomic_json, export_run
from tools.support_evaluation.launcher import validate_setup
from tools.support_recovery.driver import (
    FailureCapture,
    Supervisor,
    category,
    children,
    require_linux,
    wait_proxy,
)


def sha(path: Path) -> str:
    """Bind exact protected source/artifact bytes."""
    return hashlib.sha256(path.read_bytes()).hexdigest()


def sql(
    settings: dict[str, Any],
    name: str,
    credential: str,
    variables: dict[str, str] | None = None,
) -> dict[str, Any]:
    """Run only a fixed checked audit or the predeclared loader revision change."""
    command = ["psql", "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1"]
    for key, value in (variables or {}).items():
        command.extend(["-v", key + "=" + value])
    command.extend(["-f", "/app/tools/support_approval/" + name + ".sql"])
    result = subprocess.run(
        command,
        env=postgres_environment(settings[credential]),
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        check=True,
        timeout=10,
    )
    if len(result.stdout) > 2 * 1024 * 1024:
        raise ValueError("AUDIT_TOO_LARGE")
    document: dict[str, Any] = json.loads(result.stdout)
    return document


def postgres_environment(dsn: str) -> dict[str, str]:
    """Expand the private URI into libpq environment fields, without CLI secrets."""
    try:
        parts = urlsplit(dsn)
        query = parse_qs(parts.query, strict_parsing=True)
        port = parts.port or 5432
    except ValueError:
        raise ValueError("INVALID_AUDIT_CONNECTION") from None
    if (
        parts.scheme not in {"postgres", "postgresql"}
        or not parts.hostname
        or not parts.username
        or not parts.path.startswith("/")
        or len(parts.path) < 2
        or parts.fragment
        or set(query) != {"sslmode"}
        or len(query["sslmode"]) != 1
        or query["sslmode"][0] not in {"disable", "require", "verify-ca", "verify-full"}
    ):
        raise ValueError("INVALID_AUDIT_CONNECTION")
    return dict(
        os.environ,
        PGHOST=parts.hostname,
        PGPORT=str(port),
        PGUSER=unquote(parts.username),
        PGPASSWORD=unquote(parts.password or ""),
        PGDATABASE=unquote(parts.path[1:]),
        PGSSLMODE=query["sslmode"][0],
    )


def final_audits(
    settings: dict[str, Any], plan: dict[str, Any], output: Path
) -> list[str]:
    """Try both read-only audits after execution or audit failure."""
    errors = []
    for name, query, credential, variables in (
        ("business-actions-final", "actions_audit", "business_receipt_dsn", None),
        (
            "control-audit-final",
            "control_audit",
            "control_audit_dsn",
            {"batch_id": plan["batch_account_id"]},
        ),
    ):
        try:
            atomic_json(
                output / (name + ".json"), sql(settings, query, credential, variables)
            )
        except Exception as error:
            errors.append(name + ":" + category(error))
    return errors


def verify_release(
    plan_path: Path, settings_path: Path, release_path: Path
) -> tuple[dict[str, Any], dict[str, Any]]:
    """Require independent approval of exact list/settings, head, build and checks."""
    plan, settings, release = (
        json.loads(path.read_bytes())
        for path in (plan_path, settings_path, release_path)
    )
    validate(plan, check_sources=False)
    build = Path(settings["build_receipt"])
    receipt = json.loads(build.read_bytes())
    if (
        release.get("approved") is not True
        or release.get("execution_list_sha256") != sha(plan_path)
        or release.get("settings_sha256") != sha(settings_path)
        or release.get("build_receipt_sha256") != sha(build)
        or release.get("head") != receipt["head"]
        or release.get("independent_review_passed") is not True
        or set(release.get("ci_jobs", {}))
        != {
            "go-lint",
            "go-test",
            "python-lint",
            "business-contract",
            "agent-runtime-contract",
            "proto-lint",
            "executor-process-probe",
            "observability-config",
        }
        or any(state != "passed" for state in release["ci_jobs"].values())
        or release.get("production_image_digest") != receipt["production_image_digest"]
        or release.get("operator_image_id") != receipt["operator_image_id"]
        or settings.get("operator_image_id") != receipt["operator_image_id"]
        or receipt["sources"] != plan["sources"]
        or receipt["worker_binary_sha256"] != sha(Path("/usr/local/bin/agent-worker"))
        or receipt["control_binary_sha256"] != sha(Path("/usr/local/bin/agent-control"))
        or receipt["proxy_binary_sha256"]
        != sha(Path("/usr/local/bin/supportapprovalproxy"))
        or sha(Path(settings["control_config"])) != plan["control_sha256"]["enabled"]
        or sha(Path("/etc/jobforge/executor.json"))
        != plan["config_sha256"]["executor.json"]
        or [row["worker_id"] for row in settings["workers"]] != plan["workers"]
        or any(
            sha(Path(row["config"])) != plan["config_sha256"]["worker.json"]
            for row in settings["workers"]
        )
        or release.get("price_snapshot_sha256")
        != plan["definition"]["price"]["source_sha256"]
        or not release.get("provider_account_snapshot_sha256")
    ):
        raise ValueError("S4_NOT_RELEASED_OR_RUNTIME_CHANGED")
    from importlib import import_module

    for module, prefix, key in (
        ("jobforge", "sdk/python/jobforge/", "sdk_source_sha256"),
        ("jobforge_agent", "python/jobforge_agent/", "adapter_source_sha256"),
    ):
        location = import_module(module).__file__
        if location is None:
            raise ValueError("INSTALLED_PACKAGE_UNAVAILABLE")
        parts: list[str] = []
        for file in sorted(Path(location).parent.glob("*.py")):
            parts.extend((prefix + file.name, sha(file)))
        if fingerprint("jobforge.support.adapter-source.v1", *parts) != receipt[key]:
            raise ValueError("INSTALLED_PACKAGE_CHANGED")
    if (
        receipt["adapter_source_sha256"]
        != plan["definition"]["program"]["adapter_source_sha256"]
    ):
        raise ValueError("PROFILE_ADAPTER_CHANGED")
    for path, digest in plan["sources"].items():
        if (
            path.startswith("tools/support_approval/")
            or path.startswith("tools/support_evaluation/")
            or path
            in {
                "tools/support_recovery/driver.py",
                "tools/support_recovery/plan.py",
                "tools/support_recovery/continuation.py",
            }
        ):
            if sha(Path("/app") / path) != digest:
                raise ValueError("INSTALLED_OPERATOR_TOOL_CHANGED")
    if load_package().hashes != plan["package_sha256"]:
        raise ValueError("FROZEN_BUSINESS_PACKAGE_CHANGED")
    prior = release.get("new_prior_exposure", [])
    if prior or settings["approval_actor_id"] != "s4-acceptance-operator":
        raise ValueError("FRESH_S4_BATCH_REQUIRED")
    return plan, settings


def launch(plan: dict[str, Any], settings: dict[str, Any], output: Path) -> None:
    """Every source is submitted once; every failed/unexercised row is retained."""
    require_linux()
    directory = Path(settings["barriers"])
    directory.mkdir(mode=0o700)
    output.mkdir(mode=0o700)
    control = subprocess.run(
        ["/usr/local/bin/agent-control", "inspect-support"],
        env=dict(os.environ, JOBFORGE_AGENT_CONFIG=settings["control_config"]),
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        check=True,
        timeout=10,
    )
    inspection = json.loads(control.stdout)
    validate_setup(inspection, plan)
    if inspection["profile"]["profile_hash"] != plan["profile"][
        "profile_hash"
    ] or not any(
        account["account_id"] == plan["batch_account_id"]
        and account["limits"]["cost_microyuan"] <= plan["max_cost_microyuan"]
        for account in inspection["accounts"]
        if account["scope"] == "batch"
    ):
        raise ValueError("S4_SETUP_BINDING_CHANGED")
    atomic_json(output / "setup.json", inspection)
    initial_actions = sql(settings, "actions_audit", "business_receipt_dsn")
    if (
        initial_actions["resolutions"]
        or initial_actions["database"] != settings["business_database"]
    ):
        raise ValueError("S4_BUSINESS_HAS_HISTORY")
    atomic_json(output / "business-actions-initial.json", initial_actions)
    rows = [
        dict(
            row,
            status="unattempted",
            run_id=None,
            quality=None,
            mechanism="unexercised",
            error_code="",
        )
        for row in plan["runs"]
    ]
    retry_count = 0
    supervisor = Supervisor(settings, directory)
    proxy = subprocess.Popen(
        [
            "/usr/local/bin/supportapprovalproxy",
            "--upstream",
            settings["upstream_gateway"],
            "--business",
            settings["business_origin"],
            "--directory",
            str(directory),
        ],
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )

    def save() -> None:
        atomic_json(
            output / "rows.json",
            {"schema_version": 1, "runs": rows, "retry_count": retry_count},
        )

    def check() -> None:
        if (
            not instant(plan["valid_from"])
            <= datetime.now(UTC)
            < instant(plan["valid_until"])
        ):
            raise ValueError("S4_WINDOW_CLOSED")

    def wait(
        client: RunClient, run_id: str, states: set[str], seconds: int = 600
    ) -> None:
        until = time.monotonic() + seconds
        while time.monotonic() < until:
            check()
            current = client.get(run_id)
            if (
                current.budget.batch.frozen
                or current.budget.batch.used.cost_microyuan > plan["max_cost_microyuan"]
            ):
                raise ValueError("S4_BATCH_STOPPED")
            if current.state.value in states:
                return
            if current.state.value in {"failed", "cancelled", "succeeded"}:
                raise ValueError("S4_UNEXPECTED_TERMINAL")
            time.sleep(0.05)
        raise ValueError("S4_WAIT_TIMEOUT")

    def export(
        client: RunClient,
        capture: CaptureTransport,
        run_id: str,
        archive: Path,
        name: str,
    ) -> dict[str, Any]:
        evidence, events = export_run(client, capture, run_id)
        client.effect(run_id)
        evidence["effect"] = capture.object()
        client.action_calls(run_id)
        evidence["action_calls"] = capture.object()
        if evidence["run"]["proposal_ref"] is not None:
            client.approval(run_id)
            evidence["approval"] = capture.object()
        atomic_json(archive / (name + ".json"), evidence)
        atomic_json(archive / (name + "-events.json"), {"events": events})
        return evidence

    save()
    row = rows[0]
    archive = output
    failed = False
    try:
        wait_proxy(proxy)
        supervisor.start()
        for row in rows:
            check()
            archive = output / row["case_id"]
            archive.mkdir(mode=0o700)
            capture = CaptureTransport(archive)
            binding = row["binding"]
            with RunClient(
                settings["control_origin"],
                settings["driver_tokens"][binding["tenant_id"]],
                timeout=5,
                transport=capture,
            ) as client:
                before = sql(settings, "business_audit", "business_reader_dsn")
                if before["database"] != settings["business_database"] or not before[
                    "database"
                ].startswith("jobforge_s4_"):
                    raise ValueError("S4_ISOLATED_BUSINESS_DATABASE_REQUIRED")
                atomic_json(archive / "before.json", before)
                row["status"] = "submission_attempted"
                save()
                submitted = client.submit(
                    binding["ticket_id"],
                    binding["business_request_key"],
                    plan["profile"]["profile_id"],
                    plan["batch_key"],
                    idempotency_key=row["idempotency_key"],
                    run_timeout_seconds=900,
                )
                row["run_id"] = run_id = submitted.run.run_id
                if submitted.reused:
                    raise ValueError("S4_INTENT_ALREADY_USED")
                save()
                wait(
                    client,
                    run_id,
                    {"awaiting_approval", "failed", "cancelled", "succeeded"},
                )
                pending = export(client, capture, run_id, archive, "pending")
                after = sql(settings, "business_audit", "business_reader_dsn")
                atomic_json(archive / "preapproval-after.json", after)
                traces, trace_hash, complete = outbound(Path(settings["outbound"]))
                pending["case_id"] = row["case_id"]
                pending["status"], pending["error_code"] = "run", ""
                pending["safety"] = safety(
                    traces.get(pending["run"]["snapshot_id"], []),
                    trace_hash,
                    before,
                    after,
                    fingerprint(
                        sha(archive / "before.json"),
                        sha(archive / "preapproval-after.json"),
                    ),
                    complete,
                )
                atomic_json(archive / "quality-input.json", pending)
                quality = evaluate_pending(
                    pending, binding, plan["profile"], load_package()
                )
                row["quality"] = quality
                atomic_json(archive / "quality.json", quality)
                save()
                if not quality["approval_eligible"]:
                    if pending["run"]["state"] == "awaiting_approval":
                        client.cancel(
                            run_id, idempotency_key="quality-cancel-" + run_id
                        )
                    row["status"] = "quality_failed"
                    export(client, capture, run_id, archive, "final")
                    save()
                    if not model_audit_complete(pending):
                        raise ValueError("S4_MODEL_AUDIT_STOP")
                    continue
                view = client.approval(run_id)
                approval = capture.object()
                proposal = pending["steps"][-1]["record"]["output"]["proposal"]
                if approval["proposal"] != proposal or not view.available:
                    raise ValueError("APPROVAL_SOURCE_CHANGED")
                atomic_json(archive / "original-approval.json", approval)
                boundary = (
                    "authorization_saved"
                    if row["intent"]
                    in {"ticket_revision_conflict", "authorization_cancel_retry"}
                    else "business_committed"
                    if row["intent"].startswith("commit_")
                    else None
                )
                if boundary is not None:
                    atomic_json(
                        directory / "fault.json",
                        {
                            "run_id": run_id,
                            "tenant_id": binding["tenant_id"],
                            "profile_hash": plan["profile"]["profile_hash"],
                            "boundary": boundary,
                        },
                    )
                decision = (
                    "reject" if row["intent"] == "reject_valid_proposal" else "approve"
                )
                with RunClient(
                    settings["control_origin"],
                    settings["approver_tokens"][binding["tenant_id"]],
                    timeout=5,
                    transport=capture,
                ) as approver:
                    accepted = approver.decide_approval(
                        run_id,
                        decision,
                        view.proposal_hash,
                        idempotency_key="decision-" + run_id,
                    )
                    if accepted.approval.actor_id != settings["approval_actor_id"]:
                        raise ValueError("ACCEPTANCE_ACTOR_CHANGED")
                    atomic_json(archive / "decision.json", capture.object())
                if boundary is not None:
                    barrier = directory / (run_id + ".barrier.json")
                    until = time.monotonic() + 10
                    while not barrier.exists() and time.monotonic() < until:
                        check()
                        time.sleep(0.01)
                    if (
                        not barrier.exists()
                        or not (directory / (run_id + ".active")).exists()
                    ):
                        raise ValueError("S4_FAULT_BOUNDARY_MISSED")
                    atomic_json(
                        archive / "barrier.json", json.loads(barrier.read_bytes())
                    )
                    client.get(run_id)
                    atomic_json(archive / "pre-fault-run.json", capture.object())
                    if row["intent"] == "ticket_revision_conflict":
                        mutation = sql(
                            settings,
                            "loader_revision",
                            "business_loader_dsn",
                            {
                                "tenant_id": binding["tenant_id"],
                                "ticket_id": binding["ticket_id"],
                            },
                        )
                        atomic_json(archive / "loader-revision.json", mutation)
                        if mutation[
                            "role"
                        ] != "jobforge_business_loader_login" or mutation[
                            "changed"
                        ] != [
                            {
                                "tenant_id": binding["tenant_id"],
                                "ticket_id": binding["ticket_id"],
                                "revision": pending["run"]["version_vector"]["ticket"][
                                    "revision"
                                ]
                                + 1,
                            }
                        ]:
                            raise ValueError("LOADER_REVISION_NOT_CHANGED")
                        (directory / (run_id + ".release")).touch(exist_ok=False)
                    else:
                        if supervisor.current is None or children(
                            supervisor.current.pid, "jobforge_agent.guardian"
                        ):
                            raise ValueError("S4_ACTION_BOUNDARY_HAS_PYTHON_CHILD")
                        if row["intent"] != "commit_loss_natural_recovery":
                            client.cancel(
                                run_id, idempotency_key="fault-cancel-" + run_id
                            )
                        row["fault"] = supervisor.stop(kill=True)
                        row["fault"]["child_group_at_boundary"] = "no_child"
                        save()
                        if row["intent"] == "commit_loss_natural_recovery":
                            wait(client, run_id, {"ready"}, 45)
                            (directory / "fault.json").unlink()
                            supervisor.start()
                wait(client, run_id, {"succeeded", "failed", "cancelled"}, 60)
                first_terminal = export(client, capture, run_id, archive, "terminal")
                if row["intent"] == "commit_cancel_reconcile":
                    client.reconcile(run_id)
                    atomic_json(archive / "reconcile.json", capture.object())
                if row["case_id"] in plan["retry_sources"]:
                    if retry_count >= 2:
                        raise ValueError("S4_RETRY_CAP")
                    row["retry_status"] = "attempted"
                    retry_count += 1
                    save()
                    child = client.retry(
                        run_id, idempotency_key="receipt-retry-" + run_id
                    )
                    row["retry_run_id"] = child.run.run_id
                    export(client, capture, child.run.run_id, archive, "retry")
                final = export(client, capture, run_id, archive, "final")
                if (
                    final["run"] != first_terminal["run"]
                    or final["result"] != first_terminal["result"]
                ):
                    raise ValueError("TERMINAL_EXECUTION_CHANGED")
                audit = sql(settings, "actions_audit", "business_receipt_dsn")
                atomic_json(archive / "business-actions.json", audit)
                control_audit = sql(
                    settings,
                    "control_audit",
                    "control_audit_dsn",
                    {"batch_id": plan["batch_account_id"]},
                )
                atomic_json(archive / "control-audit.json", control_audit)
                row["status"], row["mechanism"] = (
                    "finished",
                    "observed_pending_independent_validation",
                )
                if (directory / "fault.json").exists():
                    (directory / "fault.json").unlink()
                if supervisor.current is None:
                    supervisor.start()
                save()
    except Exception as error:
        failed = True
        row["error_code"] = category(error)
        save()
        if supervisor.current is not None:
            try:
                supervisor.stop()
            except (OSError, ValueError):
                row["cleanup_error"] = "WORKER_CLEANUP_FAILED"
        if row["run_id"]:
            try:
                failure = archive / "failure-api"
                failure.mkdir(mode=0o700)
                capture = FailureCapture(failure)
                with RunClient(
                    settings["control_origin"],
                    settings["driver_tokens"][row["binding"]["tenant_id"]],
                    timeout=2,
                    transport=capture,
                ) as client:
                    export(client, capture, row["run_id"], archive, "failure")
            except Exception as failure_error:
                row["failure_export_error"] = category(failure_error)
        save()
        raise
    finally:
        cleanup_errors = []
        if supervisor.current is not None:
            try:
                supervisor.stop()
            except Exception as error:
                cleanup_errors.append(category(error))
        proxy.terminate()
        try:
            proxy.wait(timeout=3)
        except subprocess.TimeoutExpired:
            proxy.kill()
            proxy.wait(timeout=2)
        cleanup_errors.extend(final_audits(settings, plan, output))
        atomic_json(output / "cleanup.json", {"errors": cleanup_errors})
        if cleanup_errors and not failed:
            raise ValueError("S4_FINAL_AUDIT_INCOMPLETE")


def main() -> None:
    """Release is issued after final checks/review, never by this driver."""
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("plan", "settings", "release", "out"):
        parser.add_argument("--" + name, type=Path, required=True)
    args = parser.parse_args()
    plan, settings = verify_release(args.plan, args.settings, args.release)

    def stop(_signal: int, _frame: Any) -> None:
        raise ValueError("OPERATOR_STOP")

    for selected in (signal.SIGINT, signal.SIGTERM):
        signal.signal(selected, stop)
    launch(plan, settings, args.out)


if __name__ == "__main__":
    main()
