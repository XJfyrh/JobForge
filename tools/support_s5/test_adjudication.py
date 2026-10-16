"""Synthetic new-version scoring without upgrading historical evidence."""

from __future__ import annotations

import copy
import json
from dataclasses import replace
from datetime import timedelta
from typing import Any

import pytest
from jobforge_agent.provider_audit import capture_chat_report, decode_call_report
from jobforge_agent.support_agent_v5 import DEFINITION_ALIASES
from jobforge_agent.support_contract import POINTERS, POLICY_ALIAS

from tools.support_evaluation.driver import continuation
from tools.support_evaluation.evidence import (
    EvidenceError,
    Package,
    load_package,
    registration,
    validate_run_sources,
)
from tools.support_evaluation.fixtures import (
    case_row,
    model_claims,
    registered,
    uid,
    unattempted,
)
from tools.support_evaluation.predicates import (
    Facts,
    claim_sources,
    claim_truth,
    derive_facts,
)
from tools.support_evaluation.score import (
    check_calls,
    check_safety,
    evaluate_case,
    usage_cost,
)
from tools.support_evaluation.test_score import _rehash
from tools.support_evaluation.validate_data import instant
from tools.support_s5 import adjudication
from tools.support_s5.adjudication import (
    CLASS_DEFINITION_TOPICS,
    accepted_policy_aliases,
    claim_level_delta,
    definition_closure,
    definition_entries,
    definition_topic,
    marker_index,
    measurement_summary,
    reasoning_budget,
    retrieval_closure,
    stage_usage,
    stages,
)
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


def package_facts(package: Package, reg: dict[str, Any]):
    """Recompute the reviewed facts for the first registered case."""
    binding = reg["bindings"][0]
    ticket = package.tickets[(binding["tenant_id"], binding["ticket_id"])]
    order = package.orders[(binding["tenant_id"], ticket["order_id"])]
    delivery = package.deliveries[(binding["tenant_id"], order["delivery_id"])]
    return derive_facts(ticket, order, delivery, package.anchors)


def check(index: int, kind: str, predicate: bool, coverage: bool) -> dict[str, Any]:
    """One per-claim measurement row in the accepted shape."""
    return {
        "index": index,
        "kind": kind,
        "predicate": predicate,
        "source_coverage": coverage,
    }


def test_claim_level_delta_exposes_a_regression_the_case_score_hides() -> None:
    """A true claim replaced by a false one must not read as 'unchanged'."""
    draft = {"available": True, "correct": False, "full_case_evidence": True}
    before = [check(0, "critical", True, True), check(1, "timing", True, True)]
    after = [check(0, "critical", True, True), check(1, "timing", False, True)]
    keys = ["critical", "timing"]
    regressed = claim_level_delta(draft, before, keys, after, keys, False)
    assert regressed["flipped"] == [{"kind": "timing", "from": True, "to": False}]
    assert regressed["added"] == [] and regressed["removed"] == []
    assert regressed["evidence_regressed"] is True
    assert regressed["evidence_improved"] is False
    improved = claim_level_delta(
        {"available": True, "correct": False, "full_case_evidence": False},
        after,
        keys,
        before,
        keys,
        True,
    )
    assert improved["flipped"] == [{"kind": "timing", "from": False, "to": True}]
    assert improved["evidence_improved"] is True
    assert improved["evidence_regressed"] is False
    # full -> full is not an improvement, and empty -> empty is not a regression.
    steady = claim_level_delta(draft, before, keys, before, keys, True)
    assert (
        steady["evidence_improved"] is False and steady["evidence_regressed"] is False
    )
    # An unsupported citation is a loss exactly like a false predicate.
    uncited = [check(0, "timing", True, False)]
    dropped = claim_level_delta(draft, before, keys, uncited, ["timing"], False)
    assert dropped["removed"] == ["critical"]
    assert dropped["flipped"] == [{"kind": "timing", "from": True, "to": False}]


def test_claim_level_delta_separates_variants_of_one_kind() -> None:
    """Coverage keys keep missing:<field> apart, which bare kinds would merge."""
    draft = {"available": True, "correct": False, "full_case_evidence": True}
    before = [check(0, "missing", True, True)]
    after = [check(0, "missing", True, True)]
    delta = claim_level_delta(
        draft,
        before,
        ["missing:ticket.order_id"],
        after,
        ["missing:order.delivery_id"],
        True,
    )
    assert delta["added"] == ["missing:order.delivery_id"]
    assert delta["removed"] == ["missing:ticket.order_id"]
    assert delta["flipped"] == []


# The complete frozen policy corpus: the negative direction of the mirror must
# probe every paragraph alias, not only the eight `.1`-biased ones.
ALL_POLICY_ALIASES = frozenset(
    f"P{number:02d}.{part}" for number in range(1, 11) for part in (1, 2)
)
# Every fact pointer group claim_sources can require, satisfied on its own so a
# probe failure is attributable to the policy alias under test.
ALL_POINTERS = frozenset(
    prefix + "#" + pointer for prefix, group in POINTERS.items() for pointer in group
)
# Every (class, timing) pair accepted_policy_aliases can be asked about: four
# whole-case classes, five missing variants, two ticket_status modes, the five
# derivable timing branches, and the underivable-timing case.
ALIAS_MIRROR_CASES = (
    ("conflict", None),
    ("correction", None),
    ("critical", None),
    ("dispute", None),
    ("missing:delivery.delivered_event", None),
    ("missing:delivery.usable_tracking_events", None),
    ("missing:order.delivery_id", None),
    ("missing:ticket.order_id", None),
    ("missing:ticket.problem_description", None),
    ("ticket_status:informational_no_action", None),
    ("ticket_status:preserve_escalated", None),
    ("timing", "delivered_late"),
    ("timing", "delivered_not_late"),
    ("timing", "outstanding_not_overdue"),
    ("timing", "outstanding_overdue_ge48"),
    ("timing", "outstanding_overdue_lt48"),
    ("timing", None),
)


def build_claim(required: str, facts: Facts) -> dict[str, Any]:
    """One claim of this class that claim_truth accepts against these facts."""
    kind, _, variant = required.partition(":")
    claim: dict[str, Any] = {"kind": kind, "refs": []}
    if kind == "timing":
        claim.update(test=facts.timing, event_id=sorted(facts.timing_ids)[0])
    elif kind == "critical":
        event_id = sorted(facts.critical_ids)[0]
        claim.update(status=facts.events[event_id]["status"], event_id=event_id)
    elif kind == "conflict":
        name, pair = next(iter(facts.conflicts))
        claim.update(type=name, event_ids=sorted(pair))
    elif kind == "correction":
        recovery, corrected = sorted(facts.corrections)[0]
        claim.update(recovery_event_id=recovery, corrected_event_id=corrected)
    elif kind == "dispute":
        claim.update(
            type=sorted(facts.dispute_types)[0],
            delivered_event_id=sorted(facts.timing_ids)[0],
        )
    elif kind == "missing":
        claim["field"] = variant
    else:
        claim["mode"] = variant
    return claim


def synthetic_facts(required: str, timing: str | None) -> Facts:
    """Facts in which exactly this required class holds, built not derived.

    The shipped development fixtures do not contain every branch the mirror
    describes, so the missing ones are constructed. These are synthetic facts
    for a source-coverage comparison; nothing here claims ``derive_facts`` would
    produce them, and no case data is authored.
    """
    kind, _, variant = required.partition(":")

    def event(event_id: str, status: str, occurred_at: str) -> dict[str, Any]:
        return {"event_id": event_id, "status": status, "occurred_at": occurred_at}

    ticket: dict[str, Any] = {
        "tenant_id": "tenant-north",
        "ticket_id": "T-synthetic",
        "status": "open",
        "order_id": "O-synthetic",
        "observed_at": "2026-09-16T12:00:00Z",
    }
    order: dict[str, Any] | None = {
        "order_id": "O-synthetic",
        "delivery_id": "D-synthetic",
        "status": "fulfilled",
        "promised_delivery_at": "2026-09-15T12:00:00Z",
    }
    delivery: dict[str, Any] | None = {
        "delivery_id": "D-synthetic",
        "order_id": "O-synthetic",
        "status": "in_transit",
        "events": [],
    }
    events: dict[str, dict[str, Any]] = {}
    active: set[str] = set()
    corrections: set[tuple[str, str]] = set()
    conflicts: set[tuple[str, frozenset[str]]] = set()
    disputes: set[str] = set()
    critical: set[str] = set()
    missing: set[str] = set()
    timing_ids: set[str] = set()
    expected: dict[str, Any] = {
        "decision": "proposal",
        "action": "record_conclusion",
        "conclusion": "on_time",
        "requested_fields": [],
        "target_ticket_status": "open",
    }
    if kind in {"timing", "ticket_status"}:
        events = {"E1": event("E1", "delivered", "2026-09-14T12:00:00Z")}
        active = timing_ids = {"E1"}
        if variant == "informational_no_action":
            ticket["status"] = "informational_only"
            expected = {**expected, "decision": "no_action", "action": ""}
        elif variant == "preserve_escalated":
            ticket["status"] = "escalated"
    elif kind == "dispute":
        events = {"E1": event("E1", "delivered", "2026-09-14T12:00:00Z")}
        active = timing_ids = {"E1"}
        disputes = {"wrong_address"}
    elif kind == "critical":
        events = {"E1": event("E1", "lost", "2026-09-14T12:00:00Z")}
        active = critical = {"E1"}
    elif kind == "conflict":
        events = {
            "E1": event("E1", "delivered", "2026-09-14T12:00:00Z"),
            "E2": event("E2", "in_transit", "2026-09-15T12:00:00Z"),
        }
        active = {"E1", "E2"}
        conflicts = {("post_delivery", frozenset({"E1", "E2"}))}
    elif kind == "correction":
        events = {
            "E1": event("E1", "in_transit", "2026-09-14T12:00:00Z"),
            "E2": event("E2", "delivered", "2026-09-15T12:00:00Z"),
        }
        active = timing_ids = {"E2"}
        corrections = {("E2", "E1")}
    elif kind == "missing":
        missing = {variant}
        if variant == "ticket.order_id":
            order, delivery = None, None
            ticket["order_id"] = None
    return Facts(
        ticket,
        order,
        delivery,
        events,
        frozenset(active),
        frozenset(corrections),
        frozenset(conflicts),
        frozenset(disputes),
        frozenset(critical),
        frozenset(missing),
        timing,
        frozenset(timing_ids),
        expected,
        frozenset({required}),
    )


def test_accepted_alias_mapping_agrees_with_the_scorer() -> None:
    """The offline mirror must not disagree with claim_sources for any class."""
    package, reg, _, _, _ = final_fixture()
    facts = package_facts(package, reg)
    assert ALL_POLICY_ALIASES == set(package.paragraphs), "corpus drifted"
    assert facts.required, "fixture case must require at least one class"
    for required in sorted(facts.required):
        claim = build_claim(required, facts)
        assert claim_truth(claim, facts), required
        accepted = accepted_policy_aliases(required, facts.timing)
        assert accepted, required
        for alias in sorted(ALL_POLICY_ALIASES):
            refs = set(ALL_POINTERS) | {alias}
            if alias in accepted:
                assert claim_sources(claim, facts, refs), (required, alias)
            else:
                assert not claim_sources(claim, facts, refs), (required, alias)
    # No derivable timing means no valid timing claim and no satisfying alias.
    assert accepted_policy_aliases("timing", None) == frozenset()


@pytest.mark.parametrize(("required", "timing"), ALIAS_MIRROR_CASES)
def test_accepted_alias_mirror_is_two_way_for_every_branch(
    required: str, timing: str | None
) -> None:
    """Cover every branch of accepted_policy_aliases in both directions.

    The shipped development fixtures exercise only one (class, timing)
    combination, so each branch gets a synthetic Facts in which that class
    genuinely holds and a claim ``claim_truth`` accepts. Both directions are
    probed over the complete 20-alias corpus, with every fact-pointer group
    already satisfied, so a failure is attributable to the policy alias alone.
    """
    facts = synthetic_facts(required, timing)
    accepted = accepted_policy_aliases(required, timing)
    assert accepted <= ALL_POLICY_ALIASES, (required, timing)
    if required == "timing" and timing is None:
        # No derivable timing means no valid timing claim exists at all, so no
        # retrieved alias can satisfy the requirement and none can be probed.
        assert accepted == frozenset()
        return
    assert accepted, (required, timing)
    claim = build_claim(required, facts)
    assert claim_truth(claim, facts), (required, timing)
    for alias in sorted(accepted):
        assert claim_sources(claim, facts, set(ALL_POINTERS) | {alias}), (
            required,
            timing,
            alias,
        )
    for alias in sorted(ALL_POLICY_ALIASES - accepted):
        assert not claim_sources(claim, facts, set(ALL_POINTERS) | {alias}), (
            required,
            timing,
            alias,
        )


def test_retrieval_closure_reports_only_citable_policy() -> None:
    """Fact pointers are not policy; only retrieved paragraphs can satisfy a class."""
    package, reg, row = agent_fixture(draft_correct=True, final="correct")
    result = evaluate_case(row, reg["bindings"][0], reg["profile"], package)
    result["full_case_evidence"] = full_case_evidence(result)
    checkpoint, _ = validate_run_sources(
        row, reg["bindings"][0], reg["profile"], package
    )
    assert marker_index(checkpoint) is not None
    closure = retrieval_closure(checkpoint, package)
    assert closure is not None
    assert closure["available"], "an agent row must have retrieved policy"
    assert all(POLICY_ALIAS.fullmatch(alias) for alias in closure["available"])
    assert closure["adjudication_recoverable"] is (not closure["unsatisfied"])
    for entry in closure["required"]:
        assert entry["satisfied"] is bool(entry["present_aliases"])
        assert set(entry["present_aliases"]) <= set(entry["accepted_aliases"])
    if not closure["adjudication_recoverable"]:
        assert result["full_case_evidence"] is False


def test_retrieval_closure_is_marker_anchored_and_absent_for_fixed() -> None:
    """Without a closed investigation marker there is no paired delta to anchor."""
    package, reg, row, _, _ = final_fixture()
    checkpoint, _ = validate_run_sources(
        row, reg["bindings"][0], reg["profile"], package
    )
    assert marker_index(checkpoint) is None
    assert retrieval_closure(checkpoint, package) is None
    assert definition_closure(checkpoint, package) is None


def test_imported_definition_table_still_has_the_expected_topics() -> None:
    """Silent drift in the v5 prompt module must break this measurement loudly.

    The table is imported, never duplicated, so the measurement follows the
    prompt. Seven topics are addressable from a required semantic class;
    ``extended_delay`` is navigation for the timing branch and no class maps to
    it, which makes eight keys in total.
    """
    assert DEFINITION_ALIASES["structured_conflict"] == ("P05.1",)
    mapped = set(CLASS_DEFINITION_TOPICS.values())
    assert mapped == {
        "carrier_correction",
        "critical_exception",
        "delivery_dispute",
        "missing_facts",
        "structured_conflict",
        "ticket_status",
        "timing",
    }
    assert len(mapped) == 7
    assert set(DEFINITION_ALIASES) == mapped | {"extended_delay"}
    for topic, aliases in DEFINITION_ALIASES.items():
        assert aliases, topic
        assert all(POLICY_ALIAS.fullmatch(alias) for alias in aliases), topic
    # Every mapped topic must be a real key, or definition_topic raises instead
    # of reporting a measurement.
    for required in ("conflict", "timing", "missing:ticket.order_id"):
        assert DEFINITION_ALIASES[definition_topic(required)]


@pytest.mark.parametrize(
    ("required", "topic"),
    [
        ("conflict", "structured_conflict"),
        ("correction", "carrier_correction"),
        ("critical", "critical_exception"),
        ("dispute", "delivery_dispute"),
        ("missing:delivery.delivered_event", "missing_facts"),
        ("missing:ticket.order_id", "missing_facts"),
        ("ticket_status:informational_no_action", "ticket_status"),
        ("ticket_status:preserve_escalated", "ticket_status"),
        ("timing", "timing"),
    ],
)
def test_definition_topic_covers_every_required_class(
    required: str, topic: str
) -> None:
    """The variant suffix never changes which paragraph defines the class."""
    assert definition_topic(required) == topic
    assert DEFINITION_ALIASES[topic]


def test_definition_topic_fails_closed_on_an_unmapped_class(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """A grown contract must not be reported as a complete investigation."""
    with pytest.raises(EvidenceError, match="DEFINITION_TOPIC_UNMAPPED"):
        definition_topic("brand_new_class")
    with pytest.raises(EvidenceError, match="DEFINITION_TOPIC_UNMAPPED"):
        definition_entries({"brand_new_class:variant"}, set())
    monkeypatch.setattr(adjudication, "DEFINITION_ALIASES", {})
    with pytest.raises(EvidenceError, match="DEFINITION_TOPIC_UNBOUND"):
        definition_topic("conflict")


def test_definition_entries_sees_the_alias_sharing_the_other_notion_misses() -> None:
    """A sibling paragraph retrieved by another search is not the definition."""
    # S5V3-004: P05.2 arrived via a correction search; P05.1, the only paragraph
    # defining the five conflict types, was never read.
    retrieved = {"P02.1", "P03.2", "P05.2", "P06.2", "P07.1", "P07.2"}
    entries = definition_entries({"conflict"}, retrieved)
    assert entries == [
        {
            "required": "conflict",
            "topic": "structured_conflict",
            "definition_aliases": ["P05.1"],
            "definition_present": [],
            "definition_satisfied": False,
        }
    ]
    # The very same retrieved set satisfies the accepted-alias notion.
    assert accepted_policy_aliases("conflict", None) & retrieved == {"P05.2"}
    assert definition_entries({"conflict"}, retrieved | {"P05.1"})[0][
        "definition_satisfied"
    ]


def test_the_two_closure_notions_disagree_in_both_directions() -> None:
    """Neither notion implies the other, so they are reported side by side."""
    # Definition-incomplete but reachable: conflict with only P05.2 retrieved.
    assert accepted_policy_aliases("conflict", None) & {"P05.2"}
    entry = definition_entries({"conflict"}, {"P05.2"})[0]
    assert entry["definition_satisfied"] is False
    # Reachable=false but definition-complete: the ge48 branch accepts only
    # P07.1, while the timing definition is P02.1/P02.2.
    assert not accepted_policy_aliases("timing", "outstanding_overdue_ge48") & {"P02.1"}
    entry = definition_entries({"timing"}, {"P02.1"})[0]
    assert entry["definition_satisfied"] is True
    # Multiple required classes keep their own topic and are sorted and summed.
    entries = definition_entries(
        {"critical", "timing", "missing:order.delivery_id"}, {"P06.1", "P02.2"}
    )
    assert [item["required"] for item in entries] == [
        "critical",
        "missing:order.delivery_id",
        "timing",
    ]
    assert [item["definition_satisfied"] for item in entries] == [True, False, True]


def test_definition_closure_pairs_with_retrieval_closure_on_a_real_row() -> None:
    """Both notions are marker-anchored over the same retrieved paragraph set."""
    package, reg, row = agent_fixture(draft_correct=True, final="correct")
    checkpoint, _ = validate_run_sources(
        row, reg["bindings"][0], reg["profile"], package
    )
    closure = retrieval_closure(checkpoint, package)
    definition = definition_closure(checkpoint, package)
    assert closure is not None and definition is not None
    available = set(closure["available"])
    assert [item["required"] for item in definition["required"]] == [
        item["required"] for item in closure["required"]
    ]
    for item in definition["required"]:
        assert item["topic"] == definition_topic(item["required"])
        assert item["definition_aliases"] == sorted(DEFINITION_ALIASES[item["topic"]])
        assert item["definition_present"] == sorted(
            set(item["definition_aliases"]) & available
        )
        assert item["definition_satisfied"] is bool(item["definition_present"])
    assert definition["definition_unsatisfied"] == [
        item["required"]
        for item in definition["required"]
        if not item["definition_satisfied"]
    ]
    assert definition["investigation_definition_complete"] is (
        not definition["definition_unsatisfied"]
    )


def test_reasoning_budget_flags_thin_completion_and_deadline_headroom() -> None:
    """Deep reasoning that nearly fills the envelope is a truncation risk."""

    def call(reasoning: int, seconds: float) -> dict[str, Any]:
        reserved = instant("2026-10-11T08:00:00Z")
        observed = reserved + timedelta(seconds=seconds)
        stamp = lambda value: value.isoformat().replace("+00:00", "Z")  # noqa: E731
        return {
            "subcall": "chat",
            "step_kind": "model_proposal",
            "reserved_at": stamp(reserved),
            "observed_at": stamp(observed),
            "known_cost_microyuan": 1,
            "held_cost_microyuan": 0,
            "known_tokens": 1,
            "held_tokens": 0,
            "provider_audit": {"mode_state": "thinking", "reasoning_tokens": reasoning},
        }

    def budget(reasoning: int, seconds: float):
        calls = [call(reasoning, seconds)]
        return reasoning_budget(calls, stage_usage(calls))

    deep = budget(15000, 30.0)
    assert deep is not None and deep["truncation_risk"] is True
    assert deep["ratio"] > 0.9 and deep["deadline_headroom_seconds"] == 90.0
    slow = budget(900, 100.0)
    assert slow is not None and slow["truncation_risk"] is True
    assert slow["deadline_headroom_seconds"] == 20.0
    safe = budget(900, 30.0)
    assert safe is not None and safe["truncation_risk"] is False
    assert safe["completion_capacity"] == 16384 and safe["deadline_seconds"] == 120
    assert reasoning_budget([], None) is None
    assert reasoning_budget([call(900, 1.0)], None) is None


def test_measurement_summary_keeps_an_explicit_denominator() -> None:
    """Unmeasured cases are skipped, never counted as recoverable or passing."""
    results: list[dict[str, Any]] = [
        {
            "case_id": "A",
            "adjudication_stages": {
                "claim_level_delta": {
                    "evidence_regressed": True,
                    "evidence_improved": False,
                },
                "retrieval_closure": {"adjudication_recoverable": False},
                "definition_closure": {"investigation_definition_complete": False},
                "reasoning_budget": {"truncation_risk": True},
            },
        },
        {
            "case_id": "B",
            "adjudication_stages": {
                "claim_level_delta": {
                    "evidence_regressed": False,
                    "evidence_improved": True,
                },
                "retrieval_closure": {"adjudication_recoverable": True},
                # The S5V3-004 shape: reachable under the accepted-alias notion
                # and still definition-incomplete. The summary must keep both.
                "definition_closure": {"investigation_definition_complete": False},
                "reasoning_budget": {"truncation_risk": False},
            },
        },
        {
            "case_id": "E",
            "adjudication_stages": {
                "claim_level_delta": None,
                "retrieval_closure": {"adjudication_recoverable": True},
                "definition_closure": {"investigation_definition_complete": True},
                "reasoning_budget": None,
            },
        },
        # A fixed-graph case carries no marker-anchored measurement at all.
        {
            "case_id": "C",
            "adjudication_stages": {
                "claim_level_delta": None,
                "retrieval_closure": None,
                "definition_closure": None,
                "reasoning_budget": None,
            },
        },
        {"case_id": "D"},
    ]
    summary = measurement_summary(results)
    assert summary["evidence_regressed"] == 1
    assert summary["evidence_regressed_cases"] == ["A"]
    assert summary["evidence_improved"] == 1 and summary["evidence_improved_cases"] == [
        "B"
    ]
    assert summary["retrieval_closure_measured"] == 3
    assert summary["adjudication_recoverable_false"] == 1
    assert summary["adjudication_recoverable_false_cases"] == ["A"]
    assert summary["definition_closure_measured"] == 3
    assert summary["investigation_definition_incomplete"] == 2
    assert summary["investigation_definition_incomplete_cases"] == ["A", "B"]
    assert summary["truncation_risk"] == 1 and summary["truncation_risk_cases"] == ["A"]


def test_stages_thread_the_new_measurements_end_to_end() -> None:
    """The paired delta exists only when an actual draft was scored."""
    package, reg, row = agent_fixture(draft_correct=True, final="correct")
    result = evaluate_case(row, reg["bindings"][0], reg["profile"], package)
    result["full_case_evidence"] = full_case_evidence(result)
    detail = stages(row, result, reg["bindings"][0], reg["profile"], package)
    assert detail["investigation"]["available"] is True
    # Measurement plumbing stays out of the published investigation block.
    assert "claim_checks" not in detail["investigation"]
    assert "claim_keys" not in detail["investigation"]
    delta = detail["claim_level_delta"]
    assert delta is not None and delta["evidence_regressed"] is False
    assert isinstance(delta["draft_claims"], list) and delta["draft_claims"]
    assert isinstance(delta["final_claims"], list) and delta["final_claims"]
    assert len(delta["draft_claims"]) == len(delta["final_claims"])
    assert isinstance(detail["retrieval_closure"], dict)
    definition = detail["definition_closure"]
    assert isinstance(definition, dict)
    assert set(definition) == {
        "required",
        "definition_unsatisfied",
        "investigation_definition_complete",
    }
    assert definition["required"], "a scored agent row has required classes"
    budget = detail["reasoning_budget"]
    assert budget is None or budget["completion_capacity"] == 16384
    summary = measurement_summary([dict(result, adjudication_stages=detail)])
    assert summary["retrieval_closure_measured"] == 1
    assert summary["definition_closure_measured"] == 1
    assert summary["investigation_definition_incomplete"] == (
        0 if definition["investigation_definition_complete"] else 1
    )
    assert summary["evidence_regressed"] == 0
