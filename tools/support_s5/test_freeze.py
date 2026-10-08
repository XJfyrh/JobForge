"""Freeze protects source and resource ceilings without authoring unseen data."""

from __future__ import annotations

import copy
import json
from pathlib import Path

import pytest

from tools.support_evaluation.evidence import EvidenceError
from tools.support_s5 import freeze
from tools.support_s5.score import THRESHOLDS


def test_new_bindings_preserve_blueprint_but_source_or_cap_change_fails(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Only listed dataset/index identities can vary after strategy freeze."""
    definition = json.loads(
        (
            Path(__file__).resolve().parents[2] / "api/support/profile-v1/fixtures.json"
        ).read_bytes()
    )["profile"]["definition"]
    definition["schema_version"] = 5
    definition["model"]["message_content_bytes"] = 65536
    definition["model"]["request_body_bytes"] = 131072
    sources = {"synthetic-source.py": "a" * 64}
    monkeypatch.setattr(freeze, "source_hashes", lambda: dict(sources))
    receipt = {
        "status": "frozen_before_unseen_creation",
        "thresholds": THRESHOLDS,
        "sources": dict(sources),
        "baseline_blueprint": freeze.blueprint(definition),
    }
    bound = copy.deepcopy(definition)
    bound["resources"]["dataset_id"] = "support-s5-2026-10-07-v1"
    bound["resources"]["seed_sha256"] = "b" * 64
    bound["resources"]["tenants"][0]["index_id"] = "new-reviewed-index-identity"
    freeze.validate_blueprint(receipt, "support_fixed_v1", freeze.blueprint(bound))
    bound["model"]["message_content_bytes"] = 131072
    with pytest.raises(EvidenceError, match="STRATEGY_BLUEPRINT_CHANGED"):
        freeze.validate_blueprint(receipt, "support_fixed_v1", freeze.blueprint(bound))
    sources["synthetic-source.py"] = "c" * 64
    with pytest.raises(EvidenceError, match="CANDIDATE_FREEZE_CHANGED"):
        freeze.validate(receipt)
