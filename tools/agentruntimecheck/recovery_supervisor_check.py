"""Exercise the external supervisor against an actual Go Worker and RPC proxy."""

from __future__ import annotations

import json
import socket
import subprocess
import sys
from pathlib import Path

from tools.support_evaluation.export import atomic_json
from tools.support_recovery.driver import Supervisor, category, wait_fact


def main() -> None:
    """Publish only process/boundary facts; test provider/data remain synthetic."""
    config = json.loads(Path(sys.argv[1]).read_bytes())
    directory = Path(config["barriers"])
    supervisor = Supervisor(config, directory)
    proxy = subprocess.Popen(
        [
            "/usr/local/bin/supportrecoveryproxy",
            "--upstream",
            config["upstream_gateway"],
            "--directory",
            str(directory),
        ],
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )

    def listening() -> bool:
        try:
            with socket.create_connection(("127.0.0.1", 8095), timeout=0.1):
                return proxy.poll() is None
        except OSError:
            return False

    try:
        wait_fact(listening, 3)
        supervisor.start()
        row = config["row"]
        marker = directory / f"{row['experiment']}.barrier.json"
        wait_fact(marker.is_file, 20)
        barrier = json.loads(marker.read_bytes())
        fault = json.loads((directory / "fault.json").read_bytes())
        if (
            any(
                barrier[key] != fault[key]
                for key in ("experiment", "run_id", "tenant_id", "profile_hash")
            )
            or barrier["attempt_no"] != 1
        ):
            raise ValueError("FAULT_BINDING_MISMATCH")
        facts = supervisor.inject(row, barrier)
        atomic_json(Path(config["result"]), {"passed": True, **barrier, **facts})
    except Exception as exc:
        atomic_json(
            Path(config["result"]), {"passed": False, "error_code": category(exc)}
        )
        raise SystemExit(1) from None
    finally:
        if supervisor.current is not None:
            supervisor.stop()
        proxy.terminate()
        proxy.wait(timeout=3)


if __name__ == "__main__":
    main()
