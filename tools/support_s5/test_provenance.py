"""Synthetic provenance contracts; these tests do not author formal cases."""

from __future__ import annotations

import copy
import json
from pathlib import Path
from typing import Any

import pytest

from tools.support_evaluation.evidence import EvidenceError
from tools.support_evaluation.validate_data import sha256
from tools.support_s5.freeze import seen_package
from tools.support_s5.package import DATASET, DATASET_V2, DATASET_V3, prior_families


@pytest.mark.parametrize(
    "mutation", ["none", "missing", "duplicate", "hash", "denied", "invalid"]
)
def test_v3_review_binds_both_previously_seen_cohorts(mutation: str) -> None:
    """Neither renaming nor omitting an old package establishes novelty."""
    packages = [
        {
            "dataset_id": DATASET,
            "manifest_sha256": "a" * 64,
            "template_families": ["old-a"],
        },
        {
            "dataset_id": DATASET_V2,
            "manifest_sha256": "b" * 64,
            "template_families": ["old-b"],
        },
    ]
    freeze = {"seen_formal_packages": copy.deepcopy(packages)}
    review: dict[str, Any] = {
        "seen_formal_manifest_sha256_by_dataset": {
            p["dataset_id"]: p["manifest_sha256"] for p in packages
        },
        "novelty_against_seen_formals_confirmed": True,
    }
    if mutation == "missing":
        freeze["seen_formal_packages"].pop()
    elif mutation == "duplicate":
        freeze["seen_formal_packages"][1] = copy.deepcopy(packages[0])
    elif mutation == "hash":
        review["seen_formal_manifest_sha256_by_dataset"][DATASET_V2] = "c" * 64
    elif mutation == "denied":
        review["novelty_against_seen_formals_confirmed"] = False
    elif mutation == "invalid":
        freeze["seen_formal_packages"][1] = {}
    if mutation == "none":
        assert prior_families(DATASET_V3, review, freeze) == {"old-a", "old-b"}
    else:
        with pytest.raises(EvidenceError, match="SEEN_FORMAL_"):
            prior_families(DATASET_V3, review, freeze)


def test_seen_package_preserves_original_review_binding(tmp_path: Path) -> None:
    """A filename cannot replace the actual reviewed manifest and family bytes."""
    review = {
        "status": "independently_accepted",
        "cases": [{"template_family": "unit-a"}],
    }
    raw = json.dumps(review).encode()
    path = tmp_path / "evaluation" / "family-review.json"
    path.parent.mkdir()
    path.write_bytes(raw)
    manifest = {
        "dataset_id": DATASET_V2,
        "artifact_sha256": {"evaluation/family-review.json": sha256(raw)},
    }
    manifest_path = tmp_path / "manifest.json"
    manifest_raw = json.dumps(manifest).encode()
    manifest_path.write_bytes(manifest_raw)
    assert seen_package(manifest_path, DATASET_V2) == {
        "dataset_id": DATASET_V2,
        "manifest_sha256": sha256(manifest_raw),
        "template_families": ["unit-a"],
    }
    with pytest.raises(EvidenceError, match="SEEN_FORMAL_PROVENANCE"):
        seen_package(manifest_path, DATASET)
    path.write_bytes(raw + b" ")
    with pytest.raises(EvidenceError, match="SEEN_FORMAL_PROVENANCE"):
        seen_package(manifest_path, DATASET_V2)
