"""Finite S4 scope/gates/hash vectors; synthetic checks never release paid work."""

from __future__ import annotations

import copy
import json
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

import pytest

from tools.support_approval import plan, quality
from tools.support_approval.driver import verify_release
from tools.support_approval.report import authorization_hash, contract_hash, mechanism
from tools.support_evaluation.evidence import load_package


def frozen() -> dict[str, Any]:
    """Synthetic plan retains the actual fixed case identities and finite list."""
    package = load_package()
    now = datetime.now(UTC)
    batch = "11111111-1111-4111-8111-111111111111"
    return {
        "schema_version": 1,
        "stage": "S4",
        "max_runs": 12,
        "max_cost_microyuan": 5_000_000,
        "max_seconds": 21600,
        "cap_origin": "planning_chat_scope_choice_not_human_spoken_amount",
        "workers": ["worker-one", "worker-two"],
        "profile": {"executor_version": "linux-v2-approval-runtime-1"},
        "definition": {
            "schema_version": 4,
            "program": {"approval_policy": "ticket_resolution_v1"},
            "action": {"operation": "apply_ticket_resolution"},
        },
        "batch_account_id": batch,
        "valid_from": now.isoformat(),
        "valid_until": (now + timedelta(hours=6)).isoformat(),
        "approval_rule": "original_pending_protocol_sources_frozen_predicates_complete_safety",
        "retry_sources": list(plan.RETRIES),
        "runs": [
            {
                "ordinal": i,
                "case_id": case,
                "intent": intent,
                "idempotency_key": f"submit-s4-{batch}-{i:02d}",
                "binding": {
                    **package.cases[case],
                    "business_request_key": f"s4-{batch}-{i:02d}",
                },
            }
            for i, (case, intent) in enumerate(plan.SPECS, 1)
        ],
    }


@pytest.mark.parametrize(
    "change", ["cap", "runs", "window", "case", "retry", "key", "schema", "workers"]
)
def test_finite_list_rejects_scope_changes(change: str) -> None:
    """Failed samples cannot be replaced, budget-expanded or silently upgraded."""
    value = frozen()
    plan.validate(value, check_sources=False)
    if change == "cap":
        value["max_cost_microyuan"] += 1
    elif change == "runs":
        value["max_runs"] = 24
    elif change == "window":
        value["valid_until"] = (
            datetime.fromisoformat(value["valid_from"]) + timedelta(hours=6, seconds=1)
        ).isoformat()
    elif change == "case":
        value["runs"][0]["case_id"] = "DEV-002"
    elif change == "retry":
        value["retry_sources"].append("DEV-035")
    elif change == "key":
        value["runs"][0]["binding"]["business_request_key"] += "-replacement"
    elif change == "schema":
        value["definition"]["schema_version"] = 3
    else:
        value["workers"][1] = value["workers"][0]
    with pytest.raises(ValueError):
        plan.validate(value, check_sources=False)


def test_missing_release_never_reaches_installed_runtime(tmp_path: Path) -> None:
    """Candidate approved=false stops before process/module/network use."""
    paths = [tmp_path / name for name in ("plan", "settings", "release", "build")]
    for path, value in zip(
        paths,
        [frozen(), {"build_receipt": str(paths[3])}, {"approved": False}, {}],
        strict=True,
    ):
        path.write_text(json.dumps(value))
    with pytest.raises(ValueError, match="S4_NOT_RELEASED"):
        verify_release(*paths[:3])


@pytest.mark.parametrize(
    "failure",
    [
        None,
        "ACCOUNT_EXPOSURE",
        "ACCOUNT_COUNTS",
        "UNAPPROVED_BUSINESS_WRITE",
        "WRONG_ACTION",
        "MODEL_AUDIT_INCOMPLETE",
    ],
)
def test_quality_uses_unmodified_original_and_every_old_failure(
    monkeypatch: pytest.MonkeyPatch, failure: str | None
) -> None:
    """The wrapper forwards original objects and cannot approve an old check failure."""
    row: dict[str, Any] = {
        "case_id": "DEV-001",
        "run": {"run_id": "synthetic", "state": "awaiting_approval"},
        "calls": {
            "batch_frozen": False,
            "batch_stop_code": None,
            "items": [
                {
                    "subcall": "chat",
                    "usage_known": True,
                    "measurement_anomaly": False,
                    "report_conflict": False,
                    "provider_audit": {},
                    "observed_at": "synthetic",
                }
            ],
        },
    }
    profile = {
        "executor_version": "linux-v2-approval-runtime-1",
        "strategy": "support_agent_v1",
    }
    binding: dict[str, Any] = {}
    package = load_package()
    before = copy.deepcopy(row)

    def original(
        actual: Any, registered: Any, capability: Any, data: Any
    ) -> dict[str, Any]:
        assert (
            actual is row
            and registered is binding
            and capability is profile
            and data is package
        )
        return {
            "errors": [] if failure is None else [failure],
            "correct": failure is None,
            "safety": "passed",
            "protocol": "passed",
            "source": "passed",
            "usage_status": "verified",
        }

    monkeypatch.setattr(quality, "evaluate_case", original)
    scored = quality.evaluate_pending(row, binding, profile, package)
    assert scored["approval_eligible"] == (failure is None)
    assert row == before
    row["run"]["state"] = "succeeded"
    assert not quality.evaluate_pending(row, binding, profile, package)[
        "approval_eligible"
    ]


def test_go_action_hash_vectors_remain_exact() -> None:
    """Independent receipt/parameter/authorization checks share the frozen Go vector."""
    path = Path(__file__).resolve().parents[2] / "api/business-action/v1/fixtures.json"
    vector = json.loads(path.read_bytes())
    action, receipt = vector["action"], vector["receipt"]
    assert authorization_hash(action["authorization"]) == vector["authorization_hash"]
    assert (
        contract_hash("jobforge.business.parameters.v1", action["parameters"])
        == vector["parameters_hash"]
    )
    assert (
        contract_hash(
            "jobforge.business.receipt.v1",
            {key: value for key, value in receipt.items() if key != "receipt_hash"},
        )
        == receipt["receipt_hash"]
    )


def test_quality_failure_stays_unexercised_without_invented_zero_cost(
    tmp_path: Path,
) -> None:
    """No terminal/action evidence is synthesized for a failed/unattempted sample."""
    result = mechanism({"status": "quality_failed"}, tmp_path, {}, tmp_path)
    assert result == {
        "state": "unexercised",
        "errors": [],
        "physical_writes": None,
        "physical_queries": None,
    }
