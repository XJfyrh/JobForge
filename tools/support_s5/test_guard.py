"""V6 gates use the final input, preserving the old investigation diagnosis."""

from __future__ import annotations

import copy
from typing import Any

import pytest

from tools.support_s5.guard import stop_reasons


def inputs() -> tuple[dict[str, Any], dict[str, int]]:
    """Return ten repaired final inputs and thirty untouched worklist entries."""
    case = {
        "source": "passed",
        "safety": "passed",
        "usage_status": "verified",
        "call_audit_complete": True,
        "execution_complete": True,
        "run_state": "awaiting_approval",
        "adjudication_stages": {
            "retrieval_closure": {"adjudication_recoverable": False},
            "definition_closure": {"investigation_definition_complete": False},
            "final_input_closure": {
                "retrieval_closure": {"adjudication_recoverable": True},
                "definition_closure": {"definition_complete": True},
            },
        },
    }
    return {
        "schema_version": 3,
        "denominator": 40,
        "correct_count": 10,
        "full_case_evidence_count": 10,
        "hard_failure_cases": {"budget_breach": 0},
        "cases": [copy.deepcopy(case) for _ in range(10)]
        + [{"status": "unattempted"} for _ in range(30)],
    }, dict(
        unknown_chat_calls=0, inflight=0, held=0, unfinished_attempts=0, known=200000
    )


@pytest.mark.parametrize(
    "mutation,expected",
    [
        ("repaired", []),
        ("retrieval", ["FINAL_RETRIEVAL_CLOSURE_GAP"]),
        ("definition", ["FINAL_DEFINITION_CLOSURE_GAP"]),
        ("absent", ["FINAL_CLOSURE_UNMEASURED"]),
        ("partial", ["FINAL_CLOSURE_UNMEASURED"]),
        ("unknown", ["CURRENT_LEDGER_NOT_CLOSED"]),
        ("held", ["CURRENT_LEDGER_NOT_CLOSED"]),
        ("hard", ["HARD_SAFETY_FAILURE"]),
        ("correct", ["CORRECT_THRESHOLD_UNREACHABLE"]),
        ("full", ["EVIDENCE_REGRESSION_UNAVOIDABLE"]),
        ("cost", ["COST_CAP"]),
        ("source", ["UNVERIFIED_CASE_BINDING"]),
        ("version", ["SCORE_VERSION"]),
        ("denominator", ["DENOMINATOR"]),
        ("one_protocol", []),
        ("two_protocol", ["SYSTEMIC_PROTOCOL_FAILURE"]),
    ],
)
def test_stop_reasons(mutation: str, expected: list[str]) -> None:
    """Keep each stop condition independent and leave scorer output immutable."""
    score, sample = inputs()
    signatures: list[Any] = [None] * 10
    last = score["cases"][9]
    closure = last["adjudication_stages"]["final_input_closure"]
    if mutation in {"retrieval", "definition"}:
        key = (
            "adjudication_recoverable"
            if mutation == "retrieval"
            else "definition_complete"
        )
        closure[mutation + "_closure"][key] = False
    elif mutation == "absent":
        last["adjudication_stages"]["final_input_closure"] = None
    elif mutation == "partial":
        closure["definition_closure"] = None
    elif mutation in {"unknown", "held"}:
        sample["unknown_chat_calls" if mutation == "unknown" else "held"] = 1
    elif mutation == "hard":
        score["hard_failure_cases"]["budget_breach"] = 1
    elif mutation == "correct":
        score["correct_count"] = 5
    elif mutation == "full":
        score["full_case_evidence_count"] = 7
    elif mutation == "cost":
        sample["known"] = 4_000_001
    elif mutation == "source":
        last["source"] = "failed"
    elif mutation == "version":
        score["schema_version"] = 2
    elif mutation == "denominator":
        score["cases"].pop()
    elif mutation in {"one_protocol", "two_protocol"}:
        last["adjudication_stages"]["final_input_closure"] = None
        last["run_state"] = "failed"
        score["correct_count"] -= 1
        score["full_case_evidence_count"] -= 1
        signatures[-1] = ("MODEL_PROTOCOL_ERROR",)
        if mutation == "two_protocol":
            signatures[-2] = signatures[-1]
    before = copy.deepcopy(score)
    assert stop_reasons(score, 10, sample, 10_591_762, signatures) == expected
    assert score == before


def test_stage_cap_and_nonconsecutive_early_protocol_errors() -> None:
    """Preserve cumulative exposure and early repeated-error stopping rules."""
    score, sample = inputs()
    assert stop_reasons(score, 10, sample, 27_900_000, [None] * 10) == ["COST_CAP"]
    assert stop_reasons(score, 10, sample, 10_591_762, ["bad", None, "bad"]) == [
        "SYSTEMIC_PROTOCOL_FAILURE"
    ]
