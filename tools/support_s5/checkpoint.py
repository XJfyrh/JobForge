"""Exchange export hashes with the host; never import scores or gold."""

from __future__ import annotations

import json
import time
from pathlib import Path
from typing import Any

from tools.support_evaluation import driver
from tools.support_evaluation.assemble import sha
from tools.support_evaluation.export import atomic_json

GUARD = Path("/var/lib/jobforge/guard")


class CheckpointDriver(driver.Driver):
    """Keep the original one-submit loop and wait for an automatic host check."""

    def __init__(self, *args: Any, **kwargs: Any) -> None:
        """Require an unused channel, so stopped batches cannot be reopened."""
        super().__init__(*args, **kwargs)
        if not GUARD.is_dir() or any(GUARD.iterdir()):
            raise driver.BatchStopped("GUARD_NOT_FRESH")
        self.guarded_case: str | None = None

    def save(self) -> None:
        """Publish each terminal export once; releases bind its exact bytes."""
        super().save()
        completed = [row for row in self.rows if row["status"] == "finished"]
        if not completed or completed[-1]["case_id"] == self.guarded_case:
            return
        last = completed[-1]
        self.guarded_case = last["case_id"]
        ordinal = len(completed)
        evidence = self.output / last["case_id"] / "evidence.json"
        ready_path = GUARD / f"{ordinal:02d}-ready.json"
        if ready_path.exists():
            raise driver.BatchStopped("GUARD_ALREADY_ATTEMPTED")
        ready = {
            "ordinal": ordinal,
            "case_id": last["case_id"],
            "run_id": last["run_id"],
            "rows_sha256": sha(self.state_path.read_bytes()),
            "evidence_sha256": sha(evidence.read_bytes()),
            "profile_hash": self.manifest["profile_hash"],
            "batch_account_id": self.manifest["batch_account_id"],
            "launch_sha256": sha((driver.CONFIG / "launch.json").read_bytes()),
        }
        atomic_json(ready_path, ready)
        expected = {
            **ready,
            "ready_sha256": sha(ready_path.read_bytes()),
            "decision": "continue",
        }
        release_path = GUARD / f"{ordinal:02d}-continue.json"
        while True:
            self.check()
            if (GUARD / "stop.json").exists():
                raise driver.BatchStopped("HEALTH_CHECK_STOP")
            if release_path.exists():
                if json.loads(release_path.read_bytes()) != expected:
                    raise driver.BatchStopped("GUARD_RELEASE_MISMATCH")
                if (
                    sha(self.state_path.read_bytes()) != ready["rows_sha256"]
                    or sha(evidence.read_bytes()) != ready["evidence_sha256"]
                ):
                    raise driver.BatchStopped("GUARD_EXPORT_CHANGED")
                return
            time.sleep(0.1)
