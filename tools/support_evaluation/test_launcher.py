"""Real Linux process lifecycle tests; synthetic children never call services."""

from __future__ import annotations

import json
import os
import signal
import sys
from datetime import UTC, datetime, timedelta
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest

from tools.support_evaluation import launcher

pytestmark = pytest.mark.skipif(sys.platform != "linux", reason="real Linux processes")


def _script(path: Path, body: str) -> None:
    path.write_text(f"#!{sys.executable}\n{body}", encoding="utf-8", newline="\n")
    path.chmod(0o700)


def _install(
    root: Path, monkeypatch: pytest.MonkeyPatch, *, driver_mode: str, ignore_term: bool
) -> tuple[dict[str, Any], bytes]:
    """Use real executable children and a synthetic preflight receipt only."""
    root.mkdir(exist_ok=True)
    state = root / "batches"
    state.mkdir()
    worker, driver, control = (root / name for name in ("worker", "driver", "control"))
    worker_pid, driver_pid = root / "worker.pid", root / "driver.pid"
    term_seen = root / "worker.term"
    now = datetime.now(UTC)
    manifest = {
        "batch_account_id": "00000000-0000-4000-8000-000000000001",
        "worker_id": "launcher-lifecycle-test",
        "profile_hash": "a" * 64,
        "valid_from": (now - timedelta(minutes=1)).isoformat(),
        "valid_until": (now + timedelta(minutes=10)).isoformat(),
        "config_sha256": {},
    }
    receipt = {
        "matches_config": True,
        "migrations_ready": True,
        "sampled_at": now.isoformat(),
        "history": {"runs": 0, "sessions": 0},
        "accounts": [],
    }
    _script(
        control,
        "from pathlib import Path\n"
        f"with Path({str(root / 'inspections')!r}).open('a') as target:\n"
        "    target.write('inspect\\n')\n"
        f"print({json.dumps(receipt)!r})\n",
    )
    _script(
        worker,
        "import os, signal\nfrom pathlib import Path\n"
        "def stop(*args):\n"
        f"    Path({str(term_seen)!r}).write_text('SIGTERM')\n"
        "    raise SystemExit(0)\n"
        f"signal.signal(signal.SIGTERM, {'signal.SIG_IGN' if ignore_term else 'stop'})\n"
        f"with Path({str(worker_pid)!r}).open('a') as target:\n"
        "    target.write(str(os.getpid()) + '\\n')\n"
        "while True:\n    signal.pause()\n",
    )
    action = (
        "while True:\n    signal.pause()\n"
        if driver_mode == "linger"
        else "os.kill(os.getpid(), signal.SIGKILL)\n"
        if driver_mode == "killed"
        else "raise SystemExit(0)\n"
    )
    _script(
        driver,
        "import os, signal, time\nfrom pathlib import Path\n"
        "deadline = time.monotonic() + 5\n"
        f"while not Path({str(worker_pid)!r}).exists():\n"
        "    if time.monotonic() >= deadline:\n        raise SystemExit(2)\n"
        "    time.sleep(0.01)\n"
        f"with Path({str(driver_pid)!r}).open('a') as target:\n"
        "    target.write(str(os.getpid()) + '\\n')\n" + action,
    )
    monkeypatch.setattr(launcher, "STATE", state)
    monkeypatch.setattr(launcher, "WORKER", str(worker))
    monkeypatch.setattr(launcher, "CONTROL", str(control))
    monkeypatch.setattr(launcher, "sys", SimpleNamespace(executable=str(driver)))
    raw = json.dumps(manifest, sort_keys=True).encode()
    launcher.prepare_state(manifest, raw)
    return manifest, raw


@pytest.mark.parametrize(
    "driver_mode,ignore_term,exit_code",
    [("completed", False, 0), ("killed", False, 1), ("killed", True, 1)],
)
def test_driver_exit_reaps_worker_and_attempted_batch_never_restarts(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    driver_mode: str,
    ignore_term: bool,
    exit_code: int,
) -> None:
    """Observe real terminate/Kill/Wait and then reject a second launch attempt."""
    manifest, raw = _install(
        tmp_path, monkeypatch, driver_mode=driver_mode, ignore_term=ignore_term
    )
    handlers = {sig: signal.getsignal(sig) for sig in (signal.SIGTERM, signal.SIGINT)}
    try:
        assert launcher.launch(manifest, raw) == exit_code
        for filename in ("worker.pid", "driver.pid"):
            pids = (tmp_path / filename).read_text().splitlines()
            assert len(pids) == 1
            pid = int(pids[0])
            assert not Path(f"/proc/{pid}").exists()
            with pytest.raises(ChildProcessError):
                os.waitpid(pid, os.WNOHANG)
        directory = launcher.STATE / manifest["batch_account_id"]
        stopped = json.loads((directory / "stopped.json").read_text())
        assert stopped["children_reaped"] is True
        assert stopped["graceful"] is not ignore_term
        assert stopped["driver_completed"] == (driver_mode == "completed")
        assert stopped["stop_reason"] == "driver_exited"
        assert stopped["driver_returncode"] == (
            0 if driver_mode == "completed" else -signal.SIGKILL
        )
        assert stopped["worker_returncode"] == (-signal.SIGKILL if ignore_term else 0)
        assert (tmp_path / "worker.term").exists() is not ignore_term
        assert datetime.fromisoformat(stopped["detected_at"]) <= datetime.fromisoformat(
            stopped["wait_completed_at"]
        )
        before = {
            name: (tmp_path / name).read_bytes()
            for name in ("worker.pid", "driver.pid", "inspections")
        }
        attempted = (directory / "attempted.json").read_bytes()
        with pytest.raises(ValueError, match="BATCH_ALREADY_ATTEMPTED"):
            launcher.launch(manifest, raw)
        assert (directory / "attempted.json").read_bytes() == attempted
        assert all(
            (tmp_path / name).read_bytes() == data for name, data in before.items()
        )
    finally:
        for sig, handler in handlers.items():
            signal.signal(sig, handler)


def test_worker_exit_retains_diagnostic_and_stops_driver(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    capfd: pytest.CaptureFixture[str],
) -> None:
    """A real failed child leaves its reason and exit status for diagnosis."""
    manifest, raw = _install(
        tmp_path, monkeypatch, driver_mode="linger", ignore_term=False
    )
    _script(
        Path(launcher.WORKER),
        "import sys\n"
        "print('agent worker stopped reason=CLEANUP_UNCONFIRMED', file=sys.stderr)\n"
        "raise SystemExit(1)\n",
    )
    handlers = {sig: signal.getsignal(sig) for sig in (signal.SIGTERM, signal.SIGINT)}
    try:
        assert launcher.launch(manifest, raw) == 1
        stopped = json.loads(
            (launcher.STATE / manifest["batch_account_id"] / "stopped.json").read_text()
        )
        assert stopped["stop_reason"] == "worker_exited"
        assert stopped["worker_returncode"] == 1
        assert stopped["driver_returncode"] == -signal.SIGTERM
        assert stopped["children_reaped"] is True
        assert "reason=CLEANUP_UNCONFIRMED" in capfd.readouterr().err
    finally:
        for sig, handler in handlers.items():
            signal.signal(sig, handler)


def container_probe(root: Path) -> None:
    """External Docker probe entry: run the real launcher as init's main child.

    Both helpers wait without network/PG activity. The host kills this launcher
    PID and observes the actual container exit; this is not a billing test.
    """
    with pytest.MonkeyPatch.context() as monkeypatch:
        manifest, raw = _install(
            root, monkeypatch, driver_mode="linger", ignore_term=False
        )
        (root / "launcher.pid").write_text(str(os.getpid()), encoding="ascii")
        launcher.launch(manifest, raw)
