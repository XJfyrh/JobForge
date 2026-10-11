"""Synthetic new-version scoring without upgrading historical evidence."""

from __future__ import annotations

import copy
import json
from dataclasses import replace
from typing import Any

import pytest
from jobforge_agent.provider_audit import capture_chat_report, decode_call_report

from tools.support_evaluation.driver import continuation
from tools.support_evaluation.evidence import EvidenceError, load_package, registration
from tools.support_evaluation.fixtures import (
    case_row,
    model_claims,
    registered,
    uid,
    unattempted,
)
from tools.support_evaluation.predicates import derive_facts
from tools.support_evaluation.score import (
    check_calls,
    check_safety,
    evaluate_case,
    usage_cost,
)
from tools.support_evaluation.test_score import _rehash
from tools.support_s5.adjudication import stage_usage, stages
from tools.support_s5.score import full_case_evidence


def final_fixture(*, model=None):
    """Convert one synthetic Fixed call, retaining the original accepted proposal."""
    package = load_package()
    reg = registered(package)
    reg["scorer_version"] = "support-s5-quality-v1"
    reg["strategy_blueprint"] = {}
    reg["comparison"] = {}
    profile = reg["profile"]
    profile.update(
        executor_version="linux-v2-fixed-adjudication-runtime-1",
        provider_audit_policy="deepseek-audit-v2",
        max_output_tokens=16384,
    )
    row = case_row(package, reg, 0, model=model)
    row["result"]["disposition"] = "proposal"
    call = next(c for c in row["calls"]["items"] if c["subcall"] == "chat")
    call["reserved"]["output_tokens"] = 16384
    call["reserved"]["total_tokens"] = profile["max_input_tokens"] + 16384
    call["reserved"]["cost_microyuan"] = usage_cost(
        profile["pricing"],
        {
            "input_tokens": profile["max_input_tokens"],
            "cached_input_tokens": 0,
            "output_tokens": 16384,
        },
    )
    body = {
        "id": "synthetic-final",
        "object": "chat.completion",
        "created": 1,
        "model": "deepseek-flash",
        "system_fingerprint": "synthetic",
        "choices": [
            {
                "index": 0,
                "finish_reason": "stop",
                "message": {
                    "role": "assistant",
                    "content": "{}",
                    "reasoning_content": "synthetic reasoning",
                },
            }
        ],
        "usage": {
            "prompt_tokens": 10,
            "completion_tokens": 5,
            "total_tokens": 15,
            "prompt_cache_hit_tokens": 0,
            "prompt_cache_miss_tokens": 10,
            "completion_tokens_details": {"reasoning_tokens": 4},
        },
    }
    report = capture_chat_report(
        json.dumps(body).encode(),
        http_status=200,
        physical_call_id=call["physical_call_id"],
        expected_response_model="deepseek-flash",
        thinking=True,
    ).to_dict()
    call.update(
        provider_audit=report["provider_audit"],
        audit_hash=report["provider_audit"]["audit_hash"],
        observed_usage=report["usage"],
        settled_usage=report["usage"],
    )
    request = next(r for r in row["safety"]["requests"] if r["subcall"] == "chat")
    request.update(thinking="enabled", max_tokens=16384)
    return package, reg, row, call, request


def test_new_registration_and_scoring_keep_original_final_contract() -> None:
    """Final reasoning is compatible only with the registered new executor."""
    package, reg, row, _, _ = final_fixture()
    registration(reg, package, s5=True)
    result = evaluate_case(row, reg["bindings"][0], reg["profile"], package)
    assert result["correct"] and result["usage_status"] == "verified"
    assert result["safety"] == "passed"
    assert continuation(row, executor_version=reg["profile"]["executor_version"])
    assert not continuation(row)
    assert result["usage"]["known_tokens"] == 15
    reg["profile"]["executor_version"] = "linux-v2-fixed-comparison-runtime-1"
    with pytest.raises(EvidenceError):
        registration(reg, package, s5=True)


@pytest.mark.parametrize(
    "mutation", ["reservation", "request", "audit_version", "step"]
)
def test_final_permission_is_not_inferred_from_response(mutation: str) -> None:
    """Reject bounds, mode or audit changes independently of proposal quality."""
    _, reg, row, call, request = final_fixture()
    if mutation == "reservation":
        call["reserved"]["output_tokens"] = 1024
    elif mutation == "request":
        request["thinking"] = "disabled"
        assert (
            "TRUSTED_CONFIGURATION_CHANGED"
            in check_safety(row, reg["bindings"][0], reg["profile"])[1]
        )
        return
    elif mutation == "audit_version":
        call["provider_audit"]["schema_version"] = 1
    else:
        call["step_kind"] = "model_decision"
    with pytest.raises((EvidenceError, ValueError)):
        check_calls(row, reg["profile"])


def test_fixed_rejected_terminal_continues_without_correction() -> None:
    """A fully observed terminal failure closes one case without a second chat."""
    _, reg, row, call, _ = final_fixture()
    row["run"].update(state="failed", error={"code": "MODEL_PROTOCOL_ERROR"})
    row["steps"] = row["steps"][:-2]
    call.update(business_outcome="rejected", error_code="MODEL_PROTOCOL_ERROR")
    assert continuation(row, executor_version=reg["profile"]["executor_version"])
    assert not continuation(row)
    call["provider_audit"]["mode_state"] = "invalid"
    assert not continuation(row, executor_version=reg["profile"]["executor_version"])


def test_stage_usage_retains_unknown_holds_and_incomplete_time() -> None:
    """Unknown terminal exposure cannot disappear into a zero-cost average."""
    _, _, _, call, _ = final_fixture()
    unknown = copy.deepcopy(call)
    unknown.update(
        observed_at=None,
        known_cost_microyuan=0,
        known_tokens=0,
        held_cost_microyuan=unknown["reserved"]["cost_microyuan"],
        held_tokens=unknown["reserved"]["total_tokens"],
    )
    result = stage_usage([call, unknown])
    assert result["chat_calls"] == 2 and result["known_tokens"] == 15
    assert result["held_tokens"] > 16384
    assert result["held_cost_microyuan"] > 0
    assert result["reservation_to_observation_seconds"] is None


@pytest.mark.parametrize("damage", ["missing", "items", "call", "field"])
def test_unavailable_stage_ledger_is_not_zero(damage: str) -> None:
    """Broken evidence keeps its denominator without inventing free calls."""
    package, reg, row, call, _ = final_fixture()
    if damage == "missing":
        row["calls"] = None
    elif damage == "items":
        row["calls"]["items"] = None
    elif damage == "call":
        row["calls"]["items"] = [None]
    else:
        del call["known_cost_microyuan"]
    result = evaluate_case(row, reg["bindings"][0], reg["profile"], package)
    result["full_case_evidence"] = full_case_evidence(result)
    assert result["usage_status"] == "unavailable"
    detail = stages(row, result, reg["bindings"][0], reg["profile"], package)
    assert detail["investigation"]["usage"] is None
    assert detail["final"]["usage"] is None
    assert detail["final"]["no_reasoning_signal_calls"] is None


def test_unattempted_stage_usage_is_zero() -> None:
    """Only an explicit unattempted row establishes no physical calls."""
    package, reg, _, _, _ = final_fixture()
    row = unattempted(reg["bindings"][0]["case_id"])
    result = evaluate_case(row, reg["bindings"][0], reg["profile"], package)
    result["full_case_evidence"] = False
    detail = stages(row, result, reg["bindings"][0], reg["profile"], package)
    assert detail["investigation"]["usage"] == stage_usage([])
    assert detail["final"]["usage"] == stage_usage([])


def agent_fixture(*, draft_correct: bool, final: str):
    """Insert synthetic agent decisions while retaining original source bytes."""
    package, reg, row, final_call, _ = final_fixture()
    legacy = case_row(package, registered(package), 0)
    chat = next(c for c in legacy["calls"]["items"] if c["subcall"] == "chat")
    request = next(c for c in legacy["safety"]["requests"] if c["subcall"] == "chat")
    reg["profile"].update(
        strategy="support_agent_v1",
        executor_version="linux-v2-adjudication-runtime-1",
    )
    binding = reg["bindings"][0]
    ticket = package.tickets[(binding["tenant_id"], binding["ticket_id"])]
    order = package.orders[(binding["tenant_id"], ticket["order_id"])]
    delivery = package.deliveries[(binding["tenant_id"], order["delivery_id"])]
    facts = derive_facts(ticket, order, delivery, package.anchors)
    draft = {**facts.expected, "claims": model_claims(facts)}
    if final == "wrong":
        _, _, row, final_call, _ = final_fixture(
            model={**draft, "action": "escalate", "target_ticket_status": "escalated"}
        )
    if not draft_correct:
        draft["conclusion"] = "conflicting"
    entries: list[dict[str, Any]] = []
    calls = []
    for entry in row["steps"]:
        record = entry["record"]
        kind = record["kind"]
        if kind in {"get_order", "get_delivery", "search_policy", "model_proposal"}:
            number = len(entries) + 1
            decision = copy.deepcopy(entry)
            decision["record"].update(
                step_id=uid(90000 + number), kind="model_decision"
            )
            content = (
                {"type": "investigation_complete", "draft": draft}
                if kind == "model_proposal"
                else {
                    "type": "tool",
                    "name": kind,
                    "arguments": {"query": "synthetic timing policy"}
                    if kind == "search_policy"
                    else {"order_id": ticket["order_id"]},
                }
            )
            call = copy.deepcopy(chat)
            call.update(
                physical_call_id=uid(91000 + number),
                step_id=decision["record"]["step_id"],
                step_kind="model_decision",
            )
            report = decode_call_report(
                json.dumps(
                    {
                        "usage": call["observed_usage"],
                        "provider_audit": call["provider_audit"],
                    }
                ).encode()
            )
            assert report.provider_audit is not None and report.usage is not None
            receipt = report.provider_audit.receipt_hash(call["physical_call_id"])
            usage_report = replace(report.usage, receipt_hash=receipt)
            usage = replace(usage_report, usage_hash=usage_report.hash()).to_dict()
            call.update(observed_usage=usage, settled_usage=usage)
            decision["record"]["output"].update(
                tool_invocation_id="",
                physical_call_id=call["physical_call_id"],
                content=content,
                proposal=None,
                evidence_refs=[],
                correction_required=False,
            )
            trace = copy.deepcopy(request)
            trace["physical_call_id"] = call["physical_call_id"]
            row["safety"]["requests"].append(trace)
            calls.append(call)
            entries.append(decision)
        calls.extend(
            c for c in row["calls"]["items"] if c["step_id"] == record["step_id"]
        )
        if final != "truncated" or kind not in {"model_proposal", "submit_proposal"}:
            entries.append(entry)
    for sequence, entry in enumerate(entries, 1):
        entry["record"].update(
            sequence=sequence,
            cursor_version=sequence,
            output_ref=f"run-step:{row['run']['run_id']}:{sequence}",
        )
    for ordinal, call in enumerate(calls, 1):
        call["ordinal"] = ordinal
    row["steps"], row["calls"]["items"] = entries, calls
    row["run"]["cursor_version"] = len(entries)
    if final == "truncated":
        row["run"].update(
            state="failed",
            proposal_ref=None,
            error={"code": "MODEL_PROTOCOL_ERROR", "message": "synthetic"},
        )
        row["result"] = {
            "available": False,
            "kind": None,
            "ref": None,
            "disposition": "none",
        }
        final_call.update(
            business_outcome="rejected", error_code="MODEL_PROTOCOL_ERROR"
        )
    extra = sum(c["step_kind"] == "model_decision" for c in calls)
    usage = row["run"]["budget"]["run_usage"]
    usage.update(
        physical_http=len(calls),
        chat=1 + extra,
        tokens=527 + 15 * extra,
        cost_microyuan=60 * (extra + 1),
    )
    for scope in ("family", "tenant", "batch"):
        row["run"]["budget"][scope].update(
            used=copy.deepcopy(usage),
            known_tokens=15 * (extra + 1),
            known_cost_microyuan=60 * (extra + 1),
        )
    _rehash(row)
    return package, reg, row


@pytest.mark.parametrize(
    "draft_correct,final", [(True, "wrong"), (True, "truncated"), (False, "correct")]
)
def test_agent_draft_never_substitutes_for_final(
    draft_correct: bool, final: str
) -> None:
    """Final acceptance cannot select whichever of the two outputs looks better."""
    package, reg, row = agent_fixture(draft_correct=draft_correct, final=final)
    result = evaluate_case(row, reg["bindings"][0], reg["profile"], package)
    result["full_case_evidence"] = full_case_evidence(result)
    assert result["protocol"] == result["source"] == "passed", result["errors"]
    assert result["usage_status"] == "verified", result["errors"]
    detail = stages(row, result, reg["bindings"][0], reg["profile"], package)
    assert detail["investigation"]["correct"] == draft_correct
    assert detail["final"]["correct"] == result["correct"] == (final == "correct")
    assert detail["investigation"]["usage"]["chat_calls"] == 4
    assert detail["final"]["usage"]["chat_calls"] == 1
