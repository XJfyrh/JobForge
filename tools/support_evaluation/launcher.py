"""Fixed Linux parent for one Worker and SDK driver; never a Run scheduler."""

from __future__ import annotations

import hashlib
import json
import os
import signal
import subprocess
import sys
import time
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from tools.support_evaluation.driver import CONFIG, instant
from tools.support_evaluation.export import atomic_json

STATE = Path("/var/lib/jobforge/batches")
WORKER = "/usr/local/bin/agent-worker"
CONTROL = "/usr/local/bin/agent-control"


def binding(manifest: dict[str, Any], raw: bytes) -> dict[str, str]:
    """Bind preparation to the exact launch file and original batch identity."""
    return {
        **{
            key: manifest[key]
            for key in ("batch_account_id", "worker_id", "profile_hash")
        },
        "launch_sha256": hashlib.sha256(raw).hexdigest(),
    }


def validate_setup(receipt: dict[str, Any], manifest: dict[str, Any]) -> None:
    """Use the actual setup snapshot only as preflight, never a call permit."""
    if not receipt["matches_config"] or not receipt["migrations_ready"]:
        raise ValueError("SETUP_MISMATCH")
    if any(receipt["history"].values()):
        raise ValueError("BATCH_HAS_HISTORY")
    now = instant(receipt["sampled_at"])
    if not instant(manifest["valid_from"]) <= now < instant(manifest["valid_until"]):
        raise ValueError("BATCH_OUTSIDE_WINDOW")
    if (instant(manifest["valid_until"]) - now).total_seconds() < 120:
        raise ValueError("BATCH_DEADLINE")
    for account in receipt["accounts"]:
        if (
            account["frozen"]
            or any(account["used"].values())
            or account["held_tokens"]
            or account["held_cost_microyuan"]
        ):
            raise ValueError("BATCH_ALREADY_USED")


def prepare_state(manifest: dict[str, Any], raw: bytes) -> None:
    """Create the one batch directory during explicit deployment preparation."""
    directory = STATE / manifest["batch_account_id"]
    directory.mkdir(mode=0o700)
    atomic_json(directory / "prepared.json", binding(manifest, raw))
    (directory / "lock").touch(mode=0o600, exist_ok=False)


def child_environment(worker: bool) -> dict[str, str]:
    """Pass control credentials/config only to the fixed Go process."""
    environment = {
        "PATH": "/usr/local/bin:/usr/bin:/bin",
        "LANG": "C.UTF-8",
        "PYTHONDONTWRITEBYTECODE": "1",
    }
    if worker:
        for key in (
            "JOBFORGE_AGENT_WORKER_CONFIG",
            "JOBFORGE_AGENT_WORKER_CREDENTIALS_FILE",
            "JOBFORGE_AGENT_GATEWAY",
            "JOBFORGE_AGENT_GRPC_TLS",
        ):
            if key in os.environ:
                environment[key] = os.environ[key]
    return environment


def finish(process: subprocess.Popen[bytes]) -> bool:
    """Allow Worker cleanup, then kill and Wait; no automatic replacement."""
    if process.poll() is None:
        process.terminate()
    try:
        process.wait(timeout=3)
        return True
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=2)
        return False


def launch(manifest: dict[str, Any], raw: bytes) -> int:
    """Hold one local startup lock while owning both fixed child lifecycles."""
    import fcntl

    directory = STATE / manifest["batch_account_id"]
    prepared = json.loads((directory / "prepared.json").read_text(encoding="utf-8"))
    if prepared != binding(manifest, raw):
        raise ValueError("PREPARED_BINDING_MISMATCH")
    with (directory / "lock").open("r+b") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if (directory / "attempted.json").exists():
            raise ValueError("BATCH_ALREADY_ATTEMPTED")
        for name, digest in manifest["config_sha256"].items():
            if hashlib.sha256((CONFIG / name).read_bytes()).hexdigest() != digest:
                raise ValueError("CONFIG_MISMATCH")
        inspected = subprocess.run(
            [CONTROL, "inspect-support"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            check=True,
            timeout=10,
        )
        receipt = json.loads(inspected.stdout)
        validate_setup(receipt, manifest)
        atomic_json(directory / "setup.json", receipt)
        atomic_json(
            directory / "attempted.json",
            {"started_at": datetime.now(UTC).isoformat(), **prepared},
        )
        stopping = False

        def stop(*_: Any) -> None:
            nonlocal stopping
            stopping = True

        signal.signal(signal.SIGTERM, stop)
        signal.signal(signal.SIGINT, stop)
        children: list[subprocess.Popen[bytes]] = []
        successful = False
        clean = True
        detected = datetime.now(UTC).isoformat()
        try:
            worker = subprocess.Popen(
                [WORKER],
                env=child_environment(True),
                stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
            children.append(worker)
            driver = subprocess.Popen(
                [sys.executable, "-m", "tools.support_evaluation.driver", "launch"],
                env=child_environment(False),
                stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
            children.append(driver)
            while not stopping:
                if worker.poll() is not None:
                    break
                if driver.poll() is not None:
                    successful = driver.returncode == 0
                    break
                if datetime.now(UTC) >= instant(manifest["valid_until"]):
                    break
                time.sleep(0.05)
            detected = datetime.now(UTC).isoformat()
        finally:
            # Signal both before waiting, so a dead driver cannot leave a
            # Worker claiming while we wait for another process to terminate.
            for child in children:
                if child.poll() is None:
                    child.terminate()
            for child in children:
                clean = finish(child) and clean
            atomic_json(
                directory / "stopped.json",
                {
                    "detected_at": detected,
                    "wait_completed_at": datetime.now(UTC).isoformat(),
                    "children_reaped": all(
                        child.poll() is not None for child in children
                    ),
                    "graceful": clean,
                    "driver_completed": successful,
                },
            )
        return 0 if successful and clean else 1


def main() -> int:
    """Use only installed fixed commands and paths; launch is Linux-only."""
    try:
        raw = (CONFIG / "launch.json").read_bytes()
        manifest = json.loads(raw)
        if sys.argv[1:] == ["prepare-state"]:
            prepare_state(manifest, raw)
            return 0
        if sys.argv[1:]:
            raise ValueError("INVALID_COMMAND")
        return launch(manifest, raw)
    except (OSError, ValueError, KeyError, subprocess.SubprocessError):
        print("support batch stopped; inspect private evidence", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
