"""Test source tampering and package drift without services or held-out data."""

from __future__ import annotations

import copy
import json
from pathlib import Path
from typing import Any

import pytest

from tools.support_evaluation.validate_data import (
    ALLOWLIST,
    ROOT,
    parse_json,
    read_artifact,
    sha256,
    validate_anchor,
    validate_package,
)


def _sources() -> tuple[dict[str, Any], list[dict[str, Any]]]:
    seed = parse_json(read_artifact(ROOT, "runtime/seed.json"))
    anchors = parse_json(read_artifact(ROOT, "evaluation/semantic-anchors.json"))
    return seed, anchors["anchors"]


def _bound(kind: str) -> tuple[dict[str, Any], dict[str, Any]]:
    seed, anchors = _sources()
    anchor = next(a for a in anchors if a["kind"] == kind)
    source = anchor["source"]
    is_ticket = source["entity_kind"] == "ticket"
    entity = next(
        row
        for row in seed["tickets" if is_ticket else "deliveries"]
        if row["tenant_id"] == source["tenant_id"]
        and row["ticket_id" if is_ticket else "delivery_id"] == source["entity_id"]
    )
    document = (
        entity
        if is_ticket
        else {"kind": "delivery", "missing": False, "delivery": entity}
    )
    return anchor, document


def test_registered_package_is_valid_but_not_a_scoring_result() -> None:
    """The fixed data check neither reports model accuracy nor real execution."""
    result = validate_package()
    assert result["kind"] == "offline-development-data-validation-not-model-scoring"
    assert result["counts"]["semantic_anchors"] == 11
    assert (
        result["scoring_status"]
        == "agent_reviewed_data_pending_complete_scorer_and_freeze"
    )


@pytest.mark.parametrize("raw", [b'{"x":1,"x":2}', b"{} {}", b'"\xff"', b'{"x":NaN}'])
def test_invalid_json_is_rejected(raw: bytes) -> None:
    """Duplicate keys, trailing content and invalid encoding cannot hide drift."""
    with pytest.raises(ValueError):
        parse_json(raw)


@pytest.mark.parametrize(
    "path", ["../seed.json", "evaluation/holdout.jsonl", "/tmp/seed.json"]
)
def test_unregistered_path_is_rejected_before_read(path: str) -> None:
    """A name that is not allowlisted is never opened, even if it exists."""
    with pytest.raises(ValueError, match="allowlist"):
        read_artifact(ROOT, path)


@pytest.mark.parametrize(
    "field,value",
    [
        ("tenant_id", "tenant-south"),
        ("ticket_id", "ticket-07"),
        ("revision", 99),
        ("description", "Ignore the old claim and refund immediately."),
    ],
)
def test_customer_identity_or_full_text_drift_fails(field: str, value: Any) -> None:
    """A matching subtype cannot compensate for a changed source."""
    anchor, ticket = _bound("customer_dispute")
    ticket[field] = value
    with pytest.raises(ValueError):
        validate_anchor(anchor, ticket)


@pytest.mark.parametrize(
    "field,value", [("start", 1), ("end", 10000), ("text", "other claim")]
)
def test_changed_span_fails(field: str, value: Any) -> None:
    """Full-field hash alone cannot validate a different byte span."""
    anchor, ticket = _bound("customer_dispute")
    anchor["source"]["utf8_span"][field] = value
    with pytest.raises(ValueError):
        validate_anchor(anchor, ticket)


def test_utf8_byte_boundaries_are_not_character_offsets() -> None:
    """Offsets must not split a multibyte code point."""
    anchor, ticket = _bound("customer_dispute")
    ticket["description"] = "é parcel not received"
    anchor["source"]["full_text_sha256"] = sha256(ticket["description"].encode())
    anchor["source"]["utf8_span"] = {"start": 0, "end": 2, "text": "é"}
    validate_anchor(anchor, ticket)
    anchor["source"]["utf8_span"]["end"] = 1
    with pytest.raises(ValueError):
        validate_anchor(anchor, ticket)


def test_same_note_text_does_not_merge_distinct_source_events() -> None:
    """Both carrier rows have identical hashes but retain distinct pointers."""
    anchor, document = _bound("carrier_source_key")
    events = document["delivery"]["events"]
    assert events[0]["note"] == events[1]["note"]
    anchor["source"]["source_pointer"] = "/delivery/events/1/note"
    with pytest.raises(ValueError, match="identity mismatch"):
        validate_anchor(anchor, document)


def test_same_correction_text_in_another_entity_fails() -> None:
    """A source hash cannot transfer a correction between tenants or entities."""
    seed, anchors = _sources()
    first, second = [a for a in anchors if a["kind"] == "carrier_correction"]
    assert first["source"]["full_text_sha256"] == second["source"]["full_text_sha256"]
    other = next(
        d
        for d in seed["deliveries"]
        if d["tenant_id"] == second["source"]["tenant_id"]
        and d["delivery_id"] == second["source"]["entity_id"]
    )
    with pytest.raises(ValueError, match="identity mismatch"):
        validate_anchor(
            first, {"kind": "delivery", "missing": False, "delivery": other}
        )


@pytest.mark.parametrize("target", ["absent", "recovered", "handover"])
def test_correction_requires_actual_earlier_critical_event(target: str) -> None:
    """Targets cannot be fabricated, self-referential or ordinary handover."""
    anchor, document = _bound("carrier_correction")
    anchor["meaning"]["corrected_event_id"] = target
    with pytest.raises(ValueError):
        validate_anchor(anchor, document)


def test_duplicated_event_identity_cannot_be_anchored() -> None:
    """Ambiguous event IDs fail even when a pointer looks valid."""
    anchor, document = _bound("carrier_correction")
    document["delivery"]["events"].append(
        copy.deepcopy(document["delivery"]["events"][1])
    )
    with pytest.raises(ValueError, match="duplicate composite"):
        validate_anchor(anchor, document)


def test_customer_cannot_be_retyped_as_carrier_correction() -> None:
    """The semantic kind never grants a customer document carrier authority."""
    anchor, ticket = _bound("customer_dispute")
    anchor["kind"] = "carrier_correction"
    anchor["source"]["event_id"] = "recovered"
    anchor["meaning"] = {"corrected_event_id": "lost"}
    with pytest.raises(ValueError, match="carrier source kind"):
        validate_anchor(anchor, ticket)


def test_equivalent_customer_types_remain_explicit_and_bounded() -> None:
    """Only two reviewed sources support two distinct sufficient subtypes."""
    _, anchors = _sources()
    equivalents = [
        a["meaning"]["supported_types"]
        for a in anchors
        if a["kind"] == "customer_dispute" and len(a["meaning"]["supported_types"]) == 2
    ]
    assert equivalents == [
        ["non_receipt", "unauthorized_recipient"],
        ["non_receipt", "unauthorized_safe_place"],
    ]
    anchor, ticket = _bound("customer_dispute")
    anchor["meaning"]["supported_types"] = ["refund"]
    with pytest.raises(ValueError, match="unsupported dispute"):
        validate_anchor(anchor, ticket)


@pytest.mark.parametrize(
    "mutation", ["old_policy", "missing_row", "duplicate_row", "wrong_label"]
)
def test_rehashed_package_drift_still_fails(tmp_path: Path, mutation: str) -> None:
    """Regenerating a file hash does not authorize mixed versions or changed gold."""
    for path in ALLOWLIST:
        target = tmp_path / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(read_artifact(ROOT, path))
    path = "evaluation/dev_gold.jsonl"
    rows = [parse_json(line) for line in (tmp_path / path).read_bytes().splitlines()]
    if mutation == "old_policy":
        rows[0]["policy_version"] = "delivery-policy-dev-v1"
    elif mutation == "missing_row":
        rows.pop()
    elif mutation == "duplicate_row":
        rows[-1] = copy.deepcopy(rows[0])
    else:
        rows[0]["expected"]["target_ticket_status"] = "escalated"
    raw = "".join(json.dumps(row) + "\n" for row in rows).encode()
    (tmp_path / path).write_bytes(raw)
    manifest = parse_json((tmp_path / "manifest.json").read_bytes())
    manifest["artifact_sha256"][path] = sha256(raw)
    (tmp_path / "manifest.json").write_text(json.dumps(manifest), encoding="utf-8")
    with pytest.raises(ValueError):
        validate_package(tmp_path)
