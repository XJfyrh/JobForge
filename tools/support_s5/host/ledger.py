"""Refresh an explicit database inventory, with no imports from old versions."""

from __future__ import annotations

import json
import subprocess
from datetime import UTC, datetime
from typing import Any

COUNTS = (
    "runs",
    "chat_calls",
    "unknown_chat_calls",
    "inflight",
    "unfinished_attempts",
    "known",
    "held",
)


def query(database: dict[str, Any]) -> dict[str, Any]:
    """Use a read-only repeatable snapshot; unreadable accounts fail closed."""
    diagnosis = database.get("diagnosis", False)
    calls, status = (
        ("diagnosis_calls", "state") if diagnosis else ("physical_calls", "status")
    )
    chat = "true" if diagnosis else "kind='chat'"
    sql = f"""begin transaction isolation level repeatable read read only;
select json_build_object('database',current_database(),'sampled_at',clock_timestamp(),
 'runs',(select count(*) from runs),
 'chat_calls',(select count(*) from {calls} where {chat}),
 'unknown_chat_calls',(select count(*) from {calls} where {status}='unknown' and {chat}),
 'inflight',(select count(*) from {calls} where {status}='reserved'),
 'unfinished_attempts',(select count(*) from run_attempts where finished_at is null),
 'known',(select coalesce(sum(known_cost_microyuan),0) from budget_accounts where scope='batch'),
 'held',(select coalesce(sum(held_cost_microyuan),0) from budget_accounts where scope='batch'));
commit;"""
    result = subprocess.run(
        [
            "docker",
            "exec",
            "-i",
            database["container"],
            "psql",
            "-X",
            "-q",
            "-At",
            "-v",
            "ON_ERROR_STOP=1",
            "-U",
            database["user"],
            "-d",
            database["database"],
        ],
        input=sql,
        text=True,
        encoding="utf-8",
        capture_output=True,
        check=True,
        timeout=30,
    )
    value: dict[str, Any] = json.loads(result.stdout)
    if value["database"] != database["database"] or any(
        type(value[k]) is not int or value[k] < 0 for k in COUNTS
    ):
        raise ValueError("LEDGER_BINDING")
    return {**database, **value}


def collect(inventory: list[dict[str, Any]]) -> dict[str, Any]:
    """Keep every registered database, including stopped and diagnostic batches."""
    identities = [(p["container"], p["database"]) for p in inventory]
    if not inventory or len(set(identities)) != len(identities):
        raise ValueError("LEDGER_INVENTORY")
    phases = [query(p) for p in inventory]
    return {
        "recorded_at": datetime.now(UTC).isoformat(),
        "phases": phases,
        "totals": {key: sum(p[key] for p in phases) for key in COUNTS},
        "cost_basis": "observed_usage_tariff_estimate_not_settled_invoice",
    }


def validate_history(previous: dict[str, Any], current: dict[str, Any]) -> None:
    """Known cost cannot disappear when an old inventory is made explicit."""
    actual = {p["database"]: p for p in current["phases"]}
    for old in previous["phases"]:
        new = actual.get(old["database"])
        if new is None or any(
            old.get(k, new[k]) != new[k] for k in ("container", "user")
        ):
            raise ValueError("HISTORICAL_DATABASE_IDENTITY")
        # Full holds may settle to smaller observed usage; known cost cannot reset.
        if new["known"] < old["known"]:
            raise ValueError("HISTORICAL_KNOWN_COST_DECREASED")


def validate_stage(
    head: dict[str, Any], current: dict[str, Any], new_database: str
) -> None:
    """A second preprepared batch cannot omit a batch launched after its review."""
    if new_database in {p["database"] for p in head["phases"]}:
        raise ValueError("STAGE_DATABASE_ALREADY_REGISTERED")
    validate_history(head, current)
