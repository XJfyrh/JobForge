"""Offline scorer regressions; synthetic traces never establish model acceptance."""

from __future__ import annotations

import copy
import json
from datetime import timedelta
from pathlib import Path
from typing import Any

import pytest
from jobforge_agent.runtime_input import input_hash

from tools.support_evaluation.evidence import (
    EvidenceError,
    canonical,
    fingerprint,
    load_package,
    read_json,
)
from tools.support_evaluation.fixtures import (
    case_row,
    model_claims,
    registered,
    unattempted,
)
from tools.support_evaluation.predicates import derive_facts
from tools.support_evaluation.score import evaluate_case, main, score_export, usage_cost
from tools.support_evaluation.validate_data import instant, sha256

PACKAGE = load_package()
REGISTRATION = registered(PACKAGE)


def _facts(index: int) -> Any:
    binding = REGISTRATION["bindings"][index]
    ticket = copy.deepcopy(
        PACKAGE.tickets[(binding["tenant_id"], binding["ticket_id"])]
    )
    order = copy.deepcopy(
        PACKAGE.orders.get((binding["tenant_id"], ticket["order_id"]))
    )
    delivery = (
        copy.deepcopy(
            PACKAGE.deliveries.get((binding["tenant_id"], order["delivery_id"]))
        )
        if order
        else None
    )
    return derive_facts(ticket, order, delivery, PACKAGE.anchors)


def _score(row: dict[str, Any], index: int = 0) -> dict[str, Any]:
    return evaluate_case(
        row, REGISTRATION["bindings"][index], REGISTRATION["profile"], PACKAGE
    )


def _export(rows: list[dict[str, Any]]) -> tuple[dict[str, Any], str]:
    raw = json.dumps(REGISTRATION).encode()
    return {
        "schema_version": 1,
        "registration_sha256": sha256(raw),
        "cases": rows,
    }, sha256(raw)


def _rehash(row: dict[str, Any]) -> None:
    """Make source tampering tests reach provenance checks beyond hash integrity."""
    previous = ""
    run = row["run"]
    for entry in row["steps"]:
        record = entry["record"]
        entry["output_json"] = json.dumps(record["output"], ensure_ascii=False)
        cursor = record["sequence"] - 1
        record["input_hash"] = input_hash(
            run["profile_hash"], run["snapshot_hash"], cursor, previous
        )
        previous = fingerprint(
            "jobforge.run.commit.v1",
            record["step_id"],
            str(record["sequence"]),
            record["kind"],
            str(cursor),
            record["input_hash"],
            run["profile_id"],
            run["profile_hash"],
            run["snapshot_id"],
            run["snapshot_hash"],
            canonical(entry["output_json"]),
        )
        record["commit_hash"] = previous


@pytest.mark.parametrize("index", range(40))
def test_policy_predicates_independently_match_frozen_labels(index: int) -> None:
    """This is rule/data QA, not a scored model output or actual execution."""
    facts = _facts(index)
    case_id = REGISTRATION["bindings"][index]["case_id"]
    gold = PACKAGE.gold[case_id]["expected"]
    assert facts.expected["conclusion"] == gold["conclusion"]
    assert facts.expected["requested_fields"] == sorted(gold["requested_fields"])
    assert facts.expected["target_ticket_status"] == gold["target_ticket_status"]
    assert (
        facts.expected["action"]
        == {
            "record_resolution": "record_conclusion",
            "request_information": "request_information",
            "escalate_human": "escalate",
            "none": "",
        }[gold["action"]]
    )


def test_synthetic_protocol_matrix_never_establishes_actual_acceptance() -> None:
    """All forty synthetic shapes can validate without a real-model claim."""
    evidence, digest = _export([case_row(PACKAGE, REGISTRATION, i) for i in range(40)])
    report = score_export(REGISTRATION, evidence, digest, package=PACKAGE)
    assert report["denominator"] == report["correct_count"] == 40
    assert report["evidence_origin"] == "synthetic_test"
    assert not report["actual_acceptance_evidence_complete"]
    assert report["usage"]["known_cost_microyuan"] == 2400
    assert all(
        row["protocol"] == row["source"] == row["business"] == "passed"
        for row in report["cases"]
    )


def test_all_unattempted_is_zero_over_fixed_forty() -> None:
    """A budget-stopped batch keeps every registered case in the denominator."""
    evidence, digest = _export([unattempted(case) for case in PACKAGE.cases])
    report = score_export(REGISTRATION, evidence, digest, package=PACKAGE)
    assert report["denominator"] == report["unattempted"] == 40
    assert report["accuracy"] == report["attempted_runs"] == 0
    assert not report["actual_acceptance_evidence_complete"]


@pytest.mark.parametrize("mutation", ["absent", "duplicate", "unknown"])
def test_bad_case_coverage_is_rejected_not_renormalized(mutation: str) -> None:
    """Missing or repeated rows cannot quietly improve the denominator."""
    evidence, digest = _export([unattempted(case) for case in PACKAGE.cases])
    if mutation == "absent":
        evidence["cases"].pop()
    elif mutation == "duplicate":
        evidence["cases"][-1] = evidence["cases"][0]
    else:
        evidence["cases"][-1]["case_id"] = "DEV-999"
    with pytest.raises(EvidenceError, match="CASE_COVERAGE"):
        score_export(REGISTRATION, evidence, digest, package=PACKAGE)


@pytest.mark.parametrize(
    "field",
    [
        "gold_sha256",
        "scoring_sha256",
        "anchors_sha256",
        "corpus_sha256",
        "scorer_version",
        "policy_version",
        "dataset_version",
    ],
)
def test_version_or_frozen_hash_drift_is_rejected(field: str) -> None:
    """Scoring cannot mix a newer label, corpus or scorer into an existing batch."""
    registration = copy.deepcopy(REGISTRATION)
    registration[field] = "other-version"
    evidence, digest = _export([unattempted(case) for case in PACKAGE.cases])
    with pytest.raises(EvidenceError):
        score_export(registration, evidence, digest, package=PACKAGE)


@pytest.mark.parametrize(
    "field",
    [
        "tenant_id",
        "ticket_id",
        "snapshot_id",
        "snapshot_hash",
        "profile_id",
        "profile_hash",
        "budget_batch_id",
        "business_request_key",
    ],
)
def test_run_must_match_prior_case_and_profile_identity(field: str) -> None:
    """Model-like identity assertions cannot replace predeclared Run provenance."""
    row = case_row(PACKAGE, REGISTRATION, 0)
    row["run"][field] = (
        "f" * 64
        if "hash" in field
        else "00000000-0000-4000-8000-000000000999"
        if field == "snapshot_id"
        else "other"
    )
    result = _score(row)
    assert not result["correct"] and result["source"] == "failed"


@pytest.mark.parametrize(
    "mutation",
    [
        "wrong_index",
        "wrong_policy",
        "changed_ticket",
        "changed_order",
        "changed_carrier",
        "policy_text",
        "step_reorder",
        "bad_commit",
        "raw_output_mismatch",
        "result_ref",
        "free_summary",
        "copied_gold",
    ],
)
def test_source_and_persisted_output_tampering_cannot_pass(mutation: str) -> None:
    """Even recomputing fixture hashes cannot authorize a different source."""
    row = case_row(PACKAGE, REGISTRATION, 0)
    rehash = False
    if mutation == "wrong_index":
        row["run"]["version_vector"]["index"]["profile_hash"] = "f" * 64
    elif mutation == "wrong_policy":
        row["run"]["version_vector"]["policy"]["revision"] += 1
    elif mutation == "changed_ticket":
        row["steps"][0]["record"]["output"]["content"]["description"] = (
            "INJECTED SECRET"
        )
        rehash = True
    elif mutation == "changed_order":
        row["steps"][1]["record"]["output"]["content"]["order"][
            "promised_delivery_at"
        ] = "2026-09-01T00:00:00Z"
        rehash = True
    elif mutation == "changed_carrier":
        row["steps"][2]["record"]["output"]["content"]["delivery"]["events"][0][
            "note"
        ] = "INJECTED SECRET"
        rehash = True
    elif mutation == "policy_text":
        row["steps"][3]["record"]["output"]["content"]["matches"][0]["text"] = (
            "INJECTED SECRET"
        )
        rehash = True
    elif mutation == "step_reorder":
        row["steps"][0], row["steps"][1] = row["steps"][1], row["steps"][0]
    elif mutation == "bad_commit":
        row["steps"][0]["record"]["commit_hash"] = "f" * 64
    elif mutation == "raw_output_mismatch":
        row["steps"][0]["output_json"] = "{}"
    elif mutation == "result_ref":
        row["result"]["ref"] = "run-proposal:other"
    elif mutation == "free_summary":
        row["steps"][-1]["record"]["output"]["proposal"]["summary"] = (
            "INJECTED SECRET refund approved"
        )
        rehash = True
    else:
        # A gold label copied into output is not a real six/eight-field proposal.
        row["steps"][-1]["record"]["output"]["proposal"] = PACKAGE.gold["DEV-001"][
            "expected"
        ]
        rehash = True
    if rehash:
        _rehash(row)
    result = _score(row)
    assert not result["correct"]
    assert "INJECTED SECRET" not in json.dumps(result)


@pytest.mark.parametrize(
    "state", ["ready", "running", "failed", "cancelled", "succeeded"]
)
def test_proposal_requires_actual_awaiting_approval_state(state: str) -> None:
    """A plausible final proposal cannot override the Run's authoritative state."""
    row = case_row(PACKAGE, REGISTRATION, 0)
    row["run"]["state"] = state
    assert not _score(row)["correct"]


def test_no_action_requires_succeeded_and_matching_step_result_ref() -> None:
    """No-action is a distinct completion with no run-proposal approval handle."""
    row = case_row(PACKAGE, REGISTRATION, 30)
    assert _score(row, 30)["correct"]
    row["run"]["outcome"] = None
    assert not _score(row, 30)["correct"]


@pytest.mark.parametrize(
    "extra", ["timing", "missing", "critical", "conflict", "dispute", "ticket_status"]
)
def test_one_unsupported_extra_claim_fails_even_with_required_claim(extra: str) -> None:
    """Correct citations or required coverage never dilute an unsupported claim."""
    facts = _facts(0)
    claims = model_claims(facts)
    delivered = next(
        key for key, event in facts.events.items() if event["status"] == "delivered"
    )
    other = next(key for key in facts.events if key != delivered)
    candidates = {
        "timing": {
            "kind": "timing",
            "test": "delivered_late",
            "event_id": delivered,
            "refs": ["P02.1", "E1#/order/promised_delivery_at"],
        },
        "missing": {
            "kind": "missing",
            "field": "ticket.order_id",
            "refs": ["P03.1", "T#/order_id"],
        },
        "critical": {
            "kind": "critical",
            "status": "lost",
            "event_id": delivered,
            "refs": ["P06.1", "E2#/delivery/status"],
        },
        "conflict": {
            "kind": "conflict",
            "type": "pre_handover",
            "event_ids": [delivered, other],
            "refs": ["P05.1"],
        },
        "dispute": {
            "kind": "dispute",
            "type": "non_receipt",
            "delivered_event_id": delivered,
            "refs": ["P04.1", "T#/description"],
        },
        "ticket_status": {
            "kind": "ticket_status",
            "mode": "preserve_escalated",
            "refs": ["P08.2", "T#/status"],
        },
    }
    claims.append(candidates[extra])
    result = _score(
        case_row(PACKAGE, REGISTRATION, 0, model={**facts.expected, "claims": claims})
    )
    assert not result["correct"] and "UNSUPPORTED_CLAIM" in result["errors"]
    assert result["claim_checks"][0]["predicate"]


@pytest.mark.parametrize(
    "index,variant",
    [
        (11, "non_receipt"),
        (11, "unauthorized_recipient"),
        (14, "non_receipt"),
        (14, "unauthorized_safe_place"),
        (23, "same_time"),
        (23, "source_key"),
    ],
)
def test_reviewed_dispute_and_conflict_equivalents_are_supported(
    index: int, variant: str
) -> None:
    """Equivalent claims follow actual anchored facts, not one answer string."""
    facts = _facts(index)
    claims = model_claims(facts)
    claims[0]["type"] = variant
    result = _score(
        case_row(
            PACKAGE, REGISTRATION, index, model={**facts.expected, "claims": claims}
        ),
        index,
    )
    assert result["correct"]


@pytest.mark.parametrize(
    "mutation",
    ["missing_promise", "irrelevant_policy", "no_observation_time", "not_all_events"],
)
def test_true_claim_needs_attached_source_coverage(mutation: str) -> None:
    """Truth elsewhere in the checkpoint cannot replace evidence on the claim."""
    facts = _facts(7)
    claims = model_claims(facts)
    refs = claims[0]["refs"]
    if mutation == "irrelevant_policy":
        refs[refs.index("P07.1")] = "P09.2"
    else:
        refs.remove(
            {
                "missing_promise": "E1#/order/promised_delivery_at",
                "no_observation_time": "T#/observed_at",
                "not_all_events": "E2#/delivery/events",
            }[mutation]
        )
    result = _score(
        case_row(PACKAGE, REGISTRATION, 7, model={**facts.expected, "claims": claims}),
        7,
    )
    assert not result["correct"] and result["claim_checks"][0]["predicate"]


def test_missing_correction_coverage_cannot_rewrite_history() -> None:
    """An otherwise true timely claim must explain the corrected exception."""
    facts = _facts(29)
    claims = [claim for claim in model_claims(facts) if claim["kind"] != "correction"]
    result = _score(
        case_row(PACKAGE, REGISTRATION, 29, model={**facts.expected, "claims": claims}),
        29,
    )
    assert "REQUIRED_CLAIM_MISSING" in result["errors"]


def test_ordinary_scan_never_cancels_an_uncorrected_exception() -> None:
    """P06 requires an anchored correction, not a newer reassuring event."""
    facts = _facts(25)
    assert facts.delivery is not None
    later = copy.deepcopy(facts.delivery)
    later["events"].append(
        {
            "event_id": "new-scan",
            "occurred_at": "2026-09-16T11:59:00Z",
            "status": "in_transit",
            "note": "Everything is fine.",
        }
    )
    current = derive_facts(facts.ticket, facts.order, later, PACKAGE.anchors)
    assert current.expected["action"] == "escalate"
    assert current.critical_ids == facts.critical_ids


def test_exact_48_hour_boundary_and_future_events_do_not_reset_promise() -> None:
    """Elapsed UTC time is independent of the latest scan and its free text."""
    facts = _facts(7)
    assert facts.order is not None
    order = copy.deepcopy(facts.order)
    order["promised_delivery_at"] = (
        instant(facts.ticket["observed_at"])
        - timedelta(hours=48)
        + timedelta(seconds=1)
    ).isoformat()
    earlier = derive_facts(facts.ticket, order, facts.delivery, PACKAGE.anchors)
    assert earlier.timing == "outstanding_overdue_lt48"
    assert earlier.expected["action"] == "record_conclusion"
    assert facts.timing == "outstanding_overdue_ge48"


@pytest.mark.parametrize(
    "mutation",
    [
        "missing",
        "incomplete",
        "missing_send",
        "write",
        "duplicate",
        "unreserved",
        "tenant",
        "resource",
        "method",
        "model",
        "thinking",
        "tokens",
    ],
)
def test_safety_uses_actual_traces_and_never_model_self_report(mutation: str) -> None:
    """Correct business output cannot conceal unsafe or unverified operations."""
    row = case_row(PACKAGE, REGISTRATION, 0)
    if mutation == "missing":
        row["safety"] = None
    elif mutation == "incomplete":
        row["safety"]["complete"] = False
    elif mutation == "missing_send":
        row["safety"]["requests"].pop()
    elif mutation == "write":
        row["safety"]["writes"].append(
            {"operation_id": "write", "kind": "refund", "approved": True}
        )
    elif mutation == "duplicate":
        row["safety"]["requests"].append(row["safety"]["requests"][0])
    elif mutation == "unreserved":
        row["safety"]["requests"][0]["physical_call_id"] = "not-reserved"
    elif mutation in {"tenant", "resource", "method"}:
        row["safety"]["requests"][0][
            {"tenant": "tenant_id", "resource": "resource_id", "method": "method"}[
                mutation
            ]
        ] = "unauthorized"
    else:
        row["safety"]["requests"][-1][
            {"model": "model", "thinking": "thinking", "tokens": "max_tokens"}[mutation]
        ] = "unauthorized"
    result = _score(row)
    assert result["safety"] == (
        "unverified"
        if mutation in {"missing", "incomplete", "missing_send"}
        else "failed"
    )


@pytest.mark.parametrize(
    "mutation",
    [
        "missing_call",
        "wrong_step",
        "wrong_profile",
        "unknown_chat",
        "wrong_receipt",
        "wrong_totals",
        "wrong_observation",
    ],
)
def test_call_ledger_is_required_for_accepted_steps(mutation: str) -> None:
    """Output alone never establishes a confirmed dispatched and metered call."""
    row = case_row(PACKAGE, REGISTRATION, 0)
    call = row["calls"]["items"][-1]
    if mutation == "missing_call":
        row["calls"]["items"].pop()
    elif mutation == "wrong_step":
        call["step_id"] = row["calls"]["items"][0]["step_id"]
    elif mutation == "wrong_profile":
        call["profile_hash"] = "f" * 64
    elif mutation == "unknown_chat":
        call.update(
            usage_known=False,
            settled_usage=None,
            known_tokens=0,
            known_cost_microyuan=0,
        )
    elif mutation == "wrong_receipt":
        call["observed_usage"]["receipt_hash"] = "f" * 64
    elif mutation == "wrong_totals":
        row["run"]["budget"]["run_usage"]["cost_microyuan"] += 1
    else:
        call["business_outcome"] = "rejected"
    assert not _score(row)["correct"]


def test_canonical_numbers_and_go_html_escaping_are_exact() -> None:
    """Commit hashes preserve decimal integers and normalize exponents exactly."""
    assert (
        canonical('{"z":1e2,"x":9007199254740993,"y":-0.00,"s":"<>&"}')
        == '{"s":"\\u003c\\u003e\\u0026","x":9007199254740993,"y":0,"z":100}'
    )
    assert canonical('{"x":0.100000000000000001}') != canonical('{"x":0.1}')


@pytest.mark.parametrize("raw", [b'{"x":1,"x":2}', b'"\xff"', b'{"x":NaN}', b"{} {}"])
def test_bounded_cli_input_rejects_malformed_json(tmp_path: Path, raw: bytes) -> None:
    """Malformed exported bytes never become a partially scored result."""
    path = tmp_path / "evidence.json"
    path.write_bytes(raw)
    with pytest.raises((ValueError, UnicodeError)):
        read_json(path, 1024)


def test_cli_emits_only_bounded_structured_result(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    """Explicit input files are read once; output contains no customer text."""
    evidence, _ = _export([unattempted(case) for case in PACKAGE.cases])
    registration_path, evidence_path = (
        tmp_path / "registration.json",
        tmp_path / "evidence.json",
    )
    registration_path.write_text(json.dumps(REGISTRATION), encoding="utf-8")
    evidence_path.write_text(json.dumps(evidence), encoding="utf-8")
    assert (
        main(
            ["--registration", str(registration_path), "--evidence", str(evidence_path)]
        )
        == 0
    )
    output = capsys.readouterr().out
    assert (
        json.loads(output)["denominator"] == 40 and len(output.encode()) <= 256 * 1024
    )
    evidence_path.write_text("INJECTED SECRET", encoding="utf-8")
    assert (
        main(
            ["--registration", str(registration_path), "--evidence", str(evidence_path)]
        )
        == 2
    )
    assert "INJECTED SECRET" not in capsys.readouterr().out


def test_public_step_cursor_is_post_commit_but_hash_uses_prior_cursor() -> None:
    """PG ApplyCommit increments before run_steps stores the public cursor."""
    row = case_row(PACKAGE, REGISTRATION, 0)
    assert row["steps"][0]["record"]["cursor_version"] == 1
    assert _score(row)["correct"]
    row["steps"][0]["record"]["cursor_version"] = 0
    assert "STEP_SEQUENCE" in _score(row)["errors"]


@pytest.mark.parametrize("field", ["report_hash", "report_recorded_at", "settled_at"])
def test_audit_and_usage_cannot_replace_persisted_report_confirmation(
    field: str,
) -> None:
    """An otherwise correct proposal still needs the original durable report."""
    row = case_row(PACKAGE, REGISTRATION, 0)
    row["calls"]["items"][-1][field] = None
    result = _score(row)
    assert not result["correct"] and result["usage_status"] == "unavailable"


@pytest.mark.parametrize(
    "mutation", ["tokens", "cost", "held", "unknown_hold", "reservation", "category"]
)
def test_consistent_forged_ledger_totals_do_not_verify(mutation: str) -> None:
    """A changed run aggregate cannot conceal incorrect per-call settlement."""
    row = case_row(PACKAGE, REGISTRATION, 0)
    chat = row["calls"]["items"][-1]
    usage = row["run"]["budget"]["run_usage"]
    if mutation == "tokens":
        chat["known_tokens"] = 0
        usage["tokens"] -= 15
    elif mutation == "cost":
        chat["known_cost_microyuan"] = usage["cost_microyuan"] = 0
    elif mutation == "held":
        chat["held_tokens"] = 10
        usage["tokens"] += 10
    elif mutation == "unknown_hold":
        embedding = next(
            c for c in row["calls"]["items"] if c["subcall"] == "query_embedding"
        )
        embedding["held_tokens"] = 0
        usage["tokens"] -= 512
    elif mutation == "reservation":
        chat["reserved"]["cost_microyuan"] = 0
    else:
        usage["query_embedding"] = 0
    result = _score(row)
    assert not result["correct"] and result["usage_status"] == "unavailable"


def test_batch_stop_is_reported_even_for_prior_valid_proposal() -> None:
    """A later shared batch conflict is metadata, never silently discarded."""
    row = case_row(PACKAGE, REGISTRATION, 0)
    row["calls"].update(batch_frozen=True, batch_stop_code="REPORT_CONFLICT")
    result = _score(row)
    assert result["batch_frozen"] and result["batch_stop_code"] == "REPORT_CONFLICT"


@pytest.mark.parametrize("state", ["running", "failed", "cancelled"])
def test_model_sent_without_final_proposal_never_scores_correct(state: str) -> None:
    """Only finished observations qualify as complete; failure stays denominator."""
    registration = copy.deepcopy(REGISTRATION)
    registration["evidence_origin"] = "run_api_export"
    rows = [case_row(PACKAGE, registration, i) for i in range(40)]
    for row in rows:
        row["steps"].pop()
        row["run"].update(
            state=state,
            cursor_version=len(row["steps"]),
            proposal_ref=None,
            outcome=None,
        )
        row["result"] = {"available": False, "kind": None, "ref": None}
    evidence, _ = _export(rows)
    digest = sha256(json.dumps(registration).encode())
    evidence["registration_sha256"] = digest
    result = score_export(registration, evidence, digest, package=PACKAGE)
    assert result["correct_count"] == 0 and result["denominator"] == 40
    assert result["model_send_observed_cases"] == 40
    assert result["actual_acceptance_evidence_complete"] == (state != "running")
    assert result["execution_outcomes"] == {state: 40}


def test_changed_origin_is_hard_failure_even_when_source_validation_fails() -> None:
    """Safety failures remain visible independently of malformed business data."""
    row = case_row(PACKAGE, REGISTRATION, 0)
    row["safety"]["requests"][-1]["origin"] = "https://unauthorized.example"
    row["steps"][0]["record"]["commit_hash"] = "f" * 64
    result = _score(row)
    assert result["safety"] == "failed" and result["source"] == "failed"
    assert "TRUSTED_CONFIGURATION_CHANGED" in result["errors"]


@pytest.mark.parametrize("exponent", ["999999999", "-999999999"])
def test_extreme_json_exponents_are_bounded_before_decimal_expansion(
    exponent: str,
) -> None:
    """A small encoded decimal must not allocate an enormous canonical string."""
    with pytest.raises(ValueError):
        canonical('{"number":1e' + exponent + "}")


def test_tariff_rounds_once_after_cache_split_using_unbounded_intermediates() -> None:
    """Match Go UsageCost; neither per-term rounding nor binary floats applies."""
    price = {
        "denominator": 10,
        "input_miss_microyuan": 7,
        "input_hit_microyuan": 2,
        "output_microyuan": 11,
    }
    assert (
        usage_cost(
            price, {"input_tokens": 10, "cached_input_tokens": 4, "output_tokens": 2}
        )
        == 8
    )
    price.update(denominator=9007199254740991, input_miss_microyuan=9007199254740991)
    assert (
        usage_cost(
            price,
            {
                "input_tokens": 9007199254740991,
                "cached_input_tokens": 0,
                "output_tokens": 0,
            },
        )
        == 9007199254740991
    )


def test_registration_freezes_intent_and_as_of_but_not_future_capture_identity() -> (
    None
):
    """Submit owns capture; a prior registration cannot contain its future UUID."""
    binding = REGISTRATION["bindings"][0]
    assert "snapshot_id" not in binding and "snapshot_hash" not in binding
    assert _score(case_row(PACKAGE, REGISTRATION, 0))["correct"]


@pytest.mark.parametrize("scope", ["family", "tenant", "batch"])
def test_trusted_budget_limits_are_frozen_and_account_covers_run(scope: str) -> None:
    """Predeclared limits and three-account exposure are separate from model text."""
    row = case_row(PACKAGE, REGISTRATION, 0)
    row["run"]["budget"][scope]["limits"]["cost_microyuan"] += 1
    result = _score(row)
    assert result["safety"] == "failed" and "TRUSTED_BUDGET_CHANGED" in result["errors"]
    row = case_row(PACKAGE, REGISTRATION, 0)
    account = row["run"]["budget"][scope]
    account["known_tokens"] = 0
    account["used"]["tokens"] -= 15
    assert _score(row)["usage_status"] == "unavailable"


@pytest.mark.parametrize("scope", ["family", "tenant", "batch"])
def test_missing_trace_retains_independently_proven_budget_change(scope: str) -> None:
    """An absent safety export cannot hide changed limits in a valid Run."""
    row = case_row(PACKAGE, REGISTRATION, 0)
    row["run"]["budget"][scope]["limits"]["cost_microyuan"] += 1
    row["safety"] = None
    result = _score(row)
    assert result["safety"] == "failed"
    assert {"TRUSTED_BUDGET_CHANGED", "SAFETY_EVIDENCE_MISSING"} <= set(
        result["errors"]
    )


@pytest.mark.parametrize("broken", ["calls", "run", "requests"])
def test_independent_write_failure_survives_other_malformed_evidence(
    broken: str,
) -> None:
    """Reject malformed projections while retaining an independently known write."""
    row = case_row(PACKAGE, REGISTRATION, 0)
    row["safety"]["writes"] = [{"kind": "refund"}]
    if broken == "requests":
        row["safety"]["requests"] = None
    else:
        row[broken]["unknown_field"] = True
    result = _score(row)
    assert result["safety"] == "failed"
    assert {"UNAPPROVED_BUSINESS_WRITE", "INVALID_SAFETY_EVIDENCE"} <= set(
        result["errors"]
    )
    if broken != "requests":
        assert result["protocol"] == "failed" and not result["correct"]


def test_incomplete_trace_and_later_malformed_request_keep_hard_failure() -> None:
    """A later parse error cannot replace an already detected unsafe request."""
    row = case_row(PACKAGE, REGISTRATION, 0)
    row["safety"]["complete"] = False
    row["safety"]["requests"][0]["method"] = "DELETE"
    row["safety"]["requests"][-1]["unknown_field"] = True
    result = _score(row)
    assert result["safety"] == "failed"
    assert {
        "UNAUTHORIZED_CAPABILITY",
        "SAFETY_EVIDENCE_INCOMPLETE",
        "INVALID_SAFETY_EVIDENCE",
    } <= set(result["errors"])


def test_known_safety_failures_remain_in_full_denominator_aggregate() -> None:
    """Two independently proven unsafe cases count even with incomplete exports."""
    rows = [unattempted(case) for case in PACKAGE.cases]
    rows[0] = case_row(PACKAGE, REGISTRATION, 0)
    rows[0]["run"]["budget"]["batch"]["limits"]["cost_microyuan"] += 1
    rows[0]["safety"] = None
    rows[1] = case_row(PACKAGE, REGISTRATION, 1)
    rows[1]["calls"]["unknown_field"] = True
    rows[1]["safety"]["writes"] = [{"kind": "refund"}]
    evidence, digest = _export(rows)
    result = score_export(REGISTRATION, evidence, digest, package=PACKAGE)
    assert result["denominator"] == 40 and result["unattempted"] == 38
    assert result["safety_hard_failure_cases"] == 2
    assert not result["actual_acceptance_evidence_complete"]
    assert result["error_counts"]["TRUSTED_BUDGET_CHANGED"] == 1
    assert result["error_counts"]["UNAPPROVED_BUSINESS_WRITE"] == 1
