"""Offline S3 scope and stopping contracts; no provider or business service."""

from __future__ import annotations

import copy
import hashlib
import json
import subprocess
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

import httpx
import pytest
from jobforge import RequestTimeoutError

from tools.support_evaluation.export import CaptureTransport, ExportError
from tools.support_recovery import driver, plan


def frozen() -> dict[str, Any]:
    """Use synthetic bindings for the exact reviewed eleven intentions."""
    batch = "11111111-1111-4111-8111-111111111111"
    now = datetime.now(UTC)
    return {
        "schema_version": 1,
        "max_runs": 11,
        "max_cost_microyuan": 5_000_000,
        "max_seconds": 21600,
        "workers": ["worker-one", "worker-two"],
        "profile": {"executor_version": "linux-v2-recovery-runtime-1"},
        "definition": {
            "schema_version": 3,
            "program": {"recovery_policy": "confirmed_uncommitted_v1"},
        },
        "batch_account_id": batch,
        "valid_from": now.isoformat(),
        "valid_until": (now + timedelta(hours=1)).isoformat(),
        "runs": [
            {
                "ordinal": i,
                "experiment": f"{case}-{arm}",
                "case_id": case,
                "arm": arm,
                "boundary": boundary,
                "target": target,
                "idempotency_key": f"submit-s3-{batch}-{i:02d}",
                "binding": {
                    "tenant_id": "tenant-north",
                    "business_request_key": f"s3-{batch}-{i:02d}",
                },
            }
            for i, (case, arm, boundary, target) in enumerate(plan.SPECS, 1)
        ],
    }


@pytest.mark.parametrize(
    "change", ["omit", "repeat", "replace", "identity", "legacy", "budget", "window"]
)
def test_frozen_scope_rejects_replacement_or_extra_spending(change: str) -> None:
    """The independent entry point never expands the original fixed list."""
    value = frozen()
    plan.validate(value, check_sources=False)
    if change == "omit":
        value["runs"].pop()
    elif change == "repeat":
        value["runs"].append(value["runs"][0])
    elif change == "replace":
        value["runs"][0]["case_id"] = "DEV-001"
    elif change == "identity":
        value["runs"][0]["binding"]["business_request_key"] = "retry-another"
    elif change == "legacy":
        value["definition"]["schema_version"] = 2
    elif change == "budget":
        value["max_cost_microyuan"] += 1
    else:
        value["valid_until"] = value["valid_from"]
    with pytest.raises(ValueError, match="S3_"):
        plan.validate(value, check_sources=False)


def test_source_check_rejects_changed_reviewed_runtime(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """A separate preflight must bind source, beyond a matching release filename."""
    value = frozen()
    value.update(sources={"runtime": "old"}, package_sha256={})
    monkeypatch.setattr(plan, "source_hashes", lambda: {"runtime": "new"})
    with pytest.raises(ValueError, match="S3_PLAN_CHANGED"):
        plan.validate(value)


def test_replacement_config_preserves_original_preparation(tmp_path: Path) -> None:
    """Only the S3 copy adds a second principal; old restart policy is untouched."""
    original: dict[str, Any] = {
        "workers": [{"worker_id": "first", "capacity": 1}],
        "other": 9,
    }
    path = tmp_path / "control.enabled.json"
    raw = json.dumps(original).encode()
    path.write_bytes(raw)
    replacement = plan.replacement_config(tmp_path, "second", "enabled")
    assert replacement["workers"] == [
        original["workers"][0],
        {"worker_id": "second", "capacity": 1},
    ]
    assert path.read_bytes() == raw


def test_failure_capture_has_one_total_deadline_and_no_exchange_after_it(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Failure export is bounded and cannot replace earlier response archives."""
    elapsed = [10.0]
    monkeypatch.setattr(driver.time, "monotonic", lambda: elapsed[0])
    capture = driver.FailureCapture(tmp_path)
    sent: list[httpx.Request] = []

    def exchange(req: httpx.Request) -> httpx.Response:
        sent.append(req)
        return httpx.Response(200, json={"ok": True})

    capture.inner = httpx.MockTransport(exchange)
    elapsed[0] = 19.5
    capture.handle_request(httpx.Request("GET", "http://control/runs"))
    assert sent[0].extensions["timeout"]["read"] == 0.5
    elapsed[0] = 20.0
    with pytest.raises(ExportError, match="FAILURE_EXPORT_DEADLINE"):
        capture.handle_request(httpx.Request("GET", "http://control/runs"))
    assert len(sent) == 1 and len(list(tmp_path.glob("*.receipt.json"))) == 1


@pytest.mark.parametrize("accepted_then_fail", [False, True])
def test_launch_preserves_single_submit_and_all_unattempted_rows(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, accepted_then_fail: bool
) -> None:
    """Unknown Submit is never retried; accepted failure exports after Worker Wait."""
    value = frozen()
    value["profile"].update(profile_id="test", profile_hash="a" * 64)
    value.update(batch_key="batch", max_cost_microyuan=5_000_000)
    value["runs"][0]["binding"]["ticket_id"] = "ticket"
    output, barriers = tmp_path / "out", tmp_path / "barriers"
    settings = {
        "workers": [{"worker_id": name} for name in value["workers"]],
        "barriers": str(barriers),
        "upstream_gateway": "127.0.0.1:8094",
        "control_origin": "http://control",
        "driver_tokens": {"tenant-north": "synthetic-key"},
    }
    facts: list[str] = []

    class Proxy:
        def __init__(self, *_: Any, **__: Any) -> None:
            facts.append("proxy")

        def terminate(self) -> None:
            facts.append("proxy_stop")

        def wait(self, **_: Any) -> int:
            return 0

    class Supervised:
        def __init__(self, *_: Any) -> None:
            self.current: Any = None

        def start(self) -> None:
            self.current = object()
            facts.append("worker_start")
            raise ValueError("INJECTION_WINDOW_MISSED")

        def stop(self) -> None:
            facts.append("worker_wait")
            self.current = None

    def disconnected(request: httpx.Request) -> httpx.Response:
        facts.append(request.method)
        rows = json.loads((output / "rows.json").read_bytes())["runs"]
        assert rows[0]["status"] == "submission_attempted"
        if not accepted_then_fail:
            raise httpx.ReadTimeout("private provider error")
        # Actual SDK projection is reused from the existing synthetic fixture.
        from tools.support_evaluation.evidence import load_package
        from tools.support_evaluation.fixtures import case_row, registered

        package = load_package()
        run = copy.deepcopy(case_row(package, registered(package), 0)["run"])
        run.update(
            profile_id="test",
            profile_hash="a" * 64,
            ticket_id="ticket",
            business_request_key=value["runs"][0]["binding"]["business_request_key"],
            budget_batch_id="batch",
        )
        run["budget"]["batch"]["id"] = value["batch_account_id"]
        run["budget"]["batch"]["limits"]["cost_microyuan"] = 5_000_000
        return httpx.Response(201, json={"run": run, "reused": False})

    monkeypatch.setattr(driver, "require_linux", lambda: None)
    monkeypatch.setattr(driver, "wait_proxy", lambda _: None)
    monkeypatch.setattr(driver, "inspect", lambda *_: {})
    monkeypatch.setattr(driver.subprocess, "Popen", Proxy)
    monkeypatch.setattr(driver, "Supervisor", Supervised)
    monkeypatch.setattr(
        driver,
        "CaptureTransport",
        lambda path: CaptureTransport(path, httpx.MockTransport(disconnected)),
    )

    def missing_export(*_: Any) -> Any:
        facts.append("failure_export")
        assert "worker_wait" in facts
        raise ExportError("STEP_PAGE_INCOMPLETE")

    monkeypatch.setattr(driver, "export_run", missing_export)
    with pytest.raises((RequestTimeoutError, ValueError)):
        driver.launch(value, settings, output)
    rows = json.loads((output / "rows.json").read_bytes())["runs"]
    assert len(rows) == 11 and facts.count("POST") == 1
    assert all(row["status"] == "unattempted" for row in rows[1:])
    assert "private provider error" not in (output / "rows.json").read_text()
    if accepted_then_fail:
        assert rows[0]["error_code"] == "INJECTION_WINDOW_MISSED"
        assert rows[0]["failure_export_error"] == "STEP_PAGE_INCOMPLETE"
        assert facts.index("worker_wait") < facts.index("failure_export")
    else:
        assert rows[0]["status"] == "submission_attempted" and rows[0]["run_id"] is None
    with pytest.raises(FileExistsError):
        driver.launch(value, settings, output)
    assert facts.count("POST") == 1


@pytest.mark.parametrize("wrong", ["none", "config", "profile", "batch", "limits"])
def test_inspection_uses_checked_config_and_rejects_other_batch(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, wrong: str
) -> None:
    """An inherited old deployment cannot impersonate this frozen fresh batch."""
    value = frozen()
    value["profile"].update(profile_id="recovery", profile_hash="a" * 64)
    value["batch_key"] = "batch"
    config = tmp_path / "control.s3.enabled.json"
    config.write_text(
        json.dumps(
            {"budgets": [{"scope": "batch", "limits": {"cost_microyuan": 5_000_000}}]}
        )
    )
    value["control_sha256"] = {
        "enabled": hashlib.sha256(config.read_bytes()).hexdigest()
    }
    receipt = {
        "profile": value["profile"].copy(),
        "matches_config": True,
        "migrations_ready": True,
        "history": {},
        "sampled_at": value["valid_from"],
        "accounts": [
            {
                "scope": "batch",
                "account_id": value["batch_account_id"],
                "scope_key": "batch",
                "limits": {"cost_microyuan": 5_000_000},
                "frozen": False,
                "used": {},
                "held_tokens": 0,
                "held_cost_microyuan": 0,
            }
        ],
    }
    if wrong == "config":
        config.write_text("{}")
    elif wrong == "profile":
        receipt["profile"]["profile_id"] = "legacy"
    elif wrong == "batch":
        receipt["accounts"][0]["account_id"] = "old-batch"
    elif wrong == "limits":
        receipt["accounts"][0]["limits"]["cost_microyuan"] += 1
    monkeypatch.setenv("JOBFORGE_AGENT_CONFIG", "old-preparation")

    def inspected(*_: Any, **kwargs: Any) -> Any:
        assert kwargs["env"]["JOBFORGE_AGENT_CONFIG"] == str(config)
        return subprocess.CompletedProcess([], 0, json.dumps(receipt).encode())

    monkeypatch.setattr(driver.subprocess, "run", inspected)
    if wrong == "none":
        assert driver.inspect(value, {"control_config": str(config)}) == receipt
    else:
        with pytest.raises(ValueError, match="S3_INSPECTION_"):
            driver.inspect(value, {"control_config": str(config)})
