"""Bind one concrete development experiment to files, images and money."""

from __future__ import annotations

import subprocess
from pathlib import Path
from typing import Any

from tools.support_evaluation.assemble import read, sha
from tools.support_s5.freeze import ROOT, source_hashes

POLICY = {
    "case_count": 40,
    "minimum_correct": 36,
    "minimum_full_evidence": 38,
    "batch_cap_microyuan": 4_000_000,
    "stage_cap_microyuan": 28_000_000,
    "full_hold_microyuan": {"investigation": 2_105_344, "final": 2_228_224},
    "systemic_protocol": "two_consecutive_or_two_in_first_three",
}


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
    policy = value.get("policy", {})
    if (
        set(value) != required
        or value["schema_version"] != 1
        or set(policy) != set(POLICY)
    ):
        raise ValueError("WORKFLOW_CONFIG")
    for key in (
        "case_count",
        "minimum_correct",
        "minimum_full_evidence",
        "systemic_protocol",
    ):
        if policy[key] != POLICY[key]:
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
    if value["case_ids"] != [f"DEV-{i:03d}" for i in range(1, 41)]:
        raise ValueError("DEVELOPMENT_LIST")
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
    build = read(Path(config["build_receipt"]))
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
