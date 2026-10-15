"""Strict routing metadata only; actual V4 scenarios must await a real freeze."""

from __future__ import annotations

from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest

from tools.support_evaluation.assemble import save_new
from tools.support_s5.host import config
from tools.support_s5.package import ARTIFACT_NAMES, DATASET_V3, DATASET_V4


@pytest.mark.parametrize("mutation", ["none", "seen", "relative", "toggle"])
def test_formal_loader_never_uses_historical_mode_or_development(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, mutation: str
) -> None:
    """The actual loader, not a config flag, enforces current freeze and novelty."""
    root = tmp_path / "unit-metadata"
    root.mkdir()
    save_new(
        root / "manifest.json",
        {"dataset_id": DATASET_V3 if mutation == "seen" else DATASET_V4},
    )
    freeze = tmp_path / "unit-freeze-metadata.json"
    save_new(freeze, {"synthetic": True})
    scope: dict[str, Any] = {"package": str(root), "freeze": str(freeze)}
    settings = {"schema_version": 3, "formal_v4": scope}
    if mutation == "relative":
        scope["freeze"] = "relative.json"
    elif mutation == "toggle":
        scope["historical"] = True
    calls = []
    package = SimpleNamespace(cases={})

    def load(path: Path, frozen: Path, *, historical: bool) -> Any:
        calls.append((path, frozen, historical))
        return package

    monkeypatch.setattr(config, "reviewed_package", load)
    if mutation == "none":
        assert config.evaluation_package(settings) is package
        assert calls == [(root, freeze, False)]
        assert config.evaluation_freeze(settings) == {"synthetic": True}
        assert set(config.evaluation_files(settings)) == {freeze} | {
            root / name for name in ARTIFACT_NAMES | {"manifest.json"}
        }
    else:
        with pytest.raises(ValueError, match="FORMAL_V4_"):
            config.evaluation_package(settings)
        assert not calls


@pytest.mark.parametrize(
    "schema,strategy,accepted",
    [
        (18, "support_agent_v1", True),
        (17, "support_fixed_v1", True),
        (16, "support_agent_v1", False),
        (18, "support_fixed_v1", False),
        (17, "support_agent_v1", False),
    ],
)
@pytest.mark.parametrize("mutation", ["none", "runtime", "dataset"])
def test_formal_source_binds_current_pair_runtime_and_freeze_before_build_or_db(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    schema: int,
    strategy: str,
    accepted: bool,
    mutation: str,
) -> None:
    """No build receipt exists: all invalid inputs must stop before it is read."""
    root = tmp_path / "unit-metadata"
    bindings = {}
    for name, relative in (
        ("case_map", "evaluation/case-map.jsonl"),
        ("manifest", "runtime/dataset-manifest.json"),
        ("seed", "runtime/seed.json"),
    ):
        path = root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("synthetic provenance metadata: " + name)
        bindings[name] = {"path": str(path), "sha256": config.digest(path)}
    freeze = tmp_path / "unit-freeze-metadata.json"
    save_new(freeze, {"synthetic": True})
    source: dict[str, Any] = {
        "schema_version": 2,
        "definition": {
            "schema_version": schema,
            "program": {"strategy": strategy},
            "resources": {
                "dataset_id": DATASET_V3 if mutation == "dataset" else DATASET_V4,
                "runtime_manifest_sha256": bindings["manifest"]["sha256"],
                "seed_sha256": bindings["seed"]["sha256"],
                "tenants": [],
            },
        },
        "case_map": bindings["case_map"],
        "runtime_data": {"manifest": bindings["manifest"], "seed": bindings["seed"]},
    }
    if mutation == "runtime":
        source["runtime_data"]["seed"]["sha256"] = "0" * 64
    path = tmp_path / "source.json"
    save_new(path, source)
    calls = []

    def validate(frozen: dict[str, Any], role: str, definition: dict[str, Any]) -> None:
        calls.append((frozen, role, definition))
        raise ValueError("FROZEN_BLUEPRINT_REJECTED")

    monkeypatch.setattr(config, "validate_blueprint", validate)
    reaches_freeze = accepted and mutation == "none"
    with pytest.raises(
        ValueError,
        match="FROZEN_BLUEPRINT_REJECTED"
        if reaches_freeze
        else "FORMAL_V4_SOURCE_BINDING",
    ):
        config.validate_sources(
            {
                "schema_version": 3,
                "source": str(path),
                "formal_v4": {"package": str(root), "freeze": str(freeze)},
            }
        )
    assert bool(calls) is reaches_freeze
