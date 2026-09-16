"""SDK export precision and single-submit bookkeeping; no model service."""

from __future__ import annotations

import copy
import json
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

import httpx
import pytest
from jobforge import RunClient

from tools.support_evaluation import driver
from tools.support_evaluation.driver import BatchStopped, Driver, continuation
from tools.support_evaluation.evidence import canonical, load_package
from tools.support_evaluation.export import CaptureTransport, export_run
from tools.support_evaluation.fixtures import case_row, registered

PACKAGE = load_package()
REGISTRATION = registered(PACKAGE)


def test_export_keeps_actual_number_tokens_and_reads_complete_pages(
    tmp_path: Path,
) -> None:
    """The stored hash input survives SDK float parsing and multiple pages."""
    row = case_row(PACKAGE, REGISTRATION, 0)
    steps = [entry["record"] for entry in row["steps"]]
    steps[0]["output"] = {"score": "number-token"}
    paths: list[str] = []

    def response(request: httpx.Request) -> httpx.Response:
        assert request.method == "GET"
        suffix = request.url.path.rsplit("/", 1)[-1]
        paths.append(suffix)
        if suffix == "steps":
            after = int(request.url.params["after"])
            value = {
                "items": steps[:1] if after == 0 else steps[1:],
                "next_after": 1 if after == 0 else None,
            }
            content = json.dumps(value).replace(
                '"number-token"', "0.1234567890123456789012345"
            )
            return httpx.Response(200, content=content)
        value = {
            "result": row["result"],
            "calls": row["calls"],
            "events": {"items": [], "next_after": None},
        }.get(suffix, row["run"])
        return httpx.Response(200, json=value)

    capture = CaptureTransport(tmp_path, httpx.MockTransport(response))
    with RunClient("http://control", "test-key", transport=capture) as client:
        actual, _ = export_run(client, capture, row["run"]["run_id"])
    assert (
        canonical(actual["steps"][0]["output_json"])
        == '{"score":0.1234567890123456789012345}'
    )
    assert len(actual["steps"]) == len(steps)
    assert paths.count("steps") == 2
    assert len(list(tmp_path.glob("*.receipt.json"))) == 6
    assert all(b"test-key" not in path.read_bytes() for path in tmp_path.iterdir())


@pytest.mark.parametrize(
    "mutation", ["none", "unknown", "conflict", "frozen", "wrong-result"]
)
def test_continuation_requires_real_public_barrier_facts(mutation: str) -> None:
    """Missing or conflicting persistent facts stop the next case."""
    row = case_row(PACKAGE, REGISTRATION, 0)
    if mutation == "unknown":
        row["calls"]["items"][-1]["usage_known"] = False
    elif mutation == "conflict":
        row["calls"]["items"][-1]["report_conflict"] = True
    elif mutation == "frozen":
        row["calls"]["batch_frozen"] = True
    elif mutation == "wrong-result":
        row["result"]["ref"] = "run-step:wrong:6"
    assert continuation(row) == (mutation == "none")


def manifest() -> dict[str, Any]:
    """Build a synthetic 40-row batch without changing frozen business data."""
    return {
        "valid_until": (datetime.now(UTC) + timedelta(hours=1)).isoformat(),
        "profile_id": "test-profile",
        "batch_key": "test-batch",
        "cases": [
            {
                "ordinal": i + 1,
                "case_id": row["case_id"],
                "tenant_id": row["tenant_id"],
                "ticket_id": row["ticket_id"],
                "business_request_key": row["business_request_key"],
                "idempotency_key": "submit-" + row["case_id"],
            }
            for i, row in enumerate(REGISTRATION["bindings"])
        ],
    }


def test_second_rejected_correction_uses_public_domain_error() -> None:
    """Calls API projects MODEL_PROTOCOL_ERROR, not the internal FD enum."""
    row = case_row(PACKAGE, REGISTRATION, 0)
    chat = copy.deepcopy(row["calls"]["items"][-1])
    chat["business_outcome"] = "rejected"
    chat["error_code"] = "MODEL_PROTOCOL_ERROR"
    correction = copy.deepcopy(chat)
    correction["physical_call_id"] = "00000000-0000-4000-8000-999999999999"
    row["calls"]["items"] = [chat, correction]
    row["run"]["state"] = "failed"
    row["run"]["error"] = {"code": "MODEL_PROTOCOL_ERROR", "message": "invalid output"}
    row["steps"][-1]["record"].update(
        kind="model_proposal", output={"correction_required": True}
    )
    assert continuation(row)
    correction["observed_at"] = None
    assert not continuation(row)


def test_submission_unknown_is_one_attempt_and_preserves_all_rows(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """A lost Submit response cannot erase the attempt or trigger a retry."""
    requests: list[httpx.Request] = []

    def disconnected(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        saved = json.loads((tmp_path / "rows.json").read_text())
        assert len(saved["cases"]) == 40
        assert saved["cases"][0]["status"] == "submission_attempted"
        raise httpx.ReadTimeout("synthetic connection loss")

    original = CaptureTransport
    monkeypatch.setattr(
        driver,
        "CaptureTransport",
        lambda path: original(path, httpx.MockTransport(disconnected)),
    )
    instance = Driver(
        manifest(), tmp_path, {"tenant-north": "test", "tenant-south": "test"}
    )
    with pytest.raises(BatchStopped, match="BATCH_STOPPED"):
        instance.run()
    assert len(requests) == 1
    rows = json.loads((tmp_path / "rows.json").read_text())["cases"]
    assert rows[0]["status"] == "submission_attempted"
    assert rows[0]["run_id"] is None
    assert all(row["status"] == "unattempted" for row in rows[1:])
    with pytest.raises(BatchStopped, match="BATCH_ALREADY_ATTEMPTED"):
        Driver(manifest(), tmp_path, {}).run()
    assert len(requests) == 1


def test_initial_record_failure_precedes_any_sdk_request(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """A failed initial archive produces no network request."""

    def fail(*_: Any) -> None:
        raise OSError("synthetic disk failure")

    monkeypatch.setattr(driver, "atomic_json", fail)
    monkeypatch.setattr(
        driver, "CaptureTransport", lambda _: pytest.fail("network was initialized")
    )
    with pytest.raises(OSError):
        Driver(manifest(), tmp_path, {}).run()


def test_stopped_driver_does_not_attempt_first_case(tmp_path: Path) -> None:
    """A local stop retains all rows without starting a case."""
    instance = Driver(copy.deepcopy(manifest()), tmp_path, {})
    instance.stop()
    with pytest.raises(BatchStopped, match="OPERATOR_STOP"):
        instance.run()
    assert all(row["status"] == "unattempted" for row in instance.rows)
