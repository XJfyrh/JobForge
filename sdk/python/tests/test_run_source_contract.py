"""Verify shared fixtures and SDK field shapes against the reviewed OpenAPI."""

from __future__ import annotations

import json
import re
from dataclasses import fields
from pathlib import Path
from typing import Any

import pytest
import yaml  # type: ignore[import-untyped]  # Test-only OpenAPI YAML loader.

from jobforge import run_models

SOURCE = Path(__file__).resolve().parents[3] / "api/run/v2"
OPENAPI = yaml.safe_load((SOURCE / "openapi.yaml").read_text(encoding="utf-8"))
FIXTURES = json.loads((SOURCE / "fixtures.json").read_text(encoding="utf-8"))


def check_schema(value: Any, schema: dict[str, Any]) -> None:
    """Check the finite source-schema vocabulary used by these public fixtures."""
    if "$ref" in schema:
        check_schema(
            value, OPENAPI["components"]["schemas"][schema["$ref"].split("/")[-1]]
        )
        return
    if "anyOf" in schema:
        for option in schema["anyOf"]:
            try:
                check_schema(value, option)
                return
            except AssertionError:
                continue
        raise AssertionError("value does not match nullable schema")
    kind = schema.get("type")
    if kind == "object":
        assert isinstance(value, dict)
        assert set(schema["required"]) <= set(value)
        if schema.get("additionalProperties") is False:
            assert set(value) <= set(schema["properties"])
        for key, item in value.items():
            check_schema(item, schema["properties"][key])
    elif kind == "array":
        assert isinstance(value, list) and len(value) <= schema["maxItems"]
        for item in value:
            check_schema(item, schema["items"])
    elif kind == "integer":
        assert type(value) is int
        assert schema.get("minimum", 0) <= value <= schema.get("maximum", (1 << 53) - 1)
    elif kind == "boolean":
        assert type(value) is bool
    elif kind == "string":
        assert isinstance(value, str)
        assert len(value) >= schema.get("minLength", 0)
        assert len(value) <= schema.get("maxLength", (1 << 53) - 1)
        if "pattern" in schema:
            assert re.fullmatch(schema["pattern"], value) is not None
    elif kind == "null":
        assert value is None
    else:
        assert schema == {}, "unsupported schema keyword requires extending the check"
    if "enum" in schema:
        assert value in schema["enum"]
    if "const" in schema:
        assert value == schema["const"]


@pytest.mark.parametrize(
    ("fixture", "schema"),
    [
        ("submit", "SubmitRun"),
        ("submit_explicit_default", "SubmitRun"),
        ("retry", "RetryRun"),
        ("retry_explicit_default", "RetryRun"),
        ("cancel", "CancelRun"),
        ("run", "Run"),
        ("submission", "RunSubmission"),
        ("cancellation", "RunCancellation"),
        ("page", "RunPage"),
        ("steps", "RunStepPage"),
        ("events", "RunEventPage"),
        ("result", "RunResult"),
        ("proposal_result", "RunResult"),
    ],
)
def test_shared_request_response_fixture_matches_source(
    fixture: str, schema: str
) -> None:
    """One fixture corpus is consumed by both Go and installed SDK contracts."""
    check_schema(FIXTURES[fixture], OPENAPI["components"]["schemas"][schema])
    model = getattr(run_models, schema, None)
    if model is not None:
        model.from_dict(FIXTURES[fixture])


def test_all_sdk_wire_fields_match_the_source_exactly() -> None:
    """Nullable source fields remain required; no SDK-only response contract."""
    for name, schema in OPENAPI["components"]["schemas"].items():
        model = getattr(run_models, name, None)
        if model is None or schema.get("type") != "object":
            continue
        assert {field.name for field in fields(model)} == set(schema["properties"])
        assert set(schema["required"]) == set(schema["properties"])


def test_source_exposes_no_worker_or_approval_write_api() -> None:
    """ADR-0020 adds one read route and no execution or approval mutation."""
    assert len(OPENAPI["paths"]) == 8
    assert sum(len(path) for path in OPENAPI["paths"].values()) == 9
    assert set(OPENAPI["paths"]["/v2/runs/{run_id}/calls"]) == {"get"}
    for path in OPENAPI["paths"]:
        assert all(
            word not in path for word in ("approval", "reserve", "settle", "worker")
        )
    for entry in FIXTURES["errors"]:
        check_schema(entry["body"], OPENAPI["components"]["schemas"]["ErrorEnvelope"])
