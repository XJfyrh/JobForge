"""Export joins and provenance completeness using explicit synthetic evidence."""

from __future__ import annotations

import copy
import json
from pathlib import Path
from typing import Any

import pytest

from tools.support_evaluation.assemble import (
    FACT_TABLES,
    assemble,
    outbound,
    register,
    safety,
    save_new,
    sha,
)
from tools.support_evaluation.evidence import load_package
from tools.support_evaluation.export import json_bytes
from tools.support_evaluation.fixtures import case_row, registered

ROOT = Path(__file__).resolve().parents[2]
PACKAGE = load_package()


def sample(time: str) -> dict[str, Any]:
    """Use synthetic hashes only in the test, never in actual assembly."""
    return {
        "database": "test",
        "role": "jobforge_business_reader",
        "observed_at": time,
        "facts": {name: {"rows": 1, "sha256": "a" * 64} for name in FACT_TABLES},
        "write_privileges": {name: False for name in FACT_TABLES},
    }


def test_actual_registration_reads_go_profile_and_deployment_keys(
    tmp_path: Path,
) -> None:
    """The Go account_id/scope shape and exact 40 bindings reach the scorer."""
    profile = json.loads((ROOT / "api/support/profile-v1/fixtures.json").read_bytes())[
        "profile"
    ]
    base = registered(PACKAGE)
    limits = base["bindings"][0]["budget_limits"]
    control = {
        "profiles": [profile],
        "budgets": [
            {"account_id": scope, "limits": limits[scope]}
            for scope in ("tenant", "batch")
        ],
        "bindings": [
            {"tenant_id": tenant, "tenant_account_id": "tenant"}
            for tenant in ("tenant-north", "tenant-south")
        ],
    }
    worker = {
        "tenants": {
            "tenant-north": {
                "business_origin": "http://business:8092",
                "ollama_origin": "http://ollama:11434",
            }
        }
    }
    config_hashes = {}
    for name, value in (("control.disabled.json", control), ("worker.json", worker)):
        save_new(tmp_path / name, value)
        config_hashes[name] = sha((tmp_path / name).read_bytes())
    save_new(
        tmp_path / "launch.json",
        {
            "config_sha256": config_hashes,
            "batch_key": "unit-test-batch",
            "batch_account_id": "batch",
            "cases": base["bindings"],
        },
    )
    actual = register(tmp_path)
    assert actual["profile"]["profile_hash"] == profile["profile_hash"]
    assert actual["bindings"][0]["budget_limits"]["family"]["chat"] == 12
    assert len(actual["bindings"]) == 40
    with pytest.raises(FileExistsError):
        save_new(tmp_path / "launch.json", {})


def test_partial_or_duplicate_send_never_becomes_complete(tmp_path: Path) -> None:
    """Keep real send count; missing finish is not successful observation."""
    row = case_row(PACKAGE, registered(PACKAGE), 0)
    request = row["safety"]["requests"][-1]
    events = [
        {
            **request,
            "schema_version": 1,
            "event": event,
            "time": "2026-09-16T12:02:00Z",
            "http_status": None if i == 0 else 200,
            "response_complete": i == 2,
        }
        for i, event in enumerate(("dispatch_attempt", "http_response", "finish"))
    ]
    before, after = sample("2026-09-16T12:01:00Z"), sample("2026-09-16T12:03:00Z")
    valid = safety(events, "b" * 64, before, after, "c" * 64, True)
    assert valid["complete"] and len(valid["requests"]) == 1
    assert not safety(events[:2], "b" * 64, before, after, "c" * 64, True)["complete"]
    repeated = safety(events * 2, "b" * 64, before, after, "c" * 64, True)
    assert not repeated["complete"] and len(repeated["requests"]) == 2
    (tmp_path / "call.jsonl").write_bytes(
        b"".join(json.dumps(event).encode() + b"\n" for event in events) + b"{partial"
    )
    grouped, _, complete = outbound(tmp_path)
    assert not complete and len(grouped[request["snapshot_id"]]) == 3
    after["facts"]["tickets"]["sha256"] = "d" * 64
    assert safety(events, "b" * 64, before, after, "c" * 64, False)["writes"]


def test_assembly_retains_unknown_submission_and_all_unattempted(
    tmp_path: Path,
) -> None:
    """The offline join neither invents a Run nor drops unfinished cases."""
    registration_file = tmp_path / "registration.json"
    base = registered(PACKAGE)
    save_new(registration_file, base)
    rows = [
        {**copy.deepcopy(row), "status": "unattempted", "run_id": None}
        for row in base["bindings"]
    ]
    rows[0]["status"] = "submission_attempted"
    save_new(tmp_path / "rows.json", {"cases": rows})
    before, after = tmp_path / "before.json", tmp_path / "after.json"
    save_new(before, sample("2026-09-16T12:01:00Z"))
    save_new(after, sample("2026-09-16T12:03:00Z"))
    actual = assemble(registration_file, tmp_path, tmp_path, before, after)
    assert len(actual["cases"]) == 40
    assert actual["cases"][0]["status"] == "submission_failed"
    assert all(row["status"] == "unattempted" for row in actual["cases"][1:])
    assert actual["registration_sha256"] == sha(json_bytes(base))
    rows[0]["run_id"] = "accepted-but-not-exported"
    (tmp_path / "rows.json").write_bytes(json_bytes({"cases": rows}))
    with pytest.raises(FileNotFoundError):
        assemble(registration_file, tmp_path, tmp_path, before, after)
