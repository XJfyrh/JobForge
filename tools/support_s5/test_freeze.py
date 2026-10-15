"""Freeze protects source and resource ceilings without authoring unseen data."""

from __future__ import annotations

import copy
import json
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest

from tools.support_evaluation.evidence import EvidenceError
from tools.support_evaluation.validate_data import sha256
from tools.support_s5 import freeze
from tools.support_s5.score import THRESHOLDS

ROOT = freeze.ROOT
HEAD = "0" * 40
SOURCES = {"synthetic-source.py": "a" * 64}
V1 = "support-s5-2026-10-07-v1"
V2 = "support-s5-2026-10-07-v2"
V3 = "support-s5-2026-10-08-v3"


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
    monkeypatch.setattr(freeze, "source_hashes", lambda schema=None: dict(sources))
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


def write_json(path: Path, value: Any) -> Path:
    """Write one synthetic trusted input and return its path."""
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(json.dumps(value).encode())
    return path


def seen_package(root: Path, dataset: str, family: str) -> Path:
    """One synthetic previously reviewed formal package manifest."""
    review = {
        "status": "independently_accepted",
        "cases": [{"template_family": family}],
    }
    raw = json.dumps(review).encode()
    write_json(root / "evaluation/family-review.json", review)
    return write_json(
        root / "manifest.json",
        {
            "dataset_id": dataset,
            "artifact_sha256": {"evaluation/family-review.json": sha256(raw)},
        },
    )


def definition(schema_version: int) -> dict[str, Any]:
    """One deployment definition differing only by its schema version."""
    return {
        "schema_version": schema_version,
        "model": {"request_model": "deepseek-flash", "max_tokens": 1024},
        "resources": {
            "dataset_id": "support-dev-2026-09-16-v2",
            "runtime_manifest_sha256": "a" * 64,
            "seed_sha256": "b" * 64,
            "tenants": [
                {
                    "tenant_id": "tenant-north",
                    "index_id": "c" * 64,
                    "index_content_hash": "d" * 64,
                },
                {
                    "tenant_id": "tenant-south",
                    "index_id": "e" * 64,
                    "index_content_hash": "f" * 64,
                },
            ],
        },
    }


@pytest.fixture
def inputs(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> SimpleNamespace:
    """A structurally complete, synthetic candidate-freeze input set."""
    monkeypatch.setattr(freeze, "source_hashes", lambda schema=None: dict(SOURCES))

    def run(command: list[str], **kwargs: Any) -> SimpleNamespace:
        return SimpleNamespace(stdout=b"" if command[1] == "status" else HEAD.encode())

    monkeypatch.setattr(freeze.subprocess, "run", run)
    reports = {}
    for strategy in ("support_agent_v1", "support_fixed_v1"):
        path = write_json(
            tmp_path / f"{strategy}.json",
            {
                "kind": "s5-development-regression",
                "strategy": strategy,
                "denominator": 40,
                "proposal_audit_complete": True,
            },
        )
        reports[str(path)] = sha256(path.read_bytes())
    review = write_json(
        tmp_path / "development-review.json",
        {"status": "independently_accepted", "reports": reports},
    )
    build = write_json(
        tmp_path / "build-receipt.json",
        {
            "source_commit": HEAD,
            "sources": dict(SOURCES),
            "production_registry": ["support-fixed-v1", "support-agent-v1"],
            "gold_present": False,
            "images": {"operator": "sha256:" + "1" * 64},
        },
    )
    seen = {
        dataset: seen_package(tmp_path / name, dataset, f"family-{name}")
        for name, dataset in (("v1", V1), ("v2", V2), ("v3", V3))
    }
    return SimpleNamespace(tmp_path=tmp_path, review=review, build=build, seen=seen)


def create(
    inputs: SimpleNamespace, agent_schema: int, fixed_schema: int, *datasets: str
) -> dict[str, Any]:
    """Run the real freeze gate over one schema pair and seen-manifest selection."""
    names = {
        agent_schema: "agent",
        fixed_schema: "fixed",
    }
    configs = []
    for schema, name in names.items():
        configs.append(
            write_json(
                inputs.tmp_path / f"{name}-{schema}.json",
                {"profiles": [{"definition": definition(schema)}]},
            )
        )
    arguments = {
        key: inputs.seen[dataset]
        for key, dataset in (
            ("seen_formal_manifest", V1),
            ("seen_formal_v2_manifest", V2),
            ("seen_formal_v3_manifest", V3),
        )
        if dataset in datasets
    }
    return freeze.create(
        configs[0], configs[1], inputs.review, inputs.build, **arguments
    )


@pytest.mark.parametrize("schema", [13, 14, 15, 16, 18])
def test_candidate_binds_all_three_seen_formals(
    inputs: SimpleNamespace,
    schema: int,
) -> None:
    """ADR-0034's candidate freezes, and formal v3 is bound as seen too."""
    receipt = create(inputs, schema, 17 if schema in {16, 18} else 12, V1, V2, V3)
    assert receipt["status"] == "frozen_before_unseen_creation"
    assert receipt["source_commit"] == HEAD and receipt["thresholds"] == THRESHOLDS
    packages = receipt["seen_formal_packages"]
    assert [p["dataset_id"] for p in packages] == [V1, V2, V3]
    assert [p["template_families"] for p in packages] == [
        ["family-v1"],
        ["family-v2"],
        ["family-v3"],
    ]
    assert all(
        sha256(inputs.seen[p["dataset_id"]].read_bytes()) == p["manifest_sha256"]
        for p in packages
    )
    # The singular form belongs to the schema 7 shape only.
    assert "seen_formal_package" not in receipt


@pytest.mark.parametrize("omitted", [V1, V2, V3])
@pytest.mark.parametrize("schema", [13, 14, 15, 16, 18])
def test_candidate_requires_every_seen_formal_manifest(
    inputs: SimpleNamespace, omitted: str, schema: int
) -> None:
    """An omitted prior cohort is a missing provenance, never a silent pass."""
    present = tuple(dataset for dataset in (V1, V2, V3) if dataset != omitted)
    with pytest.raises(EvidenceError, match="SEEN_FORMAL_PROVENANCE_REQUIRED"):
        create(inputs, schema, 17 if schema in {16, 18} else 12, *present)


@pytest.mark.parametrize(
    ("agent_schema", "fixed_schema", "datasets", "code"),
    [
        # Every schema 1-12 behaviour is unchanged, including its rejections.
        (6, 5, (), None),
        (7, 8, (V1,), None),
        (7, 8, (V1, V2), "SEEN_FORMAL_PROVENANCE_SCOPE"),
        (7, 8, (), "SEEN_FORMAL_PROVENANCE_REQUIRED"),
        (9, 10, (V1, V2), None),
        (9, 10, (V1,), "SEEN_FORMAL_PROVENANCE_REQUIRED"),
        (9, 10, (V1, V2, V3), "SEEN_FORMAL_PROVENANCE_SCOPE"),
        (6, 5, (V2,), "SEEN_FORMAL_PROVENANCE_SCOPE"),
        # The schema 11 candidate stays rejected: admitting it would change an
        # existing rejection, and ADR-0034 supersedes that candidate.
        (11, 12, (V1, V2), "COMPARISON_PROFILE_MISMATCH"),
        (13, 11, (V1, V2, V3), "COMPARISON_PROFILE_MISMATCH"),
        (16, 12, (V1, V2, V3), "COMPARISON_PROFILE_MISMATCH"),
        (18, 12, (V1, V2, V3), "COMPARISON_PROFILE_MISMATCH"),
        (15, 17, (V1, V2, V3), "COMPARISON_PROFILE_MISMATCH"),
        (12, 13, (V1, V2, V3), "COMPARISON_PROFILE_MISMATCH"),
        (13, 12, (V1, V2), "SEEN_FORMAL_PROVENANCE_REQUIRED"),
    ],
)
def test_existing_schema_pairs_keep_their_exact_behaviour(
    inputs: SimpleNamespace,
    agent_schema: int,
    fixed_schema: int,
    datasets: tuple[str, ...],
    code: str | None,
) -> None:
    """The extension is additive: no earlier pair accepts or rejects differently."""
    if code is None:
        receipt = create(inputs, agent_schema, fixed_schema, *datasets)
        if agent_schema == 6:
            assert "seen_formal_package" not in receipt
            assert "seen_formal_packages" not in receipt
        elif agent_schema == 7:
            assert receipt["seen_formal_package"]["dataset_id"] == V1
            assert "seen_formal_packages" not in receipt
        else:
            assert [p["dataset_id"] for p in receipt["seen_formal_packages"]] == [
                V1,
                V2,
            ]
        return
    with pytest.raises(EvidenceError, match=code):
        create(inputs, agent_schema, fixed_schema, *datasets)


def test_source_hashes_bind_the_v5_deployment_definition_and_prompt() -> None:
    """The freeze receipt must carry the definition this ADR introduces."""
    sources = freeze.source_hashes()
    bound = "api/support/s5-agent-v5/profile-schema.json"
    assert bound in sources
    assert sources[bound] == sha256((ROOT / bound).read_bytes())
    # Every earlier deployment definition stays bound.
    for name in (
        "s5-agent-v1",
        "s5-fixed-v1",
        "s5-agent-v2",
        "s5-fixed-v2",
        "s5-agent-v3",
        "s5-fixed-v3",
        "s5-agent-v4",
        "s5-fixed-v4",
    ):
        assert f"api/support/{name}/profile-schema.json" in sources, name
    # The v5 prompt source is bound through the inherited recovery source list.
    prompt = "python/jobforge_agent/support_agent_v5.py"
    assert prompt in sources
    assert sources[prompt] == sha256((ROOT / prompt).read_bytes())
    for path in (
        "api/support/s5-agent-v6/profile-schema.json",
        "api/support/s5-agent-v7/profile-schema.json",
        "api/support/s5-agent-v7/status-policy-fixtures.json",
        "api/support/agent-v2/schema.json",
        "python/jobforge_agent/support_agent_v6.py",
        "python/jobforge_agent/support_agent_v7.py",
        "python/jobforge_agent/status_policy.py",
        "python/jobforge_agent/policy_supplement.py",
    ):
        assert sources[path] == sha256((ROOT / path).read_bytes())


@pytest.mark.parametrize("schema", [16, 17, 18])
def test_bundle_source_hashes_bind_execution_protocol_and_generated_wire(
    schema: int,
) -> None:
    """New candidates also freeze runtime permissions and serialized contracts."""
    legacy = freeze.source_hashes()
    sources = freeze.source_hashes(schema)
    assert all(sources[path] == digest for path, digest in legacy.items())
    for path in (
        "internal/runprotocol/v2/conversation.go",
        "internal/runprotocol/v2/protocol.go",
        "internal/runinput/input.go",
        "internal/runinput/snapshot.go",
        "api/executor/v2/schema.json",
        "api/executor/v2/runtime-input.schema.json",
        "proto/jobforge/agent/v1/agent.pb.go",
        "proto/jobforge/agent/v1/agent_grpc.pb.go",
    ):
        assert sources[path] == sha256((ROOT / path).read_bytes())
        assert path not in legacy
    assert freeze.source_hashes(15) == legacy


@pytest.mark.parametrize(
    "path",
    [
        "internal/runprotocol/v2/conversation.go",
        "internal/runinput/input.go",
        "api/executor/v2/runtime-input.schema.json",
        "proto/jobforge/agent/v1/agent.pb.go",
    ],
)
def test_bundle_freeze_rejects_execution_source_mutation(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, path: str
) -> None:
    """A post-freeze change to any newly bound execution layer invalidates it."""
    monkeypatch.setattr(freeze, "ROOT", tmp_path)
    monkeypatch.setattr(freeze, "approval_sources", lambda: dict(SOURCES))
    for name in (path, "deploy/Dockerfile.support-s5"):
        file = tmp_path / name
        file.parent.mkdir(parents=True, exist_ok=True)
        file.write_bytes(b"synthetic before")
    receipt = {
        "status": "frozen_before_unseen_creation",
        "thresholds": THRESHOLDS,
        "sources": freeze.source_hashes(16),
        "candidate_blueprint": {"schema_version": 16},
    }
    freeze.validate(receipt)
    (tmp_path / path).write_bytes(b"synthetic after")
    with pytest.raises(EvidenceError, match="CANDIDATE_FREEZE_CHANGED"):
        freeze.validate(receipt)


def test_bundle_freeze_requires_versioned_build_sources(
    inputs: SimpleNamespace, monkeypatch: pytest.MonkeyPatch
) -> None:
    """An old build receipt cannot be reused for the new deployment pair."""
    sources = {**SOURCES, "internal/runinput/input.go": "b" * 64}
    monkeypatch.setattr(
        freeze,
        "source_hashes",
        lambda schema=None: dict(sources if schema in {16, 17} else SOURCES),
    )
    with pytest.raises(EvidenceError, match="BUILD_RECEIPT_MISMATCH"):
        create(inputs, 16, 17, V1, V2, V3)
    build = json.loads(inputs.build.read_bytes())
    build["sources"] = sources
    write_json(inputs.build, build)
    receipt = create(inputs, 16, 17, V1, V2, V3)
    assert receipt["sources"] == sources
    freeze.validate(receipt)
