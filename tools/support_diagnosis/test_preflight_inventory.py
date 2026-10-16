"""Historical ledger reads must use the caller's explicit database inventory."""

from __future__ import annotations

import json
from pathlib import Path
from types import SimpleNamespace

import pytest

from tools.support_diagnosis import preflight


def test_prior_inventory_selects_hosts_and_rejects_missing_database(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """No old container-name default may redirect a migrated evidence database."""
    folder = tmp_path / "final-20261008"
    folder.mkdir()
    plans = [{"phase": str(i), "database": f"synthetic_{i}"} for i in range(20)]
    (folder / "cumulative-stage-ledger.json").write_text(json.dumps({"phases": plans}))
    inventory = tmp_path / "inventory.json"
    hosts = {
        p["database"]: {"container": "migrated-host", "user": "test-reader"}
        for p in plans
    }
    inventory.write_text(json.dumps(hosts))
    calls = []

    def command(args: list[str], **kwargs: object) -> SimpleNamespace:
        calls.append(args)
        return SimpleNamespace(
            stdout=json.dumps(
                dict.fromkeys(
                    (
                        "runs",
                        "chat_calls",
                        "unknown_chat_calls",
                        "inflight",
                        "unfinished_attempts",
                        "known",
                        "held",
                    ),
                    0,
                )
            )
        )

    monkeypatch.setattr(preflight.subprocess, "run", command)
    assert preflight.old_ledgers(tmp_path, inventory)["totals"]["held"] == 0
    assert len(calls) == 20
    assert all(
        args[3] == "migrated-host" and args[args.index("-U") + 1] == "test-reader"
        for args in calls
    )
    del hosts["synthetic_0"]
    inventory.write_text(json.dumps(hosts))
    calls.clear()
    with pytest.raises(ValueError, match="inventory mismatch"):
        preflight.old_ledgers(tmp_path, inventory)
    assert not calls
