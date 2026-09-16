"""Immutable deployment identity and test-build registry isolation."""

from __future__ import annotations

import copy
import json
from pathlib import Path

import pytest
from jobforge_agent import runtime_adapters, runtime_registry
from jobforge_agent.dispatch import DispatchError
from jobforge_agent.runtime_input import EXECUTOR_VERSION
from runtime_fixture_registry import MechanismAdapter

PROFILE = {
    "profile_id": "bounded-readonly-mechanism-profile-v1",
    "profile_hash": "a" * 64,
    "adapter_id": "bounded-readonly-mechanism-v1",
}


def manifest(monkeypatch: pytest.MonkeyPatch, tmp_path: Path, value: dict) -> None:
    """Replace the fixed file only inside the test process, never via input."""
    path = tmp_path / "executor.json"
    path.write_text(json.dumps(value), encoding="utf-8")
    monkeypatch.setattr(runtime_adapters, "_MANIFEST", path)


def resolve() -> runtime_adapters.RegisteredAdapter:
    """Resolve the fixture identity through the real fixed-registry boundary."""
    return runtime_adapters.resolve_adapter(
        PROFILE["adapter_id"],
        PROFILE["profile_id"],
        PROFILE["profile_hash"],
        EXECUTOR_VERSION,
    )


def test_production_registry_refuses_mechanism_adapter(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """A matching manifest alone cannot register executable business behavior."""
    manifest(
        monkeypatch,
        tmp_path,
        {
            "schema_version": 1,
            "executor_version": EXECUTOR_VERSION,
            "profiles": [PROFILE],
        },
    )
    assert set(runtime_registry.REGISTRY) == {"support-fixed-v1"}
    with pytest.raises(DispatchError, match="PROFILE_UNAVAILABLE"):
        resolve()


@pytest.mark.parametrize(
    "change", ["extra", "duplicate", "version", "hash", "null", "bool_version"]
)
def test_manifest_rejects_mixed_or_augmented_identity(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, change: str
) -> None:
    """Static registration cannot bypass the independently checked manifest."""
    value = {
        "schema_version": 1,
        "executor_version": EXECUTOR_VERSION,
        "profiles": [copy.deepcopy(PROFILE)],
    }
    if change == "extra":
        value["command"] = "forbidden"
    elif change == "duplicate":
        value["profiles"].append(copy.deepcopy(PROFILE))
    elif change == "version":
        value["executor_version"] = "old-v2"
    elif change == "hash":
        value["profiles"][0]["profile_hash"] = "b" * 64
    elif change == "null":
        value["profiles"][0]["adapter_id"] = None
    else:
        value["schema_version"] = True
    manifest(monkeypatch, tmp_path, value)
    monkeypatch.setattr(
        runtime_registry, "REGISTRY", {PROFILE["adapter_id"]: MechanismAdapter()}
    )
    with pytest.raises(DispatchError, match="PROFILE_UNAVAILABLE"):
        resolve()


def test_matching_build_registry_and_manifest_resolve(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """The test build must explicitly install both fixed identity and adapter."""
    manifest(
        monkeypatch,
        tmp_path,
        {
            "schema_version": 1,
            "executor_version": EXECUTOR_VERSION,
            "profiles": [PROFILE],
        },
    )
    monkeypatch.setattr(
        runtime_registry, "REGISTRY", {PROFILE["adapter_id"]: MechanismAdapter()}
    )
    assert resolve().adapter_id == PROFILE["adapter_id"]


def test_production_support_requires_matching_manifest_identity(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """The sole production adapter remains bound to trusted profile/hash/version."""
    profile = dict(
        PROFILE, adapter_id="support-fixed-v1", profile_id="support-test-profile-v1"
    )
    manifest(
        monkeypatch,
        tmp_path,
        {
            "schema_version": 1,
            "executor_version": EXECUTOR_VERSION,
            "profiles": [profile],
        },
    )
    adapter = runtime_adapters.resolve_adapter(
        profile["adapter_id"],
        profile["profile_id"],
        profile["profile_hash"],
        EXECUTOR_VERSION,
    )
    assert adapter.strategy == "support_fixed_v1"
    with pytest.raises(DispatchError, match="PROFILE_UNAVAILABLE"):
        runtime_adapters.resolve_adapter(
            profile["adapter_id"], profile["profile_id"], "b" * 64, EXECUTOR_VERSION
        )
