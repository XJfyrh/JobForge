"""Fresh S4 preparation binds source artifacts without modifying frozen S1–S3 inputs."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path

import pytest
from jsonschema import Draft202012Validator  # type: ignore[import-untyped]

from tools.support_approval.sources import template

ROOT = Path(__file__).resolve().parents[2]


def test_fresh_source_profile_and_public_identity() -> None:
    """The new schema and runtime share exact opt-in identity, never a private seed."""
    historical = {
        name: hashlib.sha256((ROOT / "deploy" / name).read_bytes()).hexdigest()
        for name in (
            "support-cloud.source.example.json",
            "support-agent.source.example.json",
            "support-recovery.source.example.json",
        )
    }
    value = template(
        ROOT, "2026-10-05", "a" * 64, "http://business:8092", "fixture-key", "b" * 64
    )
    schema = json.loads(
        (ROOT / "api/support/approval-v1/profile-schema.json").read_bytes()
    )
    Draft202012Validator(schema).validate(value["definition"])
    assert (
        value["definition"]["program"]["recovery_policy"] == "confirmed_uncommitted_v1"
    )
    assert all(
        not value[name]["sha256"]
        for name in ("build_receipt", "data_review", "scoring_review")
    )
    assert all(
        hashlib.sha256((ROOT / "deploy" / name).read_bytes()).hexdigest() == digest
        for name, digest in historical.items()
    )


@pytest.mark.parametrize(
    "origin",
    [
        "http://business/path",
        "http://user:secret@business",
        "https://business?x=1",
        "file://business",
        "http://business/",
    ],
)
def test_source_rejects_action_endpoint_selection(origin: str) -> None:
    """Only a fixed deployment origin is allowed; not a runtime/model URL."""
    with pytest.raises(ValueError):
        template(ROOT, "2026-10-05", "a" * 64, origin, "fixture-key", "b" * 64)
