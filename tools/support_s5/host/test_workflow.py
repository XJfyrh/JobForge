"""Offline checks of authorization, automatic release and immutable evidence."""

from __future__ import annotations

import copy
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

import pytest
from jobforge_agent.business import POLICY_BUNDLE_ALIASES

from tools.support_evaluation import driver, launcher
from tools.support_evaluation.assemble import read, save_new
from tools.support_s5 import checkpoint
from tools.support_s5.host import config, health, ledger, workflow


def registration(schema: int = 16) -> dict[str, Any]:
    """Represent only the already registered Agent16/18 and Fixed17 identities."""
    fixed = schema == 17
    strategy = "support_fixed_v1" if fixed else "support_agent_v1"
    return {
        "strategy_blueprint": {
            "schema_version": schema,
            "program": {
                "strategy": strategy,
                "adapter": "support-fixed-v1" if fixed else "support-agent-v1",
                "prompt_version": "support-fixed-prompt-v2"
                if fixed
                else f"support-agent-prompt-v{8 if schema == 16 else 9}",
            },
        },
        "profile": {
            "strategy": strategy,
            "executor_version": "linux-v2-fixed-adjudication-runtime-2"
            if fixed
            else "linux-v2-adjudication-runtime-5",
        },
    }


def inputs(
    completed: int = 10,
) -> tuple[dict[str, Any], dict[str, Any], dict[str, Any]]:
    """Build clearly synthetic complete bundles and one failed quality result."""
    result = dict(
        source="passed",
        safety="passed",
        usage_status="verified",
        call_audit_complete=True,
        execution_complete=True,
        adjudication_stages={
            "final_input_closure": {
                "definition_closure": {"definition_complete": False}
            }
        },
    )
    row = {
        "status": "run",
        "run": {"error": None},
        "calls": {"items": []},
        "steps": [
            {
                "record": {
                    "kind": "read_policy_bundle",
                    "output": {
                        "content": {
                            "matches": [
                                {"chunk_id": alias} for alias in POLICY_BUNDLE_ALIASES
                            ]
                        }
                    },
                }
            }
        ],
    }
    score = {
        "schema_version": 3,
        "denominator": 40,
        "correct_count": completed - 1,
        "full_case_evidence_count": completed - 1,
        "hard_failure_cases": {"unauthorized_access": 0},
        "cases": [
            {"case_id": f"DEV-{i:03d}", **copy.deepcopy(result)} for i in range(1, 41)
        ],
    }
    evidence = {"cases": [copy.deepcopy(row) for _ in range(40)]}
    sample = dict(
        runs=completed,
        known=200_000,
        held=0,
        inflight=0,
        unfinished_attempts=0,
        unknown_chat_calls=0,
    )
    return score, evidence, sample


@pytest.mark.parametrize(
    "failure",
    [
        "quality",
        "single_format",
        "bundle",
        "source",
        "identity",
        "unknown",
        "unreachable",
        "correct_unreachable",
        "systemic",
        "full_hold",
    ],
)
@pytest.mark.parametrize("schema", [16, 17, 18])
def test_health_distinguishes_quality_from_mechanism(failure: str, schema: int) -> None:
    """Bad claims/one format failure continue; unsafe or unreachable batches stop."""
    score, evidence, sample = inputs()
    expected = None
    if failure == "bundle":
        evidence["cases"][9]["steps"][0]["record"]["output"]["content"]["matches"].pop()
        expected = "POLICY_BUNDLE_INCOMPLETE"
    elif failure == "source":
        score["cases"][9]["source"] = "failed"
        expected = "UNVERIFIED_CASE_BINDING"
    elif failure == "identity":
        score["hard_failure_cases"]["unauthorized_access"] = 1
        expected = "HARD_SAFETY_FAILURE"
    elif failure == "unknown":
        sample["unknown_chat_calls"] = 1
        expected = "UNKNOWN_USAGE"
    elif failure == "unreachable":
        score["full_case_evidence_count"] = 7
        expected = "EVIDENCE_THRESHOLD_UNREACHABLE" if schema != 17 else None
    elif failure == "correct_unreachable":
        score["correct_count"] = 5
        expected = "CORRECT_THRESHOLD_UNREACHABLE" if schema != 17 else None
    elif failure == "full_hold":
        sample["known"] = 4_000_000 - 2_228_224 + 1
        expected = "INSUFFICIENT_FULL_RESERVATION"
    elif failure in {"single_format", "systemic"}:
        for index in [9] if failure == "single_format" else [8, 9]:
            evidence["cases"][index]["run"]["error"] = {"code": "MODEL_PROTOCOL_ERROR"}
        if failure == "systemic":
            expected = "SYSTEMIC_PROTOCOL_FAILURE"
    original = copy.deepcopy((score, evidence, sample))
    reasons = health.stop_reasons(
        score, evidence, 10, sample, 13_212_370, config.POLICY, registration(schema)
    )
    assert (expected in reasons) if expected else not reasons
    assert (score, evidence, sample) == original


@pytest.mark.parametrize(
    "mutation",
    ["schema", "strategy", "adapter", "prompt", "executor", "profile_strategy"],
)
def test_quality_role_rejects_mixed_registration(mutation: str) -> None:
    """Relabeling one field cannot exempt a candidate from its quality gate."""
    value = registration(17)
    blueprint, profile = value["strategy_blueprint"], value["profile"]
    if mutation == "schema":
        blueprint["schema_version"] = 18
    elif mutation in {"strategy", "adapter"}:
        blueprint["program"][mutation] = "support_agent_v1"
    elif mutation == "prompt":
        blueprint["program"]["prompt_version"] = "support-agent-prompt-v9"
    elif mutation == "executor":
        profile["executor_version"] = "linux-v2-adjudication-runtime-5"
    else:
        profile["strategy"] = "support_agent_v1"
    with pytest.raises(ValueError, match="QUALITY_STRATEGY_BINDING"):
        health.quality_gate_applies(value)


@pytest.mark.parametrize(
    "mode",
    ["fixed_read_only", "agent_action", "extra_reader", "extra_writer", "tenant"],
)
def test_worker_action_scope_is_checked_without_starting_worker(mode: str) -> None:
    """Fixed deployments reject copied action keys during free preparation."""
    worker: dict[str, Any] = {"tenants": {"tenant-north": {}}}
    credentials: dict[str, Any] = {"tenants": {"tenant-north": {}}}
    if mode == "agent_action":
        worker["tenants"]["tenant-north"]["action_origin"] = "http://synthetic:8096"
        credentials["tenants"]["tenant-north"]["action_writer_key"] = "synthetic"
    elif mode in {"extra_reader", "extra_writer"}:
        name = "action_reader_key" if mode == "extra_reader" else "action_writer_key"
        credentials["tenants"]["tenant-north"][name] = "synthetic"
    elif mode == "tenant":
        credentials["tenants"]["tenant-south"] = {}
    if mode in {"extra_reader", "extra_writer", "tenant"}:
        expected = (
            "WORKER_TENANT_BINDING"
            if mode == "tenant"
            else "UNEXPECTED_ACTION_CREDENTIALS"
        )
        with pytest.raises(ValueError, match=expected):
            config.validate_worker_scope(worker, credentials)
    else:
        config.validate_worker_scope(worker, credentials)


def test_reservation_boundary_and_final_case() -> None:
    """Equality admits a full reservation; a finished list needs no next reserve."""
    _, _, sample = inputs()
    sample["known"] = 4_000_000 - 2_228_224
    assert not health.money_reasons(sample, 24_000_000, config.POLICY, next_run=True)
    assert health.money_reasons(sample, 24_000_001, config.POLICY, next_run=True)
    sample["known"] = 4_000_000
    assert not health.money_reasons(sample, 24_000_000, config.POLICY, next_run=False)


def test_case_ten_releases_automatically_and_cannot_reopen(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """The real SDK checkpoint and host exchange hashes without a human receipt."""
    batch = tmp_path
    for name in ("guard", "prepared", "exports/one", "checkpoints"):
        (batch / name).mkdir(parents=True)
    launch: dict[str, Any] = {
        "valid_until": (datetime.now(UTC) + timedelta(hours=1)).isoformat(),
        "profile_hash": "synthetic",
        "batch_account_id": "synthetic-batch",
        "cases": [{"ordinal": i, "case_id": f"DEV-{i:03d}"} for i in range(1, 41)],
    }
    save_new(batch / "prepared/launch.json", launch)
    save_new(batch / "registration.json", registration())
    monkeypatch.setattr(driver, "CONFIG", batch / "prepared")
    monkeypatch.setattr(checkpoint, "GUARD", batch / "guard")
    instance = checkpoint.CheckpointDriver(launch, batch / "exports/one", {})
    for row in instance.rows[:10]:
        row.update(status="finished", run_id="run-" + row["case_id"])
    last = batch / "exports/one/DEV-010"
    last.mkdir()
    save_new(last / "evidence.json", {"synthetic": True})
    score, evidence, sample = inputs()
    monkeypatch.setattr(ledger, "query", lambda _: sample)
    monkeypatch.setattr(
        ledger,
        "collect",
        lambda _: {"phases": [{"database": "old", **dict(sample, known=13_212_370)}]},
    )

    def snapshot(
        _: Any, __: Any, target: Path
    ) -> tuple[dict[str, Any], dict[str, Any]]:
        save_new(target / "score.json", score)
        save_new(target / "evidence.json", evidence)
        return evidence, score

    monkeypatch.setattr(workflow, "snapshot", snapshot)
    settings = {
        "inventory": [{"database": "new"}],
        "control_database": "new",
        "policy": config.POLICY,
        "case_ids": [r["case_id"] for r in launch["cases"]],
    }
    monkeypatch.setattr(
        checkpoint.time,
        "sleep",
        lambda _: workflow.advance(settings, batch, 13_212_370),
    )
    instance.save()
    assert (batch / "guard/10-continue.json").exists()
    assert read(batch / "checkpoints/10/decision.json")["reasons"] == []
    assert all(
        row["status"] == "unattempted"
        for row in read(instance.state_path)["cases"][10:]
    )
    assert not list(batch.rglob("*review*"))
    with pytest.raises(driver.BatchStopped, match="GUARD_NOT_FRESH"):
        checkpoint.CheckpointDriver(launch, batch / "exports/one", {})


def test_tampered_release_cannot_submit_next_case(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """A release for another export never authorizes the next SDK iteration."""
    guard, output = tmp_path / "guard", tmp_path / "output"
    guard.mkdir()
    output.mkdir()
    (output / "DEV-001").mkdir()
    save_new(output / "DEV-001/evidence.json", {})
    manifest = {
        "valid_until": (datetime.now(UTC) + timedelta(hours=1)).isoformat(),
        "profile_hash": "p",
        "batch_account_id": "b",
        "cases": [{"ordinal": i, "case_id": f"DEV-{i:03d}"} for i in range(1, 41)],
    }
    save_new(tmp_path / "launch.json", manifest)
    monkeypatch.setattr(driver, "CONFIG", tmp_path)
    monkeypatch.setattr(checkpoint, "GUARD", guard)
    instance = checkpoint.CheckpointDriver(manifest, output, {})
    instance.rows[0].update(status="finished", run_id="run-1")
    monkeypatch.setattr(
        checkpoint.time,
        "sleep",
        lambda _: save_new(guard / "01-continue.json", {"decision": "continue"}),
    )
    with pytest.raises(driver.BatchStopped, match="GUARD_RELEASE_MISMATCH"):
        instance.save()
    assert all(r["status"] == "unattempted" for r in instance.rows[1:])


def test_startup_review_binds_both_decisions_and_actual_evidence(
    tmp_path: Path,
) -> None:
    """A workflow approval or a review of older bytes cannot start a paid batch."""
    plan = tmp_path / "manifest.json"
    proof = tmp_path / "evidence.txt"
    proof.write_text("Synthetic authorization/review fixture; no live approval.")
    save_new(plan, {"policy": config.POLICY})
    value = {
        "manifest_sha256": config.digest(plan),
        "maintainer": {
            "identity": "maintainer",
            "decision": "authorized",
            "batch_cap_microyuan": 4_000_000,
            "stage_cap_microyuan": 28_000_000,
            "evidence": {"path": str(proof), "sha256": config.digest(proof)},
        },
        "independent_review": {
            "identity": "reviewer",
            "decision": "accepted",
            "evidence": {"path": str(proof), "sha256": config.digest(proof)},
        },
    }
    receipt = tmp_path / "review.json"
    save_new(receipt, value)
    assert config.validate_review(plan, receipt) == value
    plan.write_bytes(plan.read_bytes() + b"\n")
    with pytest.raises(ValueError, match="STARTUP_REVIEW_BINDING"):
        config.validate_review(plan, receipt)
    value["manifest_sha256"] = config.digest(plan)
    receipt.write_text(__import__("json").dumps(value))
    proof.write_text("changed")
    with pytest.raises(ValueError, match="REVIEW_EVIDENCE_BINDING"):
        config.validate_review(plan, receipt)


def test_ledger_never_deduplicates_or_ignores_unreadable_accounts(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """No fallback to a zero-fee historical snapshot is permitted."""
    db = {"container": "pg", "database": "old", "user": "owner"}
    with pytest.raises(ValueError, match="LEDGER_INVENTORY"):
        ledger.collect([db, db])

    def unreadable(_: Any) -> Any:
        raise OSError("synthetic unavailable database")

    monkeypatch.setattr(ledger, "query", unreadable)
    with pytest.raises(OSError):
        ledger.collect([db])


def test_copied_manifest_cannot_authorize_another_batch_root(tmp_path: Path) -> None:
    """Reviewed absolute files cannot be reused to bypass actual execution inputs."""
    with pytest.raises(ValueError, match="BATCH_ROOT_BINDING"):
        config.verify_bound(
            {"batch_root": str(tmp_path / "original")}, tmp_path / "copy"
        )


def test_commands_do_not_mount_gold_or_private_version_scripts(tmp_path: Path) -> None:
    """The installed driver replaces copied launchers and receives only run inputs."""
    settings = {
        "images": {
            "s5_operator": "sha256:" + "a" * 64,
            "business": "sha256:" + "b" * 64,
        },
        "container_prefix": "jobforge-s5-synthetic",
        "secrets": str(tmp_path / "secrets"),
        "network": "synthetic",
    }
    commands = workflow.commands(settings, tmp_path)
    launch = commands["launch"]
    assert "--init" in launch
    assert not any(
        word in " ".join(launch)
        for word in ("/operator", "gold", "host/", "registration.json")
    )
    assert "--read-only" in launch
    assert all(
        "agent-worker" not in " ".join(commands[k])
        for k in ("bootstrap", "inspect", "prepare_state")
    )


def test_generated_command_matches_actual_outer_launcher_cli(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Use the launcher's parser, not a duplicate expectation of its arguments."""
    import sys

    settings: dict[str, Any] = {
        "images": {
            "s5_operator": "sha256:" + "a" * 64,
            "business": "sha256:" + "b" * 64,
        },
        "container_prefix": "jobforge-s5-synthetic",
        "secrets": str(tmp_path / "secrets"),
        "network": "synthetic",
    }
    command = workflow.commands(settings, tmp_path)["launch"]
    image_index = command.index(settings["images"]["s5_operator"])
    save_new(tmp_path / "launch.json", {"synthetic": True})
    monkeypatch.setattr(launcher, "CONFIG", tmp_path)
    monkeypatch.setattr(sys, "argv", ["s5-launcher", *command[image_index + 1 :]])
    observed = []

    def start(manifest: Any, raw: bytes, *, s5: bool) -> int:
        observed.append((manifest, s5))
        return 0

    monkeypatch.setattr(launcher, "launch", start)
    assert launcher.main(s5=True) == 0
    assert observed == [({"synthetic": True}, True)]
