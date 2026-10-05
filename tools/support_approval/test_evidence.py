"""Audit partial/uncertain submissions and prevent approval evidence substitution."""

from __future__ import annotations

import copy
import json
from pathlib import Path
from typing import Any

import pytest

from tools.support_approval import driver, report


def write(path: Path, value: Any) -> None:
    """Save a clearly synthetic private test artifact."""
    path.write_text(json.dumps(value))


def test_postgres_uri_is_expanded_in_environment_without_secret_arguments() -> None:
    """Fixed audits and the single loader change use the intended host/database."""
    actual = driver.postgres_environment(
        "postgres://reader:p%40ss@business-postgres:5432/jobforge_s4_business?sslmode=disable"
    )
    assert {
        key: actual[key]
        for key in (
            "PGHOST",
            "PGPORT",
            "PGUSER",
            "PGPASSWORD",
            "PGDATABASE",
            "PGSSLMODE",
        )
    } == {
        "PGHOST": "business-postgres",
        "PGPORT": "5432",
        "PGUSER": "reader",
        "PGPASSWORD": "p@ss",
        "PGDATABASE": "jobforge_s4_business",
        "PGSSLMODE": "disable",
    }


@pytest.mark.parametrize(
    "dsn",
    [
        "jobforge_s4_business",
        "postgres://reader@db/?sslmode=disable",
        "postgres://reader@db/name?sslmode=disable&host=other",
        "postgres://reader@db/name#fragment",
    ],
)
def test_invalid_audit_connection_is_a_bounded_error(dsn: str) -> None:
    """Private connection bytes never become an exception diagnostic."""
    with pytest.raises(ValueError, match="^INVALID_AUDIT_CONNECTION$"):
        driver.postgres_environment(dsn)


@pytest.mark.parametrize(
    "field", ["run", "result", "steps", "calls", "effect", "action_calls", "approval"]
)
def test_quality_projection_must_match_original_before_scoring(
    tmp_path: Path, field: str
) -> None:
    """A valid-looking replacement cannot be scored after export substitution."""
    pending: dict[str, Any] = {
        key: {}
        for key in (
            "run",
            "result",
            "steps",
            "calls",
            "effect",
            "action_calls",
            "approval",
        )
    }
    candidate = copy.deepcopy(pending)
    candidate[field] = {"substituted": True}
    write(tmp_path / "pending.json", pending)
    write(tmp_path / "quality-input.json", candidate)
    assert report.checked_quality({}, {}, tmp_path, {}) == {
        "approval_eligible": False,
        "errors": ["QUALITY_PENDING_CHANGED"],
    }


def test_both_final_audits_are_attempted_after_first_audit_failure(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
) -> None:
    """Preserve final control evidence despite unavailable business evidence."""
    seen = []

    def audit(
        _settings: Any, query: str, _credential: str, _variables: Any
    ) -> dict[str, Any]:
        seen.append(query)
        if query == "actions_audit":
            raise ValueError("BUSINESS_UNAVAILABLE")
        return {"runs": [{"run_id": "uncertain-ACK"}]}

    monkeypatch.setattr(driver, "sql", audit)
    errors = driver.final_audits({}, {"batch_account_id": "synthetic"}, tmp_path)
    assert seen == ["actions_audit", "control_audit"]
    assert len(errors) == 1
    assert json.loads((tmp_path / "control-audit-final.json").read_bytes())["runs"]


@pytest.mark.parametrize("unknown", [False, True])
def test_uncertain_submission_is_counted_from_ledger_with_known_and_full_hold(
    tmp_path: Path,
    unknown: bool,
) -> None:
    """No submit ACK does not erase an actual Run or its budget exposure."""
    plan: dict[str, Any] = {
        "batch_account_id": "synthetic-batch",
        "max_cost_microyuan": 5_000_000,
        "profile": {"profile_id": "synthetic-profile", "profile_hash": "a" * 64},
        "retry_sources": [],
        "runs": [
            {
                "case_id": "DEV-001",
                "binding": {
                    "business_request_key": "fixed-intent",
                    "tenant_id": "tenant-north",
                    "ticket_id": "fixed-ticket",
                },
            }
        ],
    }
    model = {
        "run_id": "actual-run",
        "profile_hash": "a" * 64,
        "status": "unknown" if unknown else "known",
        "subcall": "chat",
        "known_tokens": 20,
        "known_cost_microyuan": 40,
        "reserved_tokens": 1024,
        "reserved_cost_microyuan": 8192,
    }
    control = {
        "batch_account_id": "synthetic-batch",
        "batch_account": {
            "known_tokens": 0 if unknown else 20,
            "known_cost_microyuan": 0 if unknown else 40,
            "held_tokens": 1024 if unknown else 0,
            "held_cost_microyuan": 8192 if unknown else 0,
            "used_cost_microyuan": 8192 if unknown else 40,
            "limit_cost_microyuan": 5_000_000,
            "frozen": unknown,
        },
        "runs": [
            {
                "run_id": "actual-run",
                "business_request_key": "fixed-intent",
                "authorization": None,
                "model_calls": [model],
                "run": {
                    "run_id": "actual-run",
                    "tenant_id": "tenant-north",
                    "ticket_id": "fixed-ticket",
                    "retry_of_run_id": None,
                    **plan["profile"],
                },
            }
        ],
    }
    write(tmp_path / "control-audit-final.json", control)
    write(tmp_path / "business-actions-final.json", {"resolutions": []})
    observed = report.batch_audit(
        plan, [{"case_id": "DEV-001", "run_id": None}], tmp_path
    )
    assert observed["actual_new_runs"] == 1
    assert observed["state"] == ("failed" if unknown else "passed")
    assert observed["usage"]["held_cost_microyuan"] == (8192 if unknown else 0)
    assert observed["usage"]["known_cost_microyuan"] == (0 if unknown else 40)
    assert observed["usage"]["unknown_chat_calls"] == int(unknown)


def test_missing_final_audit_preserves_unknown_count_and_cost(tmp_path: Path) -> None:
    """Unavailable ledger evidence cannot be reported as zero Runs/zero cost."""
    result = report.batch_audit({}, [], tmp_path)
    assert result["state"] == "failed"
    assert result["actual_new_runs"] is result["usage"] is None
