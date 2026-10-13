"""Keep marker losses visible while measuring only the actual final input."""

from __future__ import annotations

import copy
from typing import Any

import pytest

from tools.support_evaluation.evidence import (
    EvidenceError,
    registration,
    validate_run_sources,
)
from tools.support_evaluation.fixtures import unattempted
from tools.support_s5.adjudication import (
    definition_closure,
    final_input_closure,
    marker_index,
    measurement_summary,
    retrieval_closure,
    stage_usage,
    stages,
    supplement_reasoning_budget,
)
from tools.support_s5.score import paired, score_export
from tools.support_s5.test_adjudication import agent_fixture, final_fixture
from tools.support_s5.test_score import audit


@pytest.mark.parametrize(
    "schema,executor",
    [(14, "linux-v2-adjudication-runtime-3"), (15, "linux-v2-adjudication-runtime-4")],
)
def test_versioned_registration_and_full_export_scoring(
    schema: int, executor: str
) -> None:
    """Both five-field capability tuples survive the actual scoring entry point."""
    package, reg, row = agent_fixture(draft_correct=True, final="correct")
    reg["profile"]["executor_version"] = executor
    reg["strategy_blueprint"] = {"schema_version": schema}
    reg["comparison"] = {
        "model": {"message_content_bytes": 65536, "request_body_bytes": 131072},
        "final_model": {},
        "resources": {},
        "family_limits": {},
    }
    registration(reg, package, s5=True)
    report = score_export(
        reg,
        {
            "schema_version": 1,
            "registration_sha256": "a" * 64,
            "cases": [row] + [unattempted(c) for c in list(package.cases)[1:]],
            "receipt_audit": audit(),
        },
        "a" * 64,
        package=package,
    )
    assert report["schema_version"] == 3
    assert report["denominator"] == len(report["cases"]) == 40
    assert report["correct_count"] == report["full_case_evidence_count"] == 1
    assert report["adjudication_measurement"]["final_input_closure_measured"] == 1


def test_final_input_closure_does_not_rewrite_marker_or_count_intent_as_final() -> None:
    """A late definition repairs the final input, not the historical marker."""
    package, reg, row = agent_fixture(draft_correct=True, final="correct")
    checkpoint, _ = validate_run_sources(
        row, reg["bindings"][0], reg["profile"], package
    )
    marker = marker_index(checkpoint)
    assert marker is not None
    search = next(
        s for s in checkpoint["steps"] if s["step"]["kind"] == "search_policy"
    )
    original = copy.deepcopy(search)
    search["result_json"]["content"]["matches"] = []
    search["result_json"]["evidence_refs"] = []
    retrieval = retrieval_closure(checkpoint, package)
    definition = definition_closure(checkpoint, package)
    measured = final_input_closure(checkpoint, package)
    assert retrieval is not None and definition is not None and measured is not None
    assert not retrieval["adjudication_recoverable"]
    assert not definition["investigation_definition_complete"]
    assert not measured["definition_closure"]["definition_complete"]
    intent = copy.deepcopy(checkpoint["steps"][marker + 1])
    intent["result_json"].update(
        proposal=None,
        content={
            "type": "policy_supplement",
            "topic": "timing",
            "required_aliases": ["P02.1"],
        },
    )
    checkpoint["steps"][marker + 1 : marker + 1] = [intent, original]
    measured = final_input_closure(checkpoint, package)
    assert measured is not None
    assert measured["retrieval_closure"]["adjudication_recoverable"]
    assert measured["definition_closure"]["definition_complete"]
    assert retrieval_closure(checkpoint, package) == retrieval
    for length in (marker + 1, marker + 2, marker + 3):
        assert (
            final_input_closure(
                dict(checkpoint, steps=checkpoint["steps"][:length]), package
            )
            is None
        )
    assert final_input_closure(None, package) is None


def test_each_thinking_call_has_its_own_capacity_deadline_and_cost() -> None:
    """Two comfortable calls must not appear as one over-capacity call."""
    _, _, _, first, _ = final_fixture()
    second = copy.deepcopy(first)
    second["physical_call_id"] = "11111111-1111-4111-8111-111111111111"
    for call in (first, second):
        call["provider_audit"]["reasoning_tokens"] = 9000
        call["observed_usage"]["output_tokens"] = 10000
    result = supplement_reasoning_budget([first, second])
    assert result is not None
    assert len(result["calls"]) == 2
    assert not result["truncation_risk"]
    assert result["usage"] == stage_usage([first, second])
    assert result["usage"]["known_cost_microyuan"] == 2 * first["known_cost_microyuan"]
    assert all(c["completion_capacity"] == 16384 for c in result["calls"])
    second["observed_usage"], second["provider_audit"], second["observed_at"] = (
        None,
        None,
        None,
    )
    second["known_cost_microyuan"], second["held_cost_microyuan"] = 0, 2228224
    result = supplement_reasoning_budget([first, second])
    assert result is not None
    assert result["calls"][1]["completion_tokens"] is None
    assert result["calls"][1]["reasoning_tokens"] is None
    assert result["calls"][1]["wall_seconds"] is None
    assert result["usage"]["held_cost_microyuan"] == 2228224


def test_new_summary_has_separate_final_denominators_and_preserves_old_detail() -> None:
    """A missing final input remains unmeasured, never a successful closure."""
    package, reg, row = agent_fixture(draft_correct=True, final="correct")
    from tools.support_evaluation.score import evaluate_case
    from tools.support_s5.score import full_case_evidence

    result = evaluate_case(row, reg["bindings"][0], reg["profile"], package)
    result["full_case_evidence"] = full_case_evidence(result)
    old = stages(row, result, reg["bindings"][0], reg["profile"], package)
    reg["profile"]["executor_version"] = "linux-v2-adjudication-runtime-3"
    new = stages(row, result, reg["bindings"][0], reg["profile"], package)
    assert old["retrieval_closure"] == new["retrieval_closure"]
    assert old["definition_closure"] == new["definition_closure"]
    assert "final_input_closure" not in old
    absent = copy.deepcopy(new)
    absent["final_input_closure"] = None
    summary = measurement_summary(
        [
            {"case_id": "observed", "adjudication_stages": new},
            {"case_id": "unmeasured", "adjudication_stages": absent},
        ]
    )
    assert summary["final_input_closure_measured"] == 1
    assert summary["final_input_unrecoverable"] == 0
    assert summary["final_input_definition_incomplete"] == 0


@pytest.mark.parametrize(
    "mutation", [None, "tokens", "fixed_tokens", "chat", "model", "resources", "old"]
)
@pytest.mark.parametrize("schema", [14, 15])
def test_pair_allows_only_the_versioned_supplement_capacity_difference(
    mutation: str | None,
    schema: int,
) -> None:
    """Preserve every other comparison binding and expose the asymmetry."""
    fixed: dict[str, Any] = {
        "strategy": "support_fixed_v1",
        "strategy_blueprint": {"schema_version": 12},
        "dataset_version": "synthetic-unit",
        "gold_sha256": "a" * 64,
        "denominator": 1,
        "cases": [{"case_id": "synthetic-unit", "correct": False}],
        "comparison": {
            "model": {"synthetic": True},
            "resources": {},
            "family_limits": {"tokens": 12610560, "chat_calls": 12},
        },
        "usage_total": None,
        "verified_usage_subtotal": {},
        "usage_complete": False,
    }
    agent = copy.deepcopy(fixed)
    agent["strategy"] = "support_agent_v1"
    agent["strategy_blueprint"] = {"schema_version": schema}
    agent["comparison"]["family_limits"]["tokens"] = 12625920
    if mutation == "tokens":
        agent["comparison"]["family_limits"]["tokens"] += 1
    elif mutation == "fixed_tokens":
        fixed["comparison"]["family_limits"]["tokens"] += 1
    elif mutation == "chat":
        agent["comparison"]["family_limits"]["chat_calls"] = 13
    elif mutation in {"model", "resources"}:
        agent["comparison"][mutation] = {"different": True}
    elif mutation == "old":
        agent["strategy_blueprint"] = {"schema_version": 13}
    before = copy.deepcopy((agent, fixed))
    if mutation:
        with pytest.raises(EvidenceError, match="PAIR_DATA_BINDING"):
            paired(agent, fixed)
    else:
        report = paired(agent, fixed)
        assert report["outcomes"] == {"tie": 1}
        assert report["comparison_differences"]["family_tokens"] == {
            "agent": 12625920,
            "fixed": 12610560,
        }
    assert (agent, fixed) == before
