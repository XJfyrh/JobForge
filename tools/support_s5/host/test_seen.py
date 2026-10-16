"""Closed routing for already-seen v3; no new formal cases or model requests."""

from __future__ import annotations

import json
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest

from tools.support_evaluation.assemble import read, save_new
from tools.support_s5.host import config, health, workflow
from tools.support_s5.host.test_workflow import inputs, registration
from tools.support_s5.package import DATASET_V3


@pytest.mark.parametrize("mutation", ["none", "reverse", "drop", "threshold"])
@pytest.mark.parametrize("schema", [2, 3])
def test_seen_config_binds_complete_order_and_fixed_quality_gates(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, mutation: str, schema: int
) -> None:
    """Neither20-row mode can select cases or weaken predeclared quality gates."""
    ids = [f"{'UNIT' if schema == 3 else 'S5V3'}-{i:03d}" for i in range(1, 21)]
    monkeypatch.setattr(
        config,
        "evaluation_package",
        lambda _: SimpleNamespace(cases=dict.fromkeys(ids)),
    )
    save_new(tmp_path / "prior.json", {"phases": []})
    settings: dict[str, Any] = {
        "schema_version": schema,
        "question": "Synthetic guard?",
        "hypothesis": "Original whole list.",
        "case_ids": ids.copy(),
        "source": str(tmp_path / "source.json"),
        "build_receipt": str(tmp_path / "build.json"),
        "prior_ledger": str(tmp_path / "prior.json"),
        "inventory": [
            {"container": "synthetic-pg", "user": "owner", "database": "new"}
        ],
        "control_database": "new",
        "business_database": "business",
        "business_container": "synthetic-business",
        "network": "synthetic",
        "container_prefix": "synthetic",
        "secrets": str(tmp_path / "private"),
        "identity": {},
        "images": {
            name: "sha256:" + "a" * 64
            for name in (
                "production_worker",
                "approval_operator",
                "s5_operator",
                "business",
            )
        },
        "policy": dict(config.SEEN_V3_POLICY),
        "stage_lock": str(tmp_path / "stage.lock"),
        "formal_v4" if schema == 3 else "seen_v3": {
            "package": str(tmp_path / "original"),
            "freeze": str(tmp_path / "original-freeze.json"),
        },
    }
    expected = None
    if mutation == "reverse":
        settings["case_ids"].reverse()
        expected = "FORMAL_V4_LIST" if schema == 3 else "SEEN_V3_LIST"
    elif mutation == "drop":
        settings["case_ids"].pop()
        expected = "FORMAL_V4_LIST" if schema == 3 else "SEEN_V3_LIST"
    elif mutation == "threshold":
        settings["policy"]["minimum_full_evidence"] = 17
        expected = "QUALITY_POLICY_CHANGED"
    path = tmp_path / "config.json"
    save_new(path, settings)
    if expected:
        with pytest.raises(ValueError, match=expected):
            config.load(path)
    else:
        assert config.load(path) == settings


@pytest.mark.parametrize(
    "mutation",
    [
        "schema",
        "strategy",
        "dataset",
        "case_map",
        "map_hash",
        "manifest_path",
        "manifest_hash",
        "manifest_resource",
        "seed_path",
        "seed_hash",
        "seed_resource",
    ],
)
def test_seen_source_rejects_rebound_scope_before_any_build_or_database(
    tmp_path: Path, mutation: str
) -> None:
    """Only the current Agent18 and the original reviewed map can enter this mode."""
    original = tmp_path / "original"
    (original / "evaluation").mkdir(parents=True)
    case_map = original / "evaluation/case-map.jsonl"
    case_map.write_text("Synthetic provenance bytes; no case dataset.")
    (original / "runtime").mkdir()
    runtime = {}
    for key, relative in (
        ("manifest", "runtime/dataset-manifest.json"),
        ("seed", "runtime/seed.json"),
    ):
        path = original / relative
        path.write_text("Synthetic runtime provenance: " + key)
        runtime[key] = {"path": str(path), "sha256": config.digest(path)}
    source: dict[str, Any] = {
        "schema_version": 2,
        "definition": {
            "schema_version": 18,
            "program": {"strategy": "support_agent_v1"},
            "resources": {
                "dataset_id": DATASET_V3,
                "runtime_manifest_sha256": runtime["manifest"]["sha256"],
                "seed_sha256": runtime["seed"]["sha256"],
            },
        },
        "case_map": {"path": str(case_map), "sha256": config.digest(case_map)},
        "runtime_data": runtime,
    }
    if mutation == "schema":
        source["definition"]["schema_version"] = 17
    elif mutation == "strategy":
        source["definition"]["program"]["strategy"] = "support_fixed_v1"
    elif mutation == "dataset":
        source["definition"]["resources"]["dataset_id"] = "synthetic-new-cohort"
    elif mutation == "case_map":
        source["case_map"]["path"] = str(tmp_path / "other.jsonl")
    elif mutation == "map_hash":
        source["case_map"]["sha256"] = "a" * 64
    else:
        key, field = mutation.split("_")
        if field == "resource":
            resource = "runtime_manifest_sha256" if key == "manifest" else "seed_sha256"
            source["definition"]["resources"][resource] = "a" * 64
        else:
            source["runtime_data"][key]["path" if field == "path" else "sha256"] = (
                str(tmp_path / "other.json") if field == "path" else "a" * 64
            )
    path = tmp_path / "source.json"
    save_new(path, source)
    with pytest.raises(ValueError, match="SEEN_V3_SOURCE_BINDING"):
        config.validate_sources(
            {
                "schema_version": 2,
                "source": str(path),
                "seen_v3": {"package": str(original)},
            }
        )


@pytest.mark.parametrize(
    "mutation", ["none", "same_id", "freeze", "new_dataset", "relative", "toggle"]
)
def test_only_original_seen_v3_can_use_historical_loading(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, mutation: str
) -> None:
    """A renamed cohort or arbitrary historical flag cannot weaken unseen guards."""
    original = tmp_path / "original"
    original.mkdir()
    save_new(original / "manifest.json", {"dataset_id": DATASET_V3})
    freeze = tmp_path / "old-freeze.json"
    freeze.write_text("Synthetic original freeze provenance.")
    monkeypatch.setattr(
        config, "SEEN_V3_MANIFEST_SHA256", config.digest(original / "manifest.json")
    )
    monkeypatch.setattr(config, "SEEN_V3_FREEZE_SHA256", config.digest(freeze))
    scope: dict[str, Any] = {
        "package": str(original),
        "freeze": str(freeze),
    }
    settings = {"schema_version": 2, "seen_v3": scope}
    if mutation == "same_id":
        (original / "manifest.json").write_text(
            json.dumps({"dataset_id": DATASET_V3, "replacement": "same name"})
        )
    elif mutation == "freeze":
        freeze.write_text("Synthetic replacement freeze.")
    elif mutation == "new_dataset":
        (original / "manifest.json").write_text(
            json.dumps({"dataset_id": "synthetic-unseen-v4"})
        )
    elif mutation == "relative":
        scope["freeze"] = "old-freeze.json"
    elif mutation == "toggle":
        scope["historical"] = True
    calls = []
    package = SimpleNamespace(cases={"synthetic-known": {}})

    def load(root: Path, freeze: Path, *, historical: bool) -> Any:
        calls.append((root, freeze, historical))
        return package

    monkeypatch.setattr(config, "reviewed_package", load)
    if mutation == "none":
        assert config.evaluation_package(settings) is package
        assert calls == [(original, Path(scope["freeze"]), True)]
    else:
        with pytest.raises(ValueError, match="SEEN_V3_"):
            config.evaluation_package(settings)
        assert not calls


@pytest.mark.parametrize("schema", [2, 3])
def test_snapshot_cannot_fall_back_to_another_cohort_or_scoring_mode(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, schema: int
) -> None:
    """Assembler and scorer receive the same package and its matching strict mode."""
    batch, output = tmp_path / "batch", tmp_path / "output"
    batch.mkdir()
    output.mkdir()
    save_new(batch / "registration.json", {"synthetic": True})
    (batch / "receipts-before.json").write_text("{}")
    package = SimpleNamespace(cases={})
    settings = {"schema_version": schema}
    frozen = {"synthetic": "frozen-metadata"} if schema == 3 else None
    monkeypatch.setattr(workflow, "evaluation_freeze", lambda _: frozen)
    monkeypatch.setattr(workflow, "evaluation_package", lambda _: package)
    monkeypatch.setattr(workflow, "archive", lambda _: tmp_path / "original-sdk")
    monkeypatch.setattr(
        workflow, "audit", lambda *a: (output / "receipts-after.json").write_text("{}")
    )
    calls = []
    expected = {
        "kind": "s5-unseen-quality" if schema == 3 else "s5-seen-diagnostic",
        "thresholds_met": schema == 3,
    }

    def assemble(*args: Any, **kwargs: Any) -> dict[str, Any]:
        assert kwargs == {"package": package, "s5": True}
        calls.append("assemble")
        return {}

    def score(*args: Any, **kwargs: Any) -> dict[str, Any]:
        assert kwargs == {
            "package": package,
            "freeze": frozen,
            "seen_diagnostic": schema == 2,
        }
        calls.append("score")
        return expected

    monkeypatch.setattr(workflow, "assemble", assemble)
    monkeypatch.setattr(workflow, "score_export", score)
    _, report = workflow.snapshot(settings, batch, output)
    assert calls == ["assemble", "score"]
    assert report == read(output / "score.json") == expected


@pytest.mark.parametrize("mutation", ["none", "unreachable", "unknown", "bundle"])
def test_seen20_preserves_quality_and_safety_stops(mutation: str) -> None:
    """The20-row seen scope uses16/18 without changing safety or the denominator."""
    score, evidence, sample = inputs()
    score.update(denominator=20, cases=score["cases"][:20])
    evidence["cases"] = evidence["cases"][:20]
    expected = None
    if mutation == "unreachable":
        score["full_case_evidence_count"] = 7
        expected = "EVIDENCE_THRESHOLD_UNREACHABLE"
    elif mutation == "unknown":
        sample["unknown_chat_calls"] = 1
        expected = "UNKNOWN_USAGE"
    elif mutation == "bundle":
        evidence["cases"][9]["steps"][0]["record"]["output"]["content"]["matches"].pop()
        expected = "POLICY_BUNDLE_INCOMPLETE"
    reasons = health.stop_reasons(
        score, evidence, 10, sample, 18_860_825, config.SEEN_V3_POLICY, registration(18)
    )
    assert (expected in reasons) if expected else not reasons
