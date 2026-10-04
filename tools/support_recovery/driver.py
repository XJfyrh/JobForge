"""Run one released S3 list with real Worker replacement and existing SDK APIs."""

from __future__ import annotations

import argparse
import array
import hashlib
import json
import os
import signal
import socket
import subprocess
import time
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

import httpx
from jobforge import RunClient

from tools.support_evaluation.driver import instant
from tools.support_evaluation.export import (
    CaptureTransport,
    ExportError,
    atomic_json,
    export_run,
)
from tools.support_recovery.plan import validate

WORKER = "/usr/local/bin/agent-worker"


def verify_runtime(
    plan: dict[str, Any], settings: dict[str, Any], release: dict[str, Any]
) -> None:
    """Bind actual private mounts, installed packages and binaries before Submit."""
    from importlib import import_module

    from tools.support_evaluation.evidence import fingerprint

    def digest(path: Path) -> str:
        return hashlib.sha256(path.read_bytes()).hexdigest()

    def package_digest(module: str, prefix: str) -> str:
        parts: list[str] = []
        source = import_module(module).__file__
        if source is None:
            raise ValueError("INSTALLED_PACKAGE_UNAVAILABLE")
        location = Path(source).parent
        for path in sorted(location.glob("*.py")):
            parts.extend((prefix + path.name, digest(path)))
        return fingerprint("jobforge.support.adapter-source.v1", *parts)

    build, preflight = (
        Path(settings["build_receipt"]),
        Path(settings["preflight_receipt"]),
    )
    receipt, checked = (
        json.loads(build.read_bytes()),
        json.loads(preflight.read_bytes()),
    )
    if (
        digest(build) != release["build_receipt_sha256"]
        or digest(preflight) != release["preflight_sha256"]
        or checked["execution_list_sha256"] != release["execution_list_sha256"]
        or checked["source_check_passed"] is not True
        or receipt["worker_binary_sha256"] != digest(Path(WORKER))
        or receipt["control_binary_sha256"]
        != digest(Path("/usr/local/bin/agent-control"))
        or receipt["proxy_binary_sha256"]
        != digest(Path("/usr/local/bin/supportrecoveryproxy"))
        or receipt["adapter_source_sha256"]
        != package_digest("jobforge_agent", "python/jobforge_agent/")
        or receipt["adapter_source_sha256"]
        != plan["definition"]["program"]["adapter_source_sha256"]
        or receipt["sdk_source_sha256"]
        != package_digest("jobforge", "sdk/python/jobforge/")
        or receipt["production_image_digest"] != release["production_image_digest"]
        or digest(Path(settings["control_config"])) != plan["control_sha256"]["enabled"]
        or digest(Path("/etc/jobforge/executor.json"))
        != plan["config_sha256"]["executor.json"]
        or any(
            digest(Path(worker["config"])) != plan["config_sha256"]["worker.json"]
            for worker in settings["workers"]
        )
    ):
        raise ValueError("S3_ACTUAL_RUNTIME_MISMATCH")
    prior = release["prior_batch_exposure"]
    if len({row["batch_account_id"] for row in prior}) != len(prior) or any(
        row["held_cost_microyuan"]
        or row["batch_frozen"]
        or row["unknown_chat_calls"]
        or row["known_cost_microyuan"] < 0
        for row in prior
    ):
        raise ValueError("S3_PRIOR_BATCH_UNRESOLVED")
    remaining = 5_000_000 - sum(
        row["known_cost_microyuan"] + row["held_cost_microyuan"] for row in prior
    )
    control = json.loads(Path(settings["control_config"]).read_bytes())
    batch = [
        row
        for row in control["budgets"]
        if row["scope"] == "batch" and row["account_id"] == plan["batch_account_id"]
    ]
    if len(batch) != 1 or batch[0]["limits"]["cost_microyuan"] > remaining:
        raise ValueError("S3_CUMULATIVE_CAP_MISMATCH")


class FailureCapture(CaptureTransport):
    """Bound the entire best-effort failure export while preserving old archives."""

    def __init__(self, archive: Path) -> None:
        """Use a fresh subdirectory and a ten-second total read budget."""
        super().__init__(archive)
        self.deadline = time.monotonic() + 10

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        """Limit each single exchange by the remaining total export time."""
        remaining = self.deadline - time.monotonic()
        if remaining <= 0:
            raise ExportError("FAILURE_EXPORT_DEADLINE")
        request.extensions["timeout"] = {
            key: min(2, remaining) for key in ("connect", "read", "write", "pool")
        }
        return super().handle_request(request)


def chat_barrier(evidence: dict[str, Any], committed: bool = False) -> bool:
    """Inspect exported facts only; Go/PostgreSQL remain the permission authority."""
    calls = evidence["calls"]
    if calls["batch_frozen"] or calls["batch_stop_code"] is not None:
        return False
    committed_calls = {
        row["record"]["output"].get("physical_call_id") for row in evidence["steps"]
    }
    for call in calls["items"]:
        if call["subcall"] != "chat":
            continue
        audit = call["provider_audit"]
        if (
            not call["usage_known"]
            or call["held_cost_microyuan"]
            or call["measurement_anomaly"]
            or call["report_conflict"]
            or call["report_hash"] is None
            or call["observed_at"] is None
            or call["settled_at"] is None
            or call["transport_outcome"] != "response"
            or call["business_outcome"] != "accepted"
            or call["error_code"]
            or audit is None
            or audit["identity_state"] != "compatible"
            or audit["mode_state"] != "nonthinking"
            or (committed and call["physical_call_id"] not in committed_calls)
        ):
            return False
    return True


def children(parent: int, module: str) -> list[int]:
    """Identify actual fixed processes from PPid and argv, never a marker PID alone."""
    result = []
    for entry in Path("/proc").iterdir():
        if not entry.name.isdecimal():
            continue
        try:
            status = (entry / "status").read_text()
            argv = (entry / "cmdline").read_bytes().split(b"\0")
            if f"PPid:\t{parent}\n" in status and module.encode() in argv:
                result.append(int(entry.name))
        except (FileNotFoundError, ProcessLookupError):
            continue
    return result


def gone(group: int, step: int) -> bool:
    """The old group and step must disappear before any replacement starts."""
    try:
        os.killpg(group, 0)
        return False
    except ProcessLookupError:
        return not Path(f"/proc/{step}").exists()


def wait_fact(fact: Any, seconds: float) -> None:
    """Wait for a real bounded fact; an elapsed timeout cannot stand in for it."""
    deadline = time.monotonic() + seconds
    while not fact():
        if time.monotonic() >= deadline:
            raise ValueError("RECOVERY_BARRIER_TIMEOUT")
        time.sleep(0.002)


def require_linux() -> None:
    """The external process supervisor requires the fixed Linux runtime."""
    if os.name != "posix" or not Path("/proc").is_dir():
        raise ValueError("FIXED_LINUX_REQUIRED")


def inspect(plan: dict[str, Any], settings: dict[str, Any]) -> dict[str, Any]:
    """Read the exact checked deployment and bind its original batch/profile."""
    from tools.support_evaluation.launcher import validate_setup

    path = Path(settings["control_config"])
    raw = path.read_bytes()
    if hashlib.sha256(raw).hexdigest() != plan["control_sha256"]["enabled"]:
        raise ValueError("S3_INSPECTION_CONFIG_MISMATCH")
    environment = dict(os.environ, JOBFORGE_AGENT_CONFIG=str(path))
    checked = subprocess.run(
        ["/usr/local/bin/agent-control", "inspect-support"],
        env=environment,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        check=True,
        timeout=10,
    )
    receipt: dict[str, Any] = json.loads(checked.stdout)
    accounts = [
        account for account in receipt["accounts"] if account["scope"] == "batch"
    ]
    configured = [row for row in json.loads(raw)["budgets"] if row["scope"] == "batch"]
    if (
        receipt["profile"]["profile_id"] != plan["profile"]["profile_id"]
        or receipt["profile"]["profile_hash"] != plan["profile"]["profile_hash"]
        or len(accounts) != 1
        or len(configured) != 1
        or accounts[0]["account_id"] != plan["batch_account_id"]
        or accounts[0]["scope_key"] != plan["batch_key"]
        or accounts[0]["limits"] != configured[0]["limits"]
        or accounts[0]["limits"]["cost_microyuan"] > plan["max_cost_microyuan"]
    ):
        raise ValueError("S3_INSPECTION_BINDING_MISMATCH")
    validate_setup(receipt, plan)
    return receipt


def wait_proxy(process: subprocess.Popen[bytes]) -> None:
    """Start a Worker only after the owned loopback proxy actually listens."""

    def listening() -> bool:
        try:
            with socket.create_connection(("127.0.0.1", 8095), timeout=0.1):
                return process.poll() is None
        except OSError:
            return False

    wait_fact(listening, 3)


class Supervisor:
    """Own only two predeclared local processes; no Claim, Retry or timing SQL."""

    def __init__(self, settings: dict[str, Any], directory: Path) -> None:
        """Bind private paths and the two approved principal identities."""
        self.settings, self.directory = settings, directory
        self.current: subprocess.Popen[bytes] | None = None
        self.index = -1
        self.last_stop: dict[int, float] = {}

    def start(self) -> None:
        """A fresh principal waits conservatively for natural session protection."""
        self.index = (self.index + 1) % 2
        remaining = self.last_stop.get(self.index, -61) + 61 - time.monotonic()
        if remaining > 0:
            time.sleep(remaining)
        worker = self.settings["workers"][self.index]
        environment = {
            "PATH": "/usr/local/bin:/usr/bin:/bin",
            "JOBFORGE_AGENT_WORKER_CONFIG": worker["config"],
            "JOBFORGE_AGENT_WORKER_CREDENTIALS_FILE": worker["credentials"],
            "JOBFORGE_AGENT_GATEWAY": "127.0.0.1:8095",
            "JOBFORGE_AGENT_GRPC_TLS": "false",
        }
        self.current = subprocess.Popen(
            [WORKER],
            env=environment,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )

    def stop(self, kill: bool = False) -> None:
        """Actual Wait is mandatory, including an externally killed Worker."""
        if self.current is None:
            return
        process = self.current
        if process.poll() is None:
            if kill:
                process.kill()
            else:
                process.terminate()
        try:
            code = process.wait(timeout=3)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=2)
            raise ValueError("WORKER_CLEANUP_TIMEOUT") from None
        self.last_stop[self.index] = time.monotonic()
        self.current = None
        if code != (-signal.SIGKILL if kill else 0):
            raise ValueError("WORKER_EXIT_UNEXPECTED")

    def inject(self, row: dict[str, Any], barrier: dict[str, Any]) -> dict[str, Any]:
        """Keep PID/Wait/pipe facts; never describe queued ACK as consumed by Python."""
        if self.current is None or self.current.poll() is not None:
            raise ValueError("WORKER_ALREADY_STOPPED")
        worker = self.current.pid
        if (
            barrier["phase"] != "commit_ack_lost"
            and not (self.directory / f"{row['experiment']}.active").is_file()
        ):
            raise ValueError("INJECTION_WINDOW_MISSED")
        guardians = children(worker, "jobforge_agent.guardian")
        group, step = 0, 0
        if guardians:
            if len(guardians) != 1:
                raise ValueError("GUARDIAN_IDENTITY_MISMATCH")
            group = guardians[0]
            steps = children(group, "jobforge_agent.step")
            if len(steps) != 1 or os.getpgid(group) != group:
                raise ValueError("STEP_IDENTITY_MISMATCH")
            step = steps[0]
        elif barrier["phase"] != "commit_ack_lost":
            raise ValueError("INJECTION_WINDOW_MISSED")
        queued = 0
        if row["target"] == "step":
            import fcntl
            import termios

            os.kill(step, signal.SIGSTOP)
            wait_fact(
                lambda: "State:\tT" in Path(f"/proc/{step}/status").read_text(), 0.25
            )
            descriptor = os.open(f"/proc/{step}/fd/0", os.O_RDONLY | os.O_NONBLOCK)
            try:

                def pending() -> int:
                    count = array.array("i", [0])
                    fcntl.ioctl(descriptor, termios.FIONREAD, count, True)
                    return int(count[0])

                if pending() != 0:
                    raise ValueError("ORDINARY_PIPE_NOT_EMPTY")
                if not (self.directory / f"{row['experiment']}.active").is_file():
                    raise ValueError("INJECTION_WINDOW_MISSED")
                (self.directory / f"{row['experiment']}.release").touch(exist_ok=False)
                wait_fact(lambda: pending() > 0, 0.75)
                queued = pending()
                os.kill(step, signal.SIGKILL)
            finally:
                os.close(descriptor)
            wait_fact(lambda: gone(group, step), 3)
            self.stop()
        else:
            self.stop(kill=True)
            if group:
                wait_fact(lambda: gone(group, step), 3)
        return {
            "worker_pid": worker,
            "guardian_pid": group,
            "step_pid": step,
            "worker_waited": True,
            "group_gone": True,
            "ordinary_ack_queued_bytes": queued,
            "python_ack_consumed": False,
            "killed_at": datetime.now(UTC).isoformat(),
        }


def launch(plan: dict[str, Any], settings: dict[str, Any], output: Path) -> None:
    """Execute each persistent intent once; retain every failure and unattempted row."""
    validate(plan, check_sources=False)
    require_linux()
    if [worker["worker_id"] for worker in settings["workers"]] != plan["workers"]:
        raise ValueError("WORKER_ALLOWLIST_MISMATCH")
    # Inspect the actual isolated batch immediately before its sole launch.
    # Credentials/DSN stay in the operator process environment and never reach
    # the Worker or Python step. This command only performs read transactions.
    inspection = inspect(plan, settings)
    output.mkdir(mode=0o700)
    atomic_json(output / "setup.json", inspection)
    directory = Path(settings["barriers"])
    directory.mkdir(mode=0o700)
    rows = [
        dict(row, status="unattempted", run_id=None, error_code="")
        for row in plan["runs"]
    ]
    row = rows[0]

    def save() -> None:
        atomic_json(output / "rows.json", {"schema_version": 1, "runs": rows})

    save()
    supervisor = Supervisor(settings, directory)
    proxy = subprocess.Popen(
        [
            "/usr/local/bin/supportrecoveryproxy",
            "--upstream",
            settings["upstream_gateway"],
            "--directory",
            str(directory),
        ],
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    deadline = instant(plan["valid_until"])

    def check() -> None:
        if not instant(plan["valid_from"]) <= datetime.now(UTC) < deadline:
            raise ValueError("S3_WINDOW_CLOSED")

    try:
        wait_proxy(proxy)
        for row in rows:
            check()
            archive = output / row["experiment"]
            archive.mkdir(mode=0o700)
            capture = CaptureTransport(archive)
            with RunClient(
                settings["control_origin"],
                settings["driver_tokens"][row["binding"]["tenant_id"]],
                timeout=5,
                transport=capture,
            ) as client:
                row["status"] = "submission_attempted"
                save()
                remaining = int((deadline - datetime.now(UTC)).total_seconds())
                if remaining < 240:
                    raise ValueError("S3_WINDOW_TOO_SHORT")
                accepted = client.submit(
                    row["binding"]["ticket_id"],
                    row["binding"]["business_request_key"],
                    plan["profile"]["profile_id"],
                    plan["batch_key"],
                    idempotency_key=row["idempotency_key"],
                    run_timeout_seconds=min(600, remaining),
                )
                row["run_id"], row["status"] = accepted.run.run_id, "accepted"
                save()
                if (
                    accepted.reused
                    or accepted.run.profile_id != plan["profile"]["profile_id"]
                    or accepted.run.profile_hash != plan["profile"]["profile_hash"]
                    or accepted.run.tenant_id != row["binding"]["tenant_id"]
                    or accepted.run.ticket_id != row["binding"]["ticket_id"]
                    or accepted.run.business_request_key
                    != row["binding"]["business_request_key"]
                    or accepted.run.budget_batch_id != plan["batch_key"]
                    or accepted.run.budget.batch.id != plan["batch_account_id"]
                    or accepted.run.budget.batch.limits.cost_microyuan
                    > plan["max_cost_microyuan"]
                ):
                    raise ValueError("SUBMITTED_RUN_BINDING_MISMATCH")
                fault_path = directory / "fault.json"
                if row["boundary"] == "none":
                    fault_path.unlink(missing_ok=True)
                else:
                    atomic_json(
                        fault_path,
                        {
                            "experiment": row["experiment"],
                            "run_id": row["run_id"],
                            "tenant_id": row["binding"]["tenant_id"],
                            "profile_hash": plan["profile"]["profile_hash"],
                            "boundary": row["boundary"],
                        },
                    )
                supervisor.start()
                injected = False
                while True:
                    check()
                    marker = directory / f"{row['experiment']}.barrier.json"
                    if marker.exists() and not injected:
                        barrier = json.loads(marker.read_bytes())
                        if any(
                            barrier[key] != expected
                            for key, expected in {
                                "experiment": row["experiment"],
                                "run_id": row["run_id"],
                                "tenant_id": row["binding"]["tenant_id"],
                                "attempt_no": 1,
                                "profile_hash": plan["profile"]["profile_hash"],
                                "worker_id": plan["workers"][supervisor.index],
                            }.items()
                        ):
                            raise ValueError("FAULT_BINDING_MISMATCH")
                        # Write the original export before replacement, so paid
                        # prefix and an uncommitted old chat cannot disappear.
                        row["fault"] = {**barrier, **supervisor.inject(row, barrier)}
                        injected = True
                        save()
                    current = client.get(row["run_id"])
                    if (
                        current.budget.batch.frozen
                        or current.budget.batch.used.cost_microyuan
                        > plan["max_cost_microyuan"]
                    ):
                        raise ValueError("S3_BATCH_STOPPED")
                    if (
                        injected
                        and supervisor.current is None
                        and current.state.value == "ready"
                    ):
                        evidence, events = export_run(client, capture, row["run_id"])
                        atomic_json(archive / "closed-attempt.json", evidence)
                        atomic_json(archive / "closed-events.json", {"events": events})
                        if not chat_barrier(evidence, committed=row["arm"] == "H0"):
                            raise ValueError("S3_AUDIT_BARRIER_INCOMPLETE")
                        row["ready_at"] = datetime.now(UTC).isoformat()
                        if row["arm"] == "H0":
                            client.cancel(
                                row["run_id"],
                                idempotency_key=f"cancel-{row['idempotency_key']}",
                            )
                        else:
                            fault_path.unlink()
                            supervisor.start()
                            row["replacement_started_at"] = datetime.now(
                                UTC
                            ).isoformat()
                        save()
                    if current.state.value in {
                        "awaiting_approval",
                        "succeeded",
                        "failed",
                        "cancelled",
                    }:
                        break
                    if (
                        supervisor.current is not None
                        and supervisor.current.poll() is not None
                    ):
                        raise ValueError("WORKER_STOPPED_WITHOUT_INJECTION")
                    time.sleep(0.025)
                if supervisor.current is not None:
                    supervisor.stop()
                evidence, events = export_run(client, capture, row["run_id"])
                atomic_json(archive / "evidence.json", evidence)
                atomic_json(archive / "events.json", {"events": events})
                row["status"] = (
                    "finished"
                    if injected or row["boundary"] == "none"
                    else "boundary_not_reached"
                )
                row["finished_at"] = datetime.now(UTC).isoformat()
                save()
                if not chat_barrier(evidence) or row["status"] != "finished":
                    raise ValueError("S3_CASE_INCOMPLETE")
                if (
                    evidence["run"]["state"] not in {"awaiting_approval", "succeeded"}
                    and row["arm"] != "H0"
                ):
                    raise ValueError("S3_CASE_INCOMPLETE")
    except Exception as exc:
        row["error_code"] = category(exc)
        if supervisor.current is not None:
            try:
                supervisor.stop()
            except (OSError, ValueError) as cleanup:
                row["cleanup_error"] = category(cleanup)
        if row["run_id"] is not None:
            try:
                failure_archive = archive / "failure-api"
                failure_archive.mkdir(mode=0o700)
                capture = FailureCapture(failure_archive)
                with RunClient(
                    settings["control_origin"],
                    settings["driver_tokens"][row["binding"]["tenant_id"]],
                    timeout=2,
                    transport=capture,
                ) as client:
                    evidence, events = export_run(client, capture, row["run_id"])
                    atomic_json(archive / "failure-evidence.json", evidence)
                    atomic_json(archive / "failure-events.json", {"events": events})
            except Exception as export_error:
                row["failure_export_error"] = category(export_error)
        save()
        raise
    finally:
        if supervisor.current is not None:
            try:
                supervisor.stop()
            except (OSError, ValueError):
                pass  # The first persisted failure remains authoritative.
        proxy.terminate()
        try:
            proxy.wait(timeout=3)
        except subprocess.TimeoutExpired:
            proxy.kill()
            proxy.wait(timeout=2)


def category(exc: Exception) -> str:
    """Publish fixed error categories without remote messages or private paths."""
    text = str(exc)
    return (
        text
        if isinstance(exc, ValueError)
        and len(text) <= 64
        and text.replace("_", "").isalnum()
        and text.upper() == text
        else type(exc).__name__
    )


def main() -> None:
    """Require a separately issued release binding this exact frozen list."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--plan", type=Path, required=True)
    parser.add_argument("--settings", type=Path, required=True)
    parser.add_argument("--release", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    raw = args.plan.read_bytes()
    release = json.loads(args.release.read_bytes())
    if (
        release.get("execution_list_sha256") != hashlib.sha256(raw).hexdigest()
        or release.get("approved") is not True
    ):
        raise ValueError("S3_LIST_NOT_RELEASED")
    plan, settings = json.loads(raw), json.loads(args.settings.read_bytes())
    verify_runtime(plan, settings, release)

    def stopped(_signal: int, _frame: Any) -> None:
        raise ValueError("OPERATOR_STOP")

    for selected in (signal.SIGINT, signal.SIGTERM):
        signal.signal(selected, stopped)
    launch(plan, settings, args.out)


if __name__ == "__main__":
    main()
