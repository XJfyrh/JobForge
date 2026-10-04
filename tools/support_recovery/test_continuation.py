"""Committed-correction and single-suffix contracts without provider calls."""

from __future__ import annotations

import copy
import json
from pathlib import Path
from typing import Any

import httpx
import pytest
from jobforge import RequestTimeoutError
from jobforge_agent.runtime_input import input_hash

from tools.support_evaluation.evidence import canonical, fingerprint
from tools.support_evaluation.export import CaptureTransport, atomic_json
from tools.support_recovery import continuation, driver
from tools.support_recovery.test_contract import frozen


def correction() -> dict[str, Any]:
    """Create an actual-hash checkpoint with one unique known rejected chat."""
    run: dict[str, Any] = {
        "run_id": "11111111-1111-4111-8111-111111111111",
        "profile_id": "recovery",
        "profile_hash": "a" * 64,
        "snapshot_id": "22222222-2222-4222-8222-222222222222",
        "snapshot_hash": "b" * 64,
        "cursor_version": 1,
    }
    call = {
        "physical_call_id": "33333333-3333-4333-8333-333333333333",
        "step_id": "44444444-4444-4444-8444-444444444444",
        "step_kind": "model_decision",
        "attempt_no": 2,
        "profile_id": run["profile_id"],
        "profile_hash": run["profile_hash"],
        "subcall": "chat",
        "usage_known": True,
        "held_tokens": 0,
        "held_cost_microyuan": 0,
        "measurement_anomaly": False,
        "report_conflict": False,
        "report_hash": "c" * 64,
        "report_recorded_at": "2026-10-04T09:00:00Z",
        "observed_at": "2026-10-04T09:00:00Z",
        "settled_at": "2026-10-04T09:00:00Z",
        "http_status": 200,
        "transport_outcome": "response",
        "business_outcome": "rejected",
        "error_code": "MODEL_PROTOCOL_ERROR",
        "provider_audit": {
            "identity_state": "compatible",
            "mode_state": "nonthinking",
            "response_complete": True,
            "usage_evidence": "complete",
        },
    }
    output = {
        "schema_version": 1,
        "physical_call_id": call["physical_call_id"],
        "tool_invocation_id": "",
        "content": None,
        "proposal": None,
        "evidence_refs": [],
        "correction_required": True,
    }
    raw = json.dumps(output)
    step = {
        "step_id": call["step_id"],
        "sequence": 1,
        "cursor_version": 1,
        "kind": "model_decision",
        "input_hash": input_hash(run["profile_hash"], run["snapshot_hash"], 0, ""),
        "profile_hash": run["profile_hash"],
        "snapshot_hash": run["snapshot_hash"],
        "output_ref": f"run-step:{run['run_id']}:1",
        "output": output,
    }
    step["commit_hash"] = fingerprint(
        "jobforge.run.commit.v1",
        step["step_id"],
        "1",
        step["kind"],
        "0",
        step["input_hash"],
        run["profile_id"],
        run["profile_hash"],
        run["snapshot_id"],
        run["snapshot_hash"],
        canonical(raw),
    )
    return {
        "run": run,
        "calls": {"batch_frozen": False, "batch_stop_code": None, "items": [call]},
        "steps": [{"record": step, "output_json": raw}],
    }


def test_committed_correction_is_accepted_but_uncommitted_rejected_is_not() -> None:
    """The legal correction Commit differs from the new recovery exemption."""
    evidence = correction()
    assert driver.chat_barrier(evidence) and driver.chat_barrier(
        evidence, committed=True
    )
    evidence["steps"] = []
    assert not driver.chat_barrier(evidence)
    call = evidence["calls"]["items"][0]
    call.update(business_outcome="accepted", error_code="")
    assert driver.chat_barrier(evidence)
    assert not driver.chat_barrier(evidence, committed=True)


@pytest.mark.parametrize(
    "wrong",
    [
        "step_id",
        "kind",
        "snapshot",
        "profile",
        "input",
        "hash",
        "raw",
        "proposal",
        "correction",
        "duplicate_call",
        "duplicate_step",
        "unknown",
        "held",
        "conflict",
        "observation",
        "report",
        "identity",
        "mode",
        "incomplete",
        "frozen",
    ],
)
def test_correction_requires_exact_checkpoint_and_complete_known_audit(
    wrong: str,
) -> None:
    """A matching call ID cannot disguise a different binding or incomplete report."""
    evidence = correction()
    call, step = evidence["calls"]["items"][0], evidence["steps"][0]["record"]
    if wrong == "step_id":
        step["step_id"] = evidence["run"]["run_id"]
    elif wrong == "kind":
        step["kind"] = "protocol_correction"
    elif wrong == "snapshot":
        step["snapshot_hash"] = "d" * 64
    elif wrong == "profile":
        call["profile_hash"] = "d" * 64
    elif wrong == "input":
        step["input_hash"] = "d" * 64
    elif wrong == "hash":
        step["commit_hash"] = "d" * 64
    elif wrong == "raw":
        evidence["steps"][0]["output_json"] = "{}"
    elif wrong == "proposal":
        step["output"]["proposal"] = {}
    elif wrong == "correction":
        step["output"]["correction_required"] = False
    elif wrong == "duplicate_call":
        evidence["calls"]["items"].append(copy.deepcopy(call))
    elif wrong == "duplicate_step":
        evidence["steps"].append(copy.deepcopy(evidence["steps"][0]))
    elif wrong == "unknown":
        call["usage_known"] = False
    elif wrong == "held":
        call["held_cost_microyuan"] = 1
    elif wrong == "conflict":
        call["report_conflict"] = True
    elif wrong == "observation":
        call["observed_at"] = None
    elif wrong == "report":
        call["report_recorded_at"] = None
    elif wrong == "identity":
        call["provider_audit"]["identity_state"] = "unknown"
    elif wrong == "mode":
        call["provider_audit"]["mode_state"] = "thinking"
    elif wrong == "incomplete":
        call["provider_audit"]["response_complete"] = False
    else:
        evidence["calls"]["batch_frozen"] = True
    assert not driver.chat_barrier(evidence)


def prior_rows(value: dict[str, Any]) -> list[dict[str, Any]]:
    """Keep the original seven statuses and failure while selecting no new intent."""
    rows = [
        dict(row, status="unattempted", run_id=None, error_code="")
        for row in value["runs"]
    ]
    for index, row in enumerate(rows[:7]):
        row.update(status="finished", run_id=f"prior-{index}")
    rows[6]["error_code"] = "S3_CASE_INCOMPLETE"
    return rows


def test_uncertain_suffix_submit_cannot_be_reclassified_as_unattempted(
    tmp_path: Path,
) -> None:
    """A timed-out POST without a Run ID still permanently blocks this suffix."""
    value = frozen()
    rows = prior_rows(value)
    archive = tmp_path / "exports/run-01"
    archive.mkdir(parents=True)
    atomic_json(archive / "rows.json", {"runs": rows})
    assert continuation.checked_rows(value, tmp_path) == rows
    rows[7]["status"] = "submission_attempted"
    atomic_json(archive / "rows.json", {"runs": rows})
    with pytest.raises(ValueError, match="S3_REMAINING_ALREADY_ATTEMPTED"):
        continuation.checked_rows(value, tmp_path)


@pytest.mark.parametrize(
    "wrong", ["none", "attempt", "open_attempt", "ready", "extra_run"]
)
def test_prior_history_checks_actual_pg_attempt_and_closed_run(wrong: str) -> None:
    """SDK's absent step attempt is checked against the independent PG export."""
    value = frozen()
    value["profile"].update(profile_id="recovery", profile_hash="a" * 64)
    value["batch_key"] = "batch"
    for row in value["runs"]:
        row["binding"]["ticket_id"] = "ticket"
    rows = prior_rows(value)
    evidence = {}
    runs = []
    for row in rows[:7]:
        exported = correction()
        exported["run"].update(
            run_id=row["run_id"],
            state="awaiting_approval",
            tenant_id="tenant-north",
            ticket_id="ticket",
            business_request_key=row["binding"]["business_request_key"],
            budget_batch_id="batch",
        )
        exported["steps"][0]["record"]["output_ref"] = f"run-step:{row['run_id']}:1"
        step = exported["steps"][0]["record"]
        evidence[row["experiment"]] = exported
        runs.append(
            {
                "run_id": row["run_id"],
                "state": "awaiting_approval",
                "tenant_id": "tenant-north",
                "profile_id": "recovery",
                "profile_hash": "a" * 64,
                "attempts": [{"attempt_no": 2, "finished_at": "2026-10-04T09:00:00Z"}],
                "steps": [
                    {
                        "step_id": step["step_id"],
                        "kind": step["kind"],
                        "sequence": 1,
                        "attempt_no": 2,
                    }
                ],
            }
        )
    audit = {"batch_account_id": value["batch_account_id"], "runs": runs}
    if wrong == "attempt":
        runs[6]["steps"][0]["attempt_no"] = 1
    elif wrong == "open_attempt":
        runs[6]["attempts"][0]["finished_at"] = None
    elif wrong == "ready":
        runs[6]["state"] = "ready"
    elif wrong == "extra_run":
        runs.append(dict(runs[6], run_id="unregistered"))
    if wrong == "none":
        continuation.closed_history(value, rows, audit, evidence)
    else:
        with pytest.raises(ValueError, match="S3_"):
            continuation.closed_history(value, rows, audit, evidence)


def test_changed_original_bytes_and_production_source_reject_before_launch(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Original exports and production sources cannot be re-frozen into the suffix."""
    value = frozen()
    atomic_json(tmp_path / "execution-list.json", value)
    original = tmp_path / "evidence.json"
    original.write_bytes(b"immutable response")
    manifest = {
        "schema_version": 1,
        "kind": continuation.KIND,
        "remaining_ordinals": [8, 9, 10, 11],
        "execution_list_sha256": continuation.digest(tmp_path / "execution-list.json"),
        "original_artifacts": {"evidence.json": continuation.digest(original)},
    }
    continuation.verify(manifest, value, tmp_path, check_sources=False)
    original.write_bytes(b"changed response")
    with pytest.raises(ValueError, match="S3_ORIGINAL_EVIDENCE_CHANGED"):
        continuation.verify(manifest, value, tmp_path, check_sources=False)
    value["sources"] = {"internal/run/postgres/provider_audit_guard.go": "old"}
    monkeypatch.setattr(
        continuation,
        "source_hashes",
        lambda: {"internal/run/postgres/provider_audit_guard.go": "new"},
    )
    with pytest.raises(ValueError, match="S3_CONTINUATION_SOURCE_SCOPE"):
        continuation.checked_sources(value)


@pytest.mark.parametrize("wrong", ["account", "known", "held", "freeze"])
def test_ledger_drift_stops_preflight_before_any_sdk_exchange(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    wrong: str,
) -> None:
    """Known exposure cannot be reset or replaced, even with internally valid totals."""
    value = frozen()
    value["profile"].update(profile_id="recovery", profile_hash="a" * 64)
    setup = {
        "matches_config": True,
        "migrations_ready": True,
        "profile": value["profile"],
        "history": {"runs": 7, "business_requests": 7},
        "bindings": [],
        "accounts": [],
    }
    for index, scope in enumerate(("batch", "tenant", "tenant")):
        setup["accounts"].append(
            {
                "scope": scope,
                "account_id": value["batch_account_id"]
                if index == 0
                else f"tenant-{index}",
                "frozen": False,
                "batch_stop_code": "",
                "held_tokens": 0,
                "held_cost_microyuan": 0,
                "known_tokens": 10,
                "known_cost_microyuan": 20,
                "used": {"tokens": 10, "cost_microyuan": 20},
                "limits": {"cost_microyuan": 5_000_000},
            }
        )
    atomic_json(tmp_path / "setup-after.json", setup)
    current = copy.deepcopy(setup)
    if wrong == "account":
        current["accounts"][1]["account_id"] = "replacement"
    elif wrong == "known":
        current["accounts"][0]["known_cost_microyuan"] = 0
        current["accounts"][0]["used"]["cost_microyuan"] = 0
    elif wrong == "held":
        current["accounts"][0]["held_cost_microyuan"] = 1
    else:
        current["accounts"][0]["frozen"] = True
    monkeypatch.setattr(continuation, "verify", lambda *_args, **_kwargs: None)
    with pytest.raises(ValueError, match="S3_CONTINUATION_"):
        continuation.preflight(
            {}, value, {"original_root": str(tmp_path)}, current, tmp_path / "out"
        )


def test_continuation_submits_only_eighth_once_and_preserves_original_seven(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Failure on the first remaining Submit never replays or edits prior rows."""
    value = frozen()
    value["profile"].update(profile_id="test", profile_hash="a" * 64)
    value["batch_key"] = "batch"
    for row in value["runs"]:
        row["binding"]["ticket_id"] = "ticket"
    rows = prior_rows(value)
    output = tmp_path / "out"
    settings = {
        "workers": [{"worker_id": name} for name in value["workers"]],
        "barriers": str(tmp_path / "barriers"),
        "upstream_gateway": "unused",
        "control_origin": "http://control",
        "driver_tokens": {"tenant-north": "test-key"},
    }
    submitted = []

    class Proxy:
        def __init__(self, *_: Any, **__: Any) -> None:
            pass

        def terminate(self) -> None:
            pass

        def wait(self, **_: Any) -> int:
            return 0

    def disconnected(request: httpx.Request) -> httpx.Response:
        submitted.append(json.loads(request.content))
        raise httpx.ReadTimeout("private error")

    monkeypatch.setattr(driver, "require_linux", lambda: None)
    monkeypatch.setattr(driver, "inspect", lambda *_: {})
    monkeypatch.setattr(continuation, "preflight", lambda *_: copy.deepcopy(rows))
    monkeypatch.setattr(driver, "wait_proxy", lambda _: None)
    monkeypatch.setattr(driver.subprocess, "Popen", Proxy)
    monkeypatch.setattr(
        driver,
        "CaptureTransport",
        lambda path: CaptureTransport(path, httpx.MockTransport(disconnected)),
    )
    with pytest.raises(RequestTimeoutError):
        driver.launch(value, settings, output, {"kind": continuation.KIND})
    actual = continuation.read(output / "rows.json")["runs"]
    assert actual[:7] == rows[:7]
    assert actual[7]["status"] == "submission_attempted" and actual[7]["run_id"] is None
    assert actual[8:] == rows[8:]
    assert len(submitted) == 1
    assert (
        submitted[0]["business_request_key"]
        == rows[7]["binding"]["business_request_key"]
    )
    with pytest.raises(FileExistsError):
        driver.launch(value, settings, output, {"kind": continuation.KIND})
    assert len(submitted) == 1


@pytest.mark.parametrize(
    "wrong", ["none", "early", "error", "cancel", "identity", "cursor", "recovery"]
)
def test_waiting_expiry_allows_only_original_deadline_transition(wrong: str) -> None:
    """Natural approval expiry cannot hide a new execution or altered checkpoint."""
    previous = {
        "state": "awaiting_approval",
        "error": None,
        "run_deadline": "2026-10-04T09:00:00Z",
        "updated_at": "2026-10-04T08:59:00Z",
        "run_id": "original",
        "attempt_no": 2,
        "cursor_version": 12,
        "recovery_count": 1,
        "snapshot_hash": "a" * 64,
    }
    current = dict(
        previous,
        state="failed",
        error={"code": "RUN_DEADLINE_EXCEEDED", "message": "RUN_DEADLINE_EXCEEDED"},
        updated_at="2026-10-04T09:01:00Z",
    )
    if wrong == "early":
        current["updated_at"] = "2026-10-04T08:59:59Z"
    elif wrong == "error":
        current["error"] = {
            "code": "MODEL_PROTOCOL_ERROR",
            "message": "MODEL_PROTOCOL_ERROR",
        }
    elif wrong == "cancel":
        current["state"] = "cancelled"
    elif wrong == "identity":
        current["snapshot_hash"] = "b" * 64
    elif wrong == "cursor":
        current["cursor_version"] = 13
    elif wrong == "recovery":
        current["recovery_count"] = 2
    assert continuation.waiting_expiry(previous, current) == (wrong == "none")
