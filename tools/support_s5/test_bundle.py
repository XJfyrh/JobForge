"""Offline scoring accepts actual bundle evidence without relabelling old runs."""

from __future__ import annotations

import copy
from dataclasses import replace

import pytest
from jobforge_agent.business import POLICY_BUNDLE_ALIASES

from tools.support_evaluation.evidence import (
    EvidenceError,
    canonical,
    registration,
    validate_run_sources,
)
from tools.support_evaluation.fixtures import unattempted
from tools.support_evaluation.score import check_calls, check_safety, evaluate_case
from tools.support_evaluation.test_score import _rehash
from tools.support_s5.adjudication import definition_closure, final_input_closure
from tools.support_s5.guard import stop_reasons
from tools.support_s5.score import score_export
from tools.support_s5.test_adjudication import agent_fixture, final_fixture
from tools.support_s5.test_score import audit


def bundle_fixture(*, fixed: bool):
    """Convert only synthetic test traces and recompute their actual hash chain."""
    if fixed:
        package, reg, row, _, _ = final_fixture()
    else:
        package, reg, row = agent_fixture(draft_correct=True, final="correct")
    reg["profile"]["executor_version"] = (
        "linux-v2-fixed-adjudication-runtime-2"
        if fixed
        else "linux-v2-adjudication-runtime-5"
    )
    entries = row["steps"]
    search = next(s for s in entries if s["record"]["kind"] == "search_policy")
    search["record"]["kind"] = "read_policy_bundle"
    output = search["record"]["output"]
    template = output["content"]["matches"][0]
    output["content"]["matches"] = [
        dict(
            template,
            chunk_id=alias,
            source=alias[:3] + ".md",
            text=package.paragraphs[alias],
            evidence_ref=f"business-policy:{reg['bindings'][0]['index_id']}:{alias}",
        )
        for alias in POLICY_BUNDLE_ALIASES
    ]
    for hit in output["content"]["matches"]:
        hit.pop("distance", None)
    output["evidence_refs"] = [
        hit["evidence_ref"] for hit in output["content"]["matches"]
    ]
    entries.remove(search)
    entries = [
        s
        for s in entries
        if (s["record"]["output"]["content"] or {}).get("name") != "search_policy"
    ]
    entries.insert(1, search)
    row["steps"] = entries
    step_ids = {entry["record"]["step_id"] for entry in entries}
    calls = [
        c
        for c in row["calls"]["items"]
        if c["step_id"] in step_ids
        and c["subcall"] not in {"profile_version", "profile_tags", "query_embedding"}
    ]
    for call in calls:
        if call["subcall"] == "search_policy":
            call.update(subcall="read_policy_bundle", step_kind="read_policy_bundle")
    positions = {s["record"]["step_id"]: n for n, s in enumerate(entries)}
    calls.sort(key=lambda c: positions[c["step_id"]])
    for ordinal, call in enumerate(calls, 1):
        call["ordinal"] = ordinal
    row["calls"]["items"] = calls
    call_ids = {c["physical_call_id"] for c in calls}
    row["safety"]["requests"] = [
        r for r in row["safety"]["requests"] if r["physical_call_id"] in call_ids
    ]
    for request in row["safety"]["requests"]:
        if request["subcall"] == "search_policy":
            request.update(subcall="read_policy_bundle", method="GET")
    for sequence, entry in enumerate(entries, 1):
        entry["record"].update(
            sequence=sequence,
            cursor_version=sequence,
            output_ref=f"run-step:{row['run']['run_id']}:{sequence}",
        )
    row["run"]["cursor_version"] = len(entries)
    chats = sum(c["subcall"] == "chat" for c in calls)
    usage = row["run"]["budget"]["run_usage"]
    usage.update(
        physical_http=len(calls),
        chat=chats,
        tokens=15 * chats,
        cost_microyuan=60 * chats,
        logical_tools=3,
        query_embedding=0,
        profile_metadata_http=0,
        business_tool_http=3,
    )
    for scope in ("family", "tenant", "batch"):
        row["run"]["budget"][scope].update(
            used=copy.deepcopy(usage),
            known_tokens=15 * chats,
            known_cost_microyuan=60 * chats,
            held_tokens=0,
        )
    _rehash(row)
    return package, reg, row


@pytest.mark.parametrize("fixed", [False, True])
def test_bundle_registration_sources_safety_and_scoring(fixed: bool) -> None:
    """Bundle participates in source verification, call accounting and policy closure."""
    package, reg, row = bundle_fixture(fixed=fixed)
    registration(reg, package, s5=True)
    checkpoint, _ = validate_run_sources(
        row, reg["bindings"][0], reg["profile"], package
    )
    usage = check_calls(row, reg["profile"])
    assert usage["business_tool_calls"] == 3
    assert usage["query_embedding_calls"] == usage["profile_metadata_calls"] == 0
    assert check_safety(row, reg["bindings"][0], reg["profile"])[0] == "passed"
    result = evaluate_case(row, reg["bindings"][0], reg["profile"], package)
    assert result["correct"] and result["source"] == "passed"
    if not fixed:
        closure = definition_closure(checkpoint, package)
        assert closure is not None and closure["investigation_definition_complete"]
    row["steps"][1]["record"]["output"]["content"]["matches"][0]["text"] += " altered"
    _rehash(row)
    result = evaluate_case(row, reg["bindings"][0], reg["profile"], package)
    assert result["source"] == "failed" and not result["correct"]


@pytest.mark.parametrize("fixed", [False, True])
def test_bundle_final_closure_and_guard_through_score_export(fixed: bool) -> None:
    """Both strategies measure the committed final without inventing a Fixed draft."""
    package, reg, row = bundle_fixture(fixed=fixed)
    reg["strategy_blueprint"] = {"schema_version": 17 if fixed else 16}
    reg["comparison"] = {
        "model": {"message_content_bytes": 65536, "request_body_bytes": 131072},
        "final_model": {},
        "resources": {},
        "family_limits": {},
    }
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
    assert report["correct_count"] == report["full_case_evidence_count"] == 1
    assert report["adjudication_measurement"]["final_input_closure_measured"] == 1
    detail = report["cases"][0]["adjudication_stages"]
    closure = detail["final_input_closure"]
    assert closure["retrieval_closure"]["adjudication_recoverable"]
    assert closure["retrieval_closure"]["available"] == list(POLICY_BUNDLE_ALIASES)
    assert closure["definition_closure"]["definition_complete"]
    assert "calls" not in detail["reasoning_budget"]
    if fixed:
        assert not detail["investigation"]["available"]
        assert detail["claim_level_delta"] is None
        assert detail["definition_closure"] is detail["retrieval_closure"] is None
    sample = dict.fromkeys(
        ("known", "held", "unknown_chat_calls", "inflight", "unfinished_attempts"), 0
    )
    assert stop_reasons(report, 1, sample, 0, [None]) == []
    for result in report["cases"][1:]:
        assert result["adjudication_stages"]["final_input_closure"] is None


@pytest.mark.parametrize("fixed", [False, True])
def test_bundle_without_committed_final_does_not_claim_final_closure(
    fixed: bool,
) -> None:
    """A complete bundle alone does not establish that final adjudication succeeded."""
    package, reg, row = bundle_fixture(fixed=fixed)
    checkpoint, _ = validate_run_sources(
        row, reg["bindings"][0], reg["profile"], package
    )
    final_index = next(
        i
        for i, entry in enumerate(checkpoint["steps"])
        if entry["step"]["kind"] == "model_proposal"
    )
    checkpoint["steps"] = checkpoint["steps"][:final_index]
    assert final_input_closure(checkpoint, package) is None


@pytest.mark.parametrize("fixed", [False, True])
def test_bundle_export_size_limit_is_versioned(fixed: bool) -> None:
    """The scorer accepts 16-32 KiB bundles without widening old result limits."""
    package, reg, row = bundle_fixture(fixed=fixed)
    paragraphs = {alias: "x" * 768 for alias in POLICY_BUNDLE_ALIASES}
    for hit in row["steps"][1]["record"]["output"]["content"]["matches"]:
        hit["text"] = paragraphs[hit["chunk_id"]]
    package = replace(package, paragraphs=paragraphs)
    _rehash(row, maximum=32768)
    raw = row["steps"][1]["output_json"]
    assert 16384 < len(raw.encode()) <= 32768
    validate_run_sources(row, reg["bindings"][0], reg["profile"], package)
    with pytest.raises(EvidenceError, match="STEP_SIZE"):
        canonical(raw)
    row["steps"][1]["output_json"] = raw + " " * 32768
    with pytest.raises(EvidenceError, match="STEP_SIZE"):
        validate_run_sources(row, reg["bindings"][0], reg["profile"], package)
    row["steps"][1]["output_json"] = raw
    reg["profile"]["executor_version"] = (
        "linux-v2-fixed-adjudication-runtime-1"
        if fixed
        else "linux-v2-adjudication-runtime-4"
    )
    with pytest.raises(EvidenceError, match="STEP_SIZE"):
        validate_run_sources(row, reg["bindings"][0], reg["profile"], package)
