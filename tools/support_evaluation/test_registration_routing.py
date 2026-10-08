"""Exercise prepared registration routing with explicit synthetic test inputs."""

from __future__ import annotations

import json
from dataclasses import replace
from pathlib import Path

import pytest

from tools.support_evaluation.assemble import register, save_new, sha
from tools.support_evaluation.evidence import EvidenceError, Package, load_package
from tools.support_evaluation.fixtures import registered
from tools.support_evaluation.validate_data import DATASET_VERSION

ROOT = Path(__file__).resolve().parents[2]
FORMAL = ("support-s5-2026-10-07-v1", "support-s5-2026-10-07-v2")
STRATEGIES = ("support_agent_v1", "support_fixed_v1")


def prepared(root: Path, dataset: str, strategy: str, count: int) -> Package:
    """Write the actual deployment file shape; no Run or model result is made."""
    development = load_package()
    case_count = 40 if dataset == DATASET_VERSION else 20
    package = replace(
        development, cases=dict(list(development.cases.items())[:case_count])
    )
    base = registered(development)
    profile = json.loads((ROOT / "api/support/profile-v1/fixtures.json").read_bytes())[
        "profile"
    ]
    profile["strategy"] = strategy
    profile["executor_version"] = (
        "linux-v2-approval-runtime-1"
        if strategy == "support_agent_v1"
        else "linux-v2-fixed-comparison-runtime-1"
    )
    profile["definition"]["program"]["strategy"] = strategy
    profile["definition"]["resources"]["dataset_id"] = dataset
    limits = base["bindings"][0]["budget_limits"]
    control = {
        "profiles": [profile],
        "budgets": [
            {"account_id": scope, "limits": limits[scope]}
            for scope in ("tenant", "batch")
        ],
        "bindings": [
            {"tenant_id": tenant, "tenant_account_id": "tenant"}
            for tenant in ("tenant-north", "tenant-south")
        ],
    }
    worker = {
        "tenants": {
            "tenant-north": {
                "business_origin": "http://business:8092",
                "ollama_origin": "http://ollama:11434",
            }
        }
    }
    hashes = {}
    for name, value in (("control.disabled.json", control), ("worker.json", worker)):
        save_new(root / name, value)
        hashes[name] = sha((root / name).read_bytes())
    save_new(
        root / "launch.json",
        {
            "config_sha256": hashes,
            "batch_key": "synthetic-routing-test",
            "batch_account_id": "batch",
            "cases": base["bindings"][:count],
        },
    )
    return package


@pytest.mark.parametrize("strategy", STRATEGIES)
@pytest.mark.parametrize(
    "dataset,count", [(v, 20) for v in FORMAL] + [(DATASET_VERSION, 40)]
)
def test_s5_register_routes_formal20_and_development40(
    tmp_path: Path, dataset: str, count: int, strategy: str
) -> None:
    """Both strategy preparations reach the official registration validator."""
    package = prepared(tmp_path, dataset, strategy, count)
    actual = register(tmp_path, package=package, s5=True)
    assert actual["dataset_version"] == dataset
    assert actual["profile"]["strategy"] == strategy
    assert {row["case_id"] for row in actual["bindings"]} == set(package.cases)
    assert len(actual["bindings"]) == count


@pytest.mark.parametrize("strategy", STRATEGIES)
@pytest.mark.parametrize(
    "dataset,count",
    [(v, count) for v in FORMAL for count in (19, 21, 40)]
    + [(DATASET_VERSION, 20), (DATASET_VERSION, 39)],
)
def test_s5_register_rejects_wrong_case_count(
    tmp_path: Path, dataset: str, count: int, strategy: str
) -> None:
    """Formal datasets cannot take the development denominator or partial sets."""
    package = prepared(tmp_path, dataset, strategy, count)
    with pytest.raises(EvidenceError, match="^CASE_COVERAGE$"):
        register(tmp_path, package=package, s5=True)


@pytest.mark.parametrize("strategy", STRATEGIES)
def test_s5_register_rejects_unknown_dataset(tmp_path: Path, strategy: str) -> None:
    """The count route does not expand the existing dataset allowlist."""
    package = prepared(tmp_path, "support-s5-unknown", strategy, 20)
    with pytest.raises(EvidenceError, match="^DATA_VERSION$"):
        register(tmp_path, package=package, s5=True)
