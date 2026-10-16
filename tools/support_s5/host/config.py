"""Bind one concrete development experiment to files, images and money."""

from __future__ import annotations

import subprocess
from pathlib import Path
from typing import Any

from tools.support_evaluation.assemble import read, sha
from tools.support_evaluation.evidence import Package
from tools.support_evaluation.evidence import load_package as development_package
from tools.support_evaluation.s5_policy import FORMAL_POLICY, POLICY
from tools.support_s5.freeze import ROOT, blueprint, source_hashes, validate_blueprint
from tools.support_s5.package import ARTIFACT_NAMES, DATASET_V3, DATASET_V4
from tools.support_s5.package import load_package as reviewed_package

SEEN_V3_POLICY = dict(FORMAL_POLICY)
SEEN_V3_GUARDS = ("S5V3-004", "S5V3-018")
SEEN_V3_MANIFEST_SHA256 = (
    "6e3eea7581b3f2ba4a273a10656f4b42dcdc2bc07dd76a8a4063a7e69283203f"
)
SEEN_V3_FREEZE_SHA256 = (
    "7eccbafe73e57aad84dee49b30eb7adce5267afb289fe11c7959a534cf7373d1"
)


def evaluation_package(config: dict[str, Any]) -> Package:
    """Keep development, original-seen and current-frozen-unseen routes distinct."""
    if config["schema_version"] == 1:
        return development_package()
    formal = config["schema_version"] == 3
    scope = config["formal_v4" if formal else "seen_v3"]
    if set(scope) != {"package", "freeze"} or any(
        not Path(scope[key]).is_absolute() for key in ("package", "freeze")
    ):
        raise ValueError(
            "FORMAL_V4_PACKAGE_BINDING" if formal else "SEEN_V3_PACKAGE_BINDING"
        )
    root = Path(scope["package"])
    if formal:
        if (
            DATASET_V4 is None
            or read(root / "manifest.json")["dataset_id"] != DATASET_V4
        ):
            raise ValueError("FORMAL_V4_PACKAGE_REQUIRED")
        return reviewed_package(root, Path(scope["freeze"]), historical=False)
    if (
        digest(root / "manifest.json") != SEEN_V3_MANIFEST_SHA256
        or digest(Path(scope["freeze"])) != SEEN_V3_FREEZE_SHA256
    ):
        raise ValueError("SEEN_V3_ORIGINAL_BYTES_REQUIRED")
    if read(root / "manifest.json")["dataset_id"] != DATASET_V3:
        raise ValueError("SEEN_V3_PACKAGE_REQUIRED")
    # The old freeze proves this seen package's provenance, not that the current
    # candidate is frozen or that the cohort has become unseen again.
    return reviewed_package(root, Path(scope["freeze"]), historical=True)


def evaluation_freeze(config: dict[str, Any]) -> dict[str, Any] | None:
    """Only a current unseen batch may supply an acceptance freeze to scoring."""
    return (
        read(Path(config["formal_v4"]["freeze"]))
        if config["schema_version"] == 3
        else None
    )


def evaluation_files(config: dict[str, Any]) -> list[Path]:
    """Bind every original historical artifact; none is mounted into the operator."""
    if config["schema_version"] == 1:
        return []
    scope = config["formal_v4" if config["schema_version"] == 3 else "seen_v3"]
    return [Path(scope["freeze"])] + [
        Path(scope["package"]) / name
        for name in sorted(ARTIFACT_NAMES | {"manifest.json"})
    ]


def command(argv: list[str], *, raw: bytes | None = None) -> bytes:
    """Run a fixed local preparation/audit command without printing secrets."""
    result = subprocess.run(argv, input=raw, capture_output=True, cwd=ROOT, timeout=90)
    if result.returncode:
        raise RuntimeError("COMMAND_FAILED: " + argv[0])
    return result.stdout


def digest(path: Path) -> str:
    """Hash original bytes rather than a JSON reserialization."""
    return sha(path.read_bytes())


def load(path: Path) -> dict[str, Any]:
    """Read explicit paths; reject old-batch and unsupported workflow settings."""
    value = read(path)
    schema = value.get("schema_version")
    required = {
        "schema_version",
        "question",
        "hypothesis",
        "case_ids",
        "source",
        "build_receipt",
        "prior_ledger",
        "inventory",
        "control_database",
        "business_database",
        "business_container",
        "network",
        "container_prefix",
        "secrets",
        "identity",
        "images",
        "policy",
        "stage_lock",
    }
    if schema == 2:
        required.add("seen_v3")
    elif schema == 3:
        required.add("formal_v4")
    expected_policy = SEEN_V3_POLICY if schema in {2, 3} else POLICY
    policy = value.get("policy", {})
    if set(value) != required or schema not in {1, 2, 3} or set(policy) != set(POLICY):
        raise ValueError("WORKFLOW_CONFIG")
    for key in (
        "case_count",
        "minimum_correct",
        "minimum_full_evidence",
        "systemic_protocol",
    ):
        if policy[key] != expected_policy[key]:
            raise ValueError("QUALITY_POLICY_CHANGED")
    if any(
        type(policy[k]) is not int or not 0 < policy[k] <= POLICY[k]
        for k in ("batch_cap_microyuan", "stage_cap_microyuan")
    ):
        raise ValueError("MONEY_CAP")
    if set(policy["full_hold_microyuan"]) != {"investigation", "final"} or any(
        type(v) is not int or v <= 0 for v in policy["full_hold_microyuan"].values()
    ):
        raise ValueError("FULL_RESERVATION")
    if not value["question"].strip() or not value["hypothesis"].strip():
        raise ValueError("EXPERIMENT_QUESTION")
    expected_cases = (
        [f"DEV-{i:03d}" for i in range(1, 41)]
        if schema == 1
        else list(evaluation_package(value).cases)
    )
    if (
        value["case_ids"] != expected_cases
        or len(expected_cases) != policy["case_count"]
    ):
        raise ValueError(
            {1: "DEVELOPMENT_LIST", 2: "SEEN_V3_LIST", 3: "FORMAL_V4_LIST"}[schema]
        )
    for name in ("source", "build_receipt", "prior_ledger", "secrets", "stage_lock"):
        target = Path(value[name])
        if not target.is_absolute():
            raise ValueError("ABSOLUTE_INPUT_PATH")
    inventory = value["inventory"]
    databases = [p["database"] for p in inventory]
    prior = read(Path(value["prior_ledger"]))["phases"]
    if (
        len(databases) != len(set(databases))
        or set(databases)
        != {p["database"] for p in prior} | {value["control_database"]}
        or value["control_database"] in {p["database"] for p in prior}
    ):
        raise ValueError("PRIOR_INVENTORY_CHANGED")
    if any(
        not p.get(k) or str(p[k]).startswith("-")
        for p in inventory
        for k in ("container", "user", "database")
    ):
        raise ValueError("DATABASE_INVENTORY")
    for old in prior:
        actual = next(p for p in inventory if p["database"] == old["database"])
        if any(old.get(k, actual[k]) != actual[k] for k in ("container", "user")):
            raise ValueError("HISTORICAL_DATABASE_IDENTITY")
    images = value["images"]
    if set(images) != {
        "production_worker",
        "approval_operator",
        "s5_operator",
        "business",
    } or any(
        not isinstance(v, str) or not v.startswith("sha256:") or len(v) != 71
        for v in images.values()
    ):
        raise ValueError("IMMUTABLE_IMAGES")
    return value


def validate_sources(config: dict[str, Any]) -> dict[str, Any]:
    """Require a rebuilt clean source receipt, including the current audit fix."""
    source = read(Path(config["source"]))
    schema = source["definition"]["schema_version"]
    frozen = None
    if config["schema_version"] in {2, 3}:
        definition = source["definition"]
        formal = config["schema_version"] == 3
        scope = config["formal_v4" if formal else "seen_v3"]
        root = Path(scope["package"])
        permitted = (
            {(18, "support_agent_v1"), (17, "support_fixed_v1")}
            if formal
            else {(18, "support_agent_v1")}
        )
        error = "FORMAL_V4_SOURCE_BINDING" if formal else "SEEN_V3_SOURCE_BINDING"
        if (
            (schema, definition["program"]["strategy"]) not in permitted
            or source["schema_version"] != 2
            or definition["resources"]["dataset_id"]
            != (DATASET_V4 if formal else DATASET_V3)
        ):
            raise ValueError(error)
        for binding, relative, resource in (
            (source["case_map"], "evaluation/case-map.jsonl", None),
            (
                source["runtime_data"]["manifest"],
                "runtime/dataset-manifest.json",
                "runtime_manifest_sha256",
            ),
            (source["runtime_data"]["seed"], "runtime/seed.json", "seed_sha256"),
        ):
            expected = digest(root / relative)
            if (
                Path(binding["path"]).resolve() != (root / relative).resolve()
                or binding["sha256"] != expected
                or resource is not None
                and definition["resources"][resource] != expected
            ):
                raise ValueError(error)
        if formal:
            frozen = evaluation_freeze(config)
            assert frozen is not None
            validate_blueprint(
                frozen, definition["program"]["strategy"], blueprint(definition)
            )
    build = read(Path(config["build_receipt"]))
    if config["schema_version"] == 3:
        assert frozen is not None
        if frozen.get("images") != build["images"]:
            raise ValueError("FROZEN_IMAGES_CHANGED")
    if schema not in {16, 17, 18} or build["sources"] != source_hashes(schema):
        raise ValueError("BUILD_SOURCES_CHANGED")
    if (
        build["source_state"] != "clean-commit"
        or command(["git", "rev-parse", "HEAD"]).decode().strip()
        != build["source_commit"]
        or command(["git", "status", "--porcelain"]).strip()
        or build["images"] != config["images"]
    ):
        raise ValueError("BUILD_RECEIPT_CHANGED")
    for name in ("build_receipt", "data_review", "scoring_review", "price_snapshot"):
        if digest(Path(source[name]["path"])) != source[name]["sha256"]:
            raise ValueError("SOURCE_RECEIPT_CHANGED")
    if (
        Path(source["build_receipt"]["path"]).resolve()
        != Path(config["build_receipt"]).resolve()
    ):
        raise ValueError("BUILD_RECEIPT_BINDING")
    return build


def validate_worker_scope(worker: dict[str, Any], credentials: dict[str, Any]) -> None:
    """Reject action credentials outside the prepared tenant execution scope."""
    if set(worker["tenants"]) != set(credentials["tenants"]):
        raise ValueError("WORKER_TENANT_BINDING")
    for tenant, origins in worker["tenants"].items():
        keys = credentials["tenants"][tenant]
        if not origins.get("action_origin") and any(
            keys.get(name) for name in ("action_reader_key", "action_writer_key")
        ):
            raise ValueError("UNEXPECTED_ACTION_CREDENTIALS")


def validate_review(manifest: Path, review: Path) -> dict[str, Any]:
    """One startup review contains maintainer authorization and independent review."""
    value = read(review)
    plan = read(manifest)
    if value.get("manifest_sha256") != digest(manifest):
        raise ValueError("STARTUP_REVIEW_BINDING")
    maintainer, reviewer = value["maintainer"], value["independent_review"]
    if (
        maintainer.get("decision") != "authorized"
        or reviewer.get("decision") != "accepted"
        or not maintainer.get("identity")
        or not reviewer.get("identity")
        or maintainer["identity"] == reviewer["identity"]
        or maintainer.get("batch_cap_microyuan")
        != plan["policy"]["batch_cap_microyuan"]
        or maintainer.get("stage_cap_microyuan")
        != plan["policy"]["stage_cap_microyuan"]
    ):
        raise ValueError("STARTUP_REVIEW_REQUIRED")
    for record in (maintainer, reviewer):
        proof = record.get("evidence", {})
        if not proof.get("path") or digest(Path(proof["path"])) != proof.get("sha256"):
            raise ValueError("REVIEW_EVIDENCE_BINDING")
    return value


def verify_bound(plan: dict[str, Any], batch: Path) -> None:
    """Changing candidates, lists, models, permissions or money invalidates review."""
    if batch.resolve() != Path(plan["batch_root"]).resolve():
        raise ValueError("BATCH_ROOT_BINDING")
    for name, expected in plan["bound_files"].items():
        if digest(Path(name)) != expected:
            raise ValueError("BOUND_FILE_CHANGED")
    config = load(Path(plan["config"]))
    if config["policy"] != plan["policy"]:
        raise ValueError("POLICY_CHANGED")
    validate_sources(config)
