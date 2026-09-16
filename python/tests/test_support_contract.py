"""Shared Go/Python support vectors and independent Draft 2020-12 validation."""

from __future__ import annotations

import copy
import json
from pathlib import Path

import pytest
from jobforge_agent.dispatch import DispatchError
from jobforge_agent.http import strict_json
from jobforge_agent.support_contract import (
    CLAIM_VARIANTS,
    POINTERS,
    REQUESTED_FIELDS,
    parse_model_proposal,
    proposal_from_model,
    validate_persisted_proposal,
    validate_persisted_shape,
)
from jsonschema import Draft202012Validator
from jsonschema.exceptions import ValidationError
from referencing import Registry, Resource

ROOT = Path(__file__).resolve().parents[2]
FIXTURE = json.loads(
    (ROOT / "api/support/v1/fixtures.json").read_text(encoding="utf-8")
)
SCHEMA = json.loads((ROOT / "api/support/v1/schema.json").read_text(encoding="utf-8"))
REGISTRY = Registry().with_resource(SCHEMA["$id"], Resource.from_contents(SCHEMA))
MODEL_VALIDATOR = Draft202012Validator(SCHEMA, registry=REGISTRY)
PERSISTED_VALIDATOR = Draft202012Validator(
    {"$ref": SCHEMA["$id"] + "#/$defs/persistedProposal"}, registry=REGISTRY
)


def checkpoint(case: dict) -> dict:
    """Project shared source vectors into the accepted runtime field names."""
    return {
        "snapshot": copy.deepcopy(case["snapshot"]),
        "steps": [
            {
                "step": {"kind": item["kind"]},
                "result_json": copy.deepcopy(item["result_json"]),
            }
            for item in case["steps"]
        ],
    }


@pytest.mark.parametrize("case", FIXTURE["valid"], ids=lambda case: case["name"])
def test_shared_valid_matches_exact_go_rendering_and_schema(case: dict) -> None:
    """Preserve every model assertion while matching all Go-generated fields."""
    protected = checkpoint(case)
    before = copy.deepcopy(protected)
    model = strict_json(case["model_json"].encode("utf-8"))
    MODEL_VALIDATOR.validate(model)
    actual = parse_model_proposal(case["model_json"].encode("utf-8"), protected)
    assert actual == case["expected"]
    PERSISTED_VALIDATOR.validate(actual)
    validate_persisted_proposal(actual, protected)
    assert protected == before


@pytest.mark.parametrize("case", FIXTURE["invalid"], ids=lambda case: case["name"])
def test_shared_invalid_sources_and_models_fail_closed(case: dict) -> None:
    """Source provenance failures remain invalid even when their schema is valid."""
    with pytest.raises(DispatchError):
        parse_model_proposal(case["model_json"].encode("utf-8"), checkpoint(case))


def test_source_schema_is_valid_and_matches_closed_runtime_vocabularies() -> None:
    """Check the authored schema with an independent standards implementation."""
    Draft202012Validator.check_schema(SCHEMA)
    defs = SCHEMA["$defs"]
    assert defs["requestedField"]["enum"] == list(REQUESTED_FIELDS)
    assert defs["shortRef"]["oneOf"][0]["enum"] == [
        prefix + "#" + pointer
        for prefix, pointers in POINTERS.items()
        for pointer in pointers
    ]
    assert {
        variant["properties"]["kind"]["const"]
        for variant in defs["claimVariant"]["oneOf"]
    } == set(CLAIM_VARIANTS)
    for alias in ("P01.1", "P01.2", "P09.1", "P10.2"):
        Draft202012Validator(
            {"$ref": SCHEMA["$id"] + "#/$defs/shortRef"}, registry=REGISTRY
        ).validate(alias)


@pytest.mark.parametrize(
    "name",
    [
        "unknown_top_field",
        "free_summary",
        "alias_key",
        "missing_key",
        "null_decision",
        "null_fields",
        "null_array_item",
        "unknown_requested_field",
        "duplicate_requested_field",
        "unknown_action",
        "no_action_write",
        "unknown_conclusion",
        "claim_extra_field",
        "claim_alias_key",
        "claim_null_scalar",
        "invalid_event_identifier",
        "unknown_test",
        "unknown_policy",
        "duplicate_reference",
        "source_pointer_not_allowed",
        "raw_event_pointer",
        "foreign_full_reference",
        "no_claims",
        "duplicate_claim",
        "too_many_claims",
        "conflict_single_event",
        "conflict_duplicate_event",
        "order_conflict_has_event",
    ],
)
def test_independent_schema_rejects_shared_structural_invalidity(name: str) -> None:
    """Independent schema checking catches drift in the handwritten runtime parser."""
    case = next(case for case in FIXTURE["invalid"] if case["name"] == name)
    with pytest.raises(ValidationError):
        MODEL_VALIDATOR.validate(json.loads(case["model_json"]))


@pytest.mark.parametrize(
    "change", ["extra", "summary", "pointer", "refs", "claim", "null", "wrong_target"]
)
def test_stored_proposal_cannot_change_template_or_provenance(change: str) -> None:
    """Persistent output must round-trip through the same trusted source expansion."""
    case = FIXTURE["valid"][0]
    proposal = copy.deepcopy(case["expected"])
    if change == "extra":
        proposal["url"] = "https://invalid.example"
    elif change == "summary":
        proposal["summary"] = "Approved, refunded and resolved."
    elif change == "pointer":
        proposal["claims"][0]["refs"][-1]["source_pointer"] = "/delivery/events/99"
    elif change == "refs":
        proposal["evidence_refs"].reverse()
    elif change == "claim":
        proposal["claims"].append(copy.deepcopy(proposal["claims"][0]))
    elif change == "null":
        proposal["requested_fields"] = None
    else:
        proposal["target_ticket_status"] = "open"
    with pytest.raises(DispatchError):
        validate_persisted_proposal(proposal, checkpoint(case))


@pytest.mark.parametrize(
    "raw", [b"\xff", b'{"x":"\\ud800"}', b'{"x":NaN}', b"{}{}", b"[]"]
)
def test_raw_model_boundary_rejects_unambiguous_parser_failures(raw: bytes) -> None:
    """The schema engine cannot substitute for strict byte-level JSON parsing."""
    with pytest.raises(DispatchError):
        parse_model_proposal(raw, checkpoint(FIXTURE["valid"][0]))


def test_output_limits_are_terminal_size_facts() -> None:
    """An oversized model cannot request a protocol correction or be truncated."""
    with pytest.raises(DispatchError) as error:
        parse_model_proposal(b" " * 16385, checkpoint(FIXTURE["valid"][0]))
    assert error.value.fact == "size_limit" and error.value.stop


def test_runtime_retains_a_semantically_unsupported_assertion() -> None:
    """Production validates structure and provenance, never reads an answer key."""
    case = FIXTURE["valid"][0]
    value = json.loads(case["model_json"])
    value["conclusion"] = "conflicting"
    actual = proposal_from_model(value, checkpoint(case))
    assert actual["conclusion"] == "conflicting"
    assert "Conclusion asserted: conflicting" in actual["summary"]
    validate_persisted_shape(actual)


@pytest.mark.parametrize(
    "field,value",
    [
        ("status", []),
        ("status", 7),
        ("note", {}),
        ("occurred_at", "tomorrow"),
        ("occurred_at", "2026-09-16"),
        ("occurred_at", "2026-02-31T08:00:00Z"),
    ],
)
def test_source_event_scalars_match_go_typed_decode(field: str, value: object) -> None:
    """A valid event identity cannot authorize ill-typed or invalid source facts."""
    case = FIXTURE["valid"][0]
    protected = checkpoint(case)
    protected["steps"][1]["result_json"]["content"]["delivery"]["events"][0][field] = (
        value
    )
    with pytest.raises(DispatchError):
        parse_model_proposal(case["model_json"].encode("utf-8"), protected)


@pytest.mark.parametrize(
    "field",
    [
        "decision",
        "action",
        "conclusion",
        "requested_fields",
        "target_ticket_status",
        "claims",
    ],
)
@pytest.mark.parametrize("value", [None, True, 7, {}, ["bad"]])
def test_invalid_field_types_fail_with_fixed_errors(field: str, value: object) -> None:
    """Malformed model fields cannot leak exceptions or loosen the schema."""
    case = FIXTURE["valid"][0]
    model = json.loads(case["model_json"])
    model[field] = value
    with pytest.raises(DispatchError):
        proposal_from_model(model, checkpoint(case))
