"""Freeze the approved observation amendment without repeating the settled prefix."""

from __future__ import annotations

import argparse
import json
import shutil
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from tools.support_diagnosis.prepare import digest, read, save
from tools.support_diagnosis.score import ledger_calls


def prepare(parent: Path, ledger: Path, output: Path, runner: Path) -> dict[str, Any]:
    """Copy exact input/evidence bytes; keep the original stop and financial base."""
    manifest = read(parent / "manifest.json")
    parent_hash = digest((parent / "manifest.json").read_bytes())
    snapshot = read(ledger)
    records = ledger_calls(manifest, parent_hash, snapshot)
    if (
        manifest["schema_version"] != 1
        or manifest.get("parent")
        or snapshot["stopped_reason"] != "thinking_not_observed"
        or len(records) != 15
    ):
        raise ValueError("not the approved stopped prefix")
    for trial in manifest["trials"][:15]:
        if records[trial["trial_id"]]["state"] != "known":
            raise ValueError("unresolved parent call")
    output.mkdir(exist_ok=False)
    for folder in ("requests", "score-inputs", "responses", "reports"):
        shutil.copytree(parent / folder, output / folder)
    shutil.copyfile(parent / "manifest.json", output / "parent-manifest.json")
    manifest["parent"] = {
        "experiment_id": manifest["experiment_id"],
        "manifest_sha256": parent_hash,
        "completed_trials": 15,
    }
    manifest.update(
        schema_version=2,
        experiment_id="s5-final-diagnosis-20261011-v2",
        created_at=datetime.now(UTC).isoformat(),
        runner_sha256=digest(runner.read_bytes()),
    )
    save(output / "manifest.json", manifest)
    save(
        output / "continuation-verification.json",
        {
            "parent_manifest_sha256": parent_hash,
            "parent_ledger_sha256": digest(ledger.read_bytes()),
            "retained_observations": 15,
            "remaining_calls": 225,
            "input_changes": 0,
            "paid_calls": 0,
        },
    )
    return {
        "manifest_sha256": digest((output / "manifest.json").read_bytes()),
        "retained_observations": 15,
        "remaining_calls": 225,
    }


def main() -> None:
    """Prepare only; the Go runner independently checks the parent in PG."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--parent", type=Path, required=True)
    parser.add_argument("--ledger", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--runner", type=Path, required=True)
    args = parser.parse_args()
    print(json.dumps(prepare(args.parent, args.ledger, args.output, args.runner)))


if __name__ == "__main__":
    main()
