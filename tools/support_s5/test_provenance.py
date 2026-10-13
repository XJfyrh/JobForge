"""Synthetic provenance contracts; these tests do not author formal cases."""

from __future__ import annotations

import copy
import json
from pathlib import Path
from typing import Any

import pytest

from tools.support_evaluation.evidence import EvidenceError
from tools.support_evaluation.validate_data import sha256
from tools.support_s5 import package
from tools.support_s5.freeze import seen_package
from tools.support_s5.package import DATASET, DATASET_V2, DATASET_V3, prior_families

# A placeholder id for the cohort ADR-0034 has not authored yet. No case data is
# created here: this only exercises the binding that admits one.
BOUND_V4 = "support-s5-2026-10-20-v4"


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


def three_seen_packages() -> list[dict[str, Any]]:
    """The three formal cohorts the ADR-0034 candidate has already seen."""
    return [
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
        {
            "dataset_id": DATASET_V3,
            "manifest_sha256": "c" * 64,
            "template_families": ["old-c"],
        },
    ]


def v4_review(packages: list[dict[str, Any]]) -> dict[str, Any]:
    """One independently accepted novelty review over all three seen cohorts."""
    return {
        "seen_formal_manifest_sha256_by_dataset": {
            p["dataset_id"]: p["manifest_sha256"] for p in packages
        },
        "novelty_against_seen_formals_confirmed": True,
    }


@pytest.mark.parametrize(
    "mutation", ["none", "missing", "duplicate", "hash", "denied", "invalid"]
)
def test_unseen_v5_cohort_binds_every_prior_formal(
    monkeypatch: pytest.MonkeyPatch, mutation: str
) -> None:
    """The first unseen cohort of schema 13 is novel against v1, v2 AND v3."""
    monkeypatch.setattr(package, "DATASET_V4", BOUND_V4)
    packages = three_seen_packages()
    freeze: dict[str, Any] = {"seen_formal_packages": copy.deepcopy(packages)}
    review = v4_review(packages)
    if mutation == "missing":
        freeze["seen_formal_packages"].pop()
    elif mutation == "duplicate":
        freeze["seen_formal_packages"][2] = copy.deepcopy(packages[0])
    elif mutation == "hash":
        review["seen_formal_manifest_sha256_by_dataset"][DATASET_V3] = "d" * 64
    elif mutation == "denied":
        review["novelty_against_seen_formals_confirmed"] = False
    elif mutation == "invalid":
        freeze["seen_formal_packages"][2] = {}
    if mutation == "none":
        # load_package unions this with the development families, so the guard
        # checks novelty against both the 40-case development set and all three
        # seen formal cohorts.
        assert prior_families(BOUND_V4, review, freeze) == {"old-a", "old-b", "old-c"}
    else:
        with pytest.raises(EvidenceError, match="SEEN_FORMAL_"):
            prior_families(BOUND_V4, review, freeze)


def test_unbound_unseen_cohort_is_not_admitted() -> None:
    """While no cohort is authored, no unseen dataset can load at all."""
    assert package.DATASET_V4 is None
    assert (
        package.DATASETS == package.SEEN_DATASETS == {DATASET, DATASET_V2, DATASET_V3}
    )


@pytest.mark.parametrize(
    "dataset", ["support-s5-2026-10-20-v4", "support-s5-renamed-v3", ""]
)
def test_unknown_dataset_fails_closed_instead_of_yielding_no_prior_families(
    dataset: str,
) -> None:
    """An unrecognised cohort must be rejected, never treated as novel."""
    packages = three_seen_packages()
    freeze: dict[str, Any] = {"seen_formal_packages": packages}
    review = v4_review(packages)
    with pytest.raises(EvidenceError, match="UNKNOWN_DATASET_PRIOR_FAMILIES"):
        prior_families(dataset, review, freeze)
    # Binding the id is what makes the same input admissible; nothing else does.
    assert package.DATASET_V4 is None


def test_first_unseen_cohort_v1_has_no_prior_formal_and_still_loads() -> None:
    """Failing closed on unknown ids must not reject the historical v1 cohort.

    v1 was the first unseen cohort, so it binds no prior formal package; its
    only priors are the development families that load_package unions in itself.
    Re-scoring the already-seen v1 set is a supported `--seen-diagnostic` path,
    so this must keep returning an empty set rather than raising.
    """
    assert prior_families(DATASET, {}, {}) == set()
    # A v1 review carrying v2-only provenance fields is still just v1: there is
    # nothing prior to bind, and inventing a requirement would break replays.
    assert (
        prior_families(DATASET, {"seen_formal_manifest_sha256": "a" * 64}, {}) == set()
    )
    # The neighbouring cohorts keep their real provenance requirements.
    with pytest.raises(EvidenceError, match="SEEN_FORMAL_NOVELTY_REVIEW"):
        prior_families(DATASET_V2, {}, {})
    with pytest.raises(EvidenceError, match="SEEN_FORMAL_PROVENANCE_REQUIRED"):
        prior_families(DATASET_V3, {}, {})


@pytest.mark.parametrize("dataset", sorted(package.SEEN_DATASETS))
@pytest.mark.parametrize(
    "historical,schema",
    [
        (False, 13),
        (True, 13),
        (False, 14),
        (True, 14),
        (False, 15),
        (True, 15),
        (False, 9),
    ],
)
def test_rebound_seen_manifest_is_not_unseen_for_schema_13(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    dataset: str,
    historical: bool,
    schema: int,
) -> None:
    """Reject old ids before reading their artifacts; keep historical routing."""
    from tools.support_s5 import freeze as freeze_module
    from tools.support_s5.score import THRESHOLDS

    freeze = {
        "status": "frozen_before_unseen_creation",
        "candidate_blueprint": {"schema_version": schema},
        "thresholds": THRESHOLDS,
        "frozen_at": "2026-10-13T00:00:00Z",
    }
    raw = json.dumps(freeze).encode()
    freeze_path = tmp_path / "freeze.json"
    freeze_path.write_bytes(raw)
    manifest = {
        "schema_version": 1,
        "dataset_id": dataset,
        "case_count": 20,
        "authored_at": "2026-10-13T00:01:00Z",
        "freeze_sha256": sha256(raw),
        "artifact_sha256": {name: "a" * 64 for name in package.ARTIFACT_NAMES},
        "corpus_sha256": "a" * 64,
    }
    (tmp_path / "manifest.json").write_text(json.dumps(manifest), encoding="utf-8")
    monkeypatch.setattr(freeze_module, "validate", lambda _: None)
    if schema in {13, 14, 15, 16} and not historical:
        with pytest.raises(EvidenceError, match="SEEN_DATASET_NOT_UNSEEN"):
            package.load_package(tmp_path, freeze_path, historical=historical)
    else:
        # The real loader got past dataset routing and attempted the artifacts.
        with pytest.raises(FileNotFoundError):
            package.load_package(tmp_path, freeze_path, historical=historical)


@pytest.mark.parametrize("dataset", sorted(package.SEEN_DATASETS))
@pytest.mark.parametrize("strategy", ["support_agent_v1", "support_fixed_v1"])
@pytest.mark.parametrize("schema", [13, 14, 15, 16])
def test_direct_formal_score_cannot_bypass_seen_dataset_guard(
    monkeypatch: pytest.MonkeyPatch,
    dataset: str,
    strategy: str,
    schema: int,
) -> None:
    """A caller supplying an already loaded Package still cannot relabel it."""
    from tools.support_evaluation.evidence import load_package
    from tools.support_s5 import score

    monkeypatch.setattr(score, "registration", lambda *a, **kw: {})
    registered = {
        "dataset_version": dataset,
        "profile": {"strategy": strategy},
        "strategy_blueprint": {},
    }
    with pytest.raises(EvidenceError, match="SEEN_DATASET_NOT_UNSEEN"):
        score.score_export(
            registered,
            {},
            "a" * 64,
            freeze={"candidate_blueprint": {"schema_version": schema}},
            package=load_package(),
        )
