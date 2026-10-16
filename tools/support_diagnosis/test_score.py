"""Exercise independent action, conclusion, claim truth and source failures."""

import copy
import json
from pathlib import Path
from typing import Any

import pytest

from tools.support_diagnosis.prepare import digest, read, save
from tools.support_diagnosis.score import ledger_calls, observed_final, score_final
from tools.support_evaluation.predicates import Facts, derive_facts


def sample() -> tuple[dict[str, Any], dict[str, Any], Facts]:
    """Build one genuinely late delivery and a fully supported proposal."""
    ticket = {
        "order_id": "order",
        "status": "open",
        "observed_at": "2026-09-16T12:00:00Z",
    }
    order = {
        "status": "fulfilled",
        "delivery_id": "delivery",
        "promised_delivery_at": "2026-09-16T08:00:00Z",
    }
    delivery = {
        "status": "delivered",
        "events": [
            {
                "event_id": "receipt",
                "status": "delivered",
                "occurred_at": "2026-09-16T09:00:00Z",
            }
        ],
    }
    facts = derive_facts(ticket, order, delivery, [])
    context = {
        "T": ticket,
        "available_refs": ["P02.1", "E1#/order/promised_delivery_at", "T#/status"],
    }
    proposal = {
        **facts.expected,
        "claims": [
            {
                "kind": "timing",
                "test": "delivered_late",
                "event_id": "receipt",
                "refs": ["P02.1", "E1#/order/promised_delivery_at"],
            }
        ],
    }
    return {"type": "final", "proposal": proposal}, context, facts


def test_correct_and_wrong_conclusion_with_true_evidence() -> None:
    """Do not let fully true evidence repair a wrong final conclusion."""
    value, context, facts = sample()
    assert score_final(value, context, facts)["correct"]
    value["proposal"]["conclusion"] = "on_time"
    result = score_final(value, context, facts)
    assert result["action"] and result["full_evidence"]
    assert not result["conclusion"] and not result["correct"]


def test_false_extra_claim_or_unprovided_source_fails() -> None:
    """Extra claims and invented policy aliases cannot dilute evidence failures."""
    value, context, facts = sample()
    extra = copy.deepcopy(value)
    extra["proposal"]["claims"].append(
        {
            "kind": "ticket_status",
            "mode": "preserve_escalated",
            "refs": ["T#/status", "P02.1"],
        }
    )
    assert not score_final(extra, context, facts)["full_evidence"]
    value["proposal"]["claims"][0]["refs"][0] = "P02.2"
    assert not score_final(value, context, facts)["sources"]
    assert not score_final(value, context, facts)["correct"]


def test_non_final_and_missing_output_remain_failures() -> None:
    """A diagnostic tool request is not silently followed or excluded."""
    _, context, facts = sample()
    for value in (None, {"type": "tool", "name": "search_policy"}):
        result = score_final(value, context, facts)
        assert not result["protocol"] and not result["correct"]


def test_target_status_must_match_runtime_action_contract() -> None:
    """A status mismatch is not a valid full-evidence model proposal."""
    value, context, facts = sample()
    value["proposal"]["target_ticket_status"] = "escalated"
    assert not score_final(value, context, facts)["protocol"]


def test_pg_identity_and_trial_bindings() -> None:
    """A local export from another experiment or request cannot be scored."""
    manifest = {
        "experiment_id": "exp",
        "database_name": "db",
        "database_id": "uuid",
        "trials": [{"trial_id": "one", "request_sha256": "request"}],
    }
    ledger: dict[str, Any] = {
        **manifest,
        "manifest_sha256": "manifest",
        "calls": [
            {
                "experiment_id": "exp",
                "trial_id": "one",
                "ordinal": 1,
                "request_sha256": "request",
            }
        ],
    }
    assert set(ledger_calls(manifest, "manifest", ledger)) == {"one"}
    for key in ("database_id", "manifest_sha256"):
        wrong = dict(ledger, **{key: "wrong"})
        with pytest.raises(ValueError):
            ledger_calls(manifest, "manifest", wrong)
    ledger["calls"][0]["request_sha256"] = "wrong"
    with pytest.raises(ValueError):
        ledger_calls(manifest, "manifest", ledger)


def test_sidecar_cannot_reassign_pg_response(tmp_path: Path) -> None:
    """Changing a local report cannot change the stored PG result binding."""
    (tmp_path / "reports").mkdir()
    (tmp_path / "responses").mkdir()
    value, _, _ = sample()
    raw = json.dumps(
        {
            "choices": [
                {"finish_reason": "stop", "message": {"content": json.dumps(value)}}
            ]
        }
    ).encode()
    (tmp_path / "responses/one.json").write_bytes(raw)
    usage = {"input_tokens": 100, "output_tokens": 30, "cached_input_tokens": 0}
    report = {
        "response_sha256": digest(raw),
        "outcome": "final",
        "stop": "",
        "usage": usage,
        "reasoning_tokens": None,
    }
    save(tmp_path / "reports/one.json", report)
    record = {
        **report,
        **usage,
        "state": "known",
        "report_sha256": digest(json.dumps(report, separators=(",", ":")).encode()),
    }
    trial = {"trial_id": "one"}
    assert observed_final(tmp_path, trial, record)[0] == value
    with pytest.raises(ValueError):
        observed_final(tmp_path, trial, None)
    record["report_sha256"] = "altered"
    with pytest.raises(ValueError):
        observed_final(tmp_path, trial, record)


def test_amended_analysis_keeps_raw_stop_but_scores_requested_group(
    tmp_path: Path,
) -> None:
    """The old analysis remains unchanged; the new one includes every B final."""
    (tmp_path / "reports").mkdir()
    (tmp_path / "responses").mkdir()
    value, _, _ = sample()
    raw = json.dumps(
        {
            "choices": [
                {
                    "finish_reason": "stop",
                    "message": {"content": json.dumps(value), "reasoning_content": ""},
                }
            ]
        }
    ).encode()
    (tmp_path / "responses/one.json").write_bytes(raw)
    usage = {"input_tokens": 100, "output_tokens": 30, "cached_input_tokens": 0}
    report = {
        "response_sha256": digest(raw),
        "outcome": "thinking_not_observed",
        "stop": "thinking_not_observed",
        "usage": usage,
        "reasoning_tokens": 0,
    }
    save(tmp_path / "reports/one.json", report)
    record = {
        **report,
        **usage,
        "state": "known",
        "report_sha256": digest(json.dumps(report, separators=(",", ":")).encode()),
    }
    trial = {"trial_id": "one", "group": "B"}
    assert observed_final(tmp_path, trial, record)[0] is None
    assert observed_final(tmp_path, trial, record, amended=True) == (
        value,
        "thinking_not_observed",
        False,
    )
    assert read(tmp_path / "reports/one.json") == report
