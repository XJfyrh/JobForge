"""Exercise preparation and a reviewed batch lifecycle with no external service."""

from __future__ import annotations

import copy
import json
from datetime import UTC, datetime, timedelta
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest

from tools.support_evaluation.assemble import read, save_new
from tools.support_s5.host import config, ledger, workflow
from tools.support_s5.package import ARTIFACT_NAMES


@pytest.mark.parametrize("failure", ["none", "lost_start", "cleanup", "no_export"])
@pytest.mark.parametrize(
    "schema,cohort",
    [
        (16, "dev"),
        (17, "dev"),
        (18, "dev"),
        (18, "seen"),
        (18, "seen_guard_failed"),
        (18, "formal"),
        (17, "formal"),
    ],
)
def test_prepare_run_and_restart_refusal_preserve_one_identity(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    failure: str,
    schema: int,
    cohort: str,
) -> None:
    """Real orchestration binds generated files; fake Docker never sends a request."""
    source_path, build_path, prior_path = (
        tmp_path / name for name in ("source.json", "build.json", "prior.json")
    )
    secret = tmp_path / "secrets"
    secret.mkdir()
    for name in ("worker.json", "driver.json", "control.env", "business.env"):
        (secret / name).write_text("synthetic; contains no real credentials")
    (secret / "worker.json").write_text(json.dumps({"tenants": {}}))
    proof = tmp_path / "synthetic-proof.txt"
    proof.write_text("Offline fixture. No actual authorization or model execution.")
    fixed = schema == 17
    seen = cohort.startswith("seen")
    formal = cohort == "formal"
    external = seen or formal
    count = 20 if external else 40
    strategy = "support_fixed_v1" if fixed else "support_agent_v1"
    source = {
        "definition": {
            "schema_version": schema,
            "program": {
                "strategy": strategy,
                "adapter": "support-fixed-v1" if fixed else "support-agent-v1",
                "prompt_version": "support-fixed-prompt-v2"
                if fixed
                else f"support-agent-prompt-v{8 if schema == 16 else 9}",
            },
            "model": {"request_model": "synthetic"},
            "final_model": {"request_model": "synthetic"},
        },
        **{
            name: {"path": str(proof)}
            for name in ("data_review", "scoring_review", "price_snapshot")
        },
    }
    save_new(source_path, source)
    save_new(prior_path, {"phases": [{"database": "old", "known": 100}]})
    images = {
        name: "sha256:" + character * 64
        for name, character in zip(
            ("production_worker", "approval_operator", "s5_operator", "business"),
            "abcd",
            strict=True,
        )
    }
    build = {"source_commit": "synthetic", "images": images}
    save_new(build_path, build)
    settings: dict[str, Any] = {
        "schema_version": 3 if formal else 2 if seen else 1,
        "question": "One synthetic lifecycle?",
        "hypothesis": "One immutable attempt.",
        "case_ids": [
            f"{'UNIT' if formal else 'S5V3' if seen else 'DEV'}-{i:03d}"
            for i in range(1, count + 1)
        ],
        "source": str(source_path),
        "build_receipt": str(build_path),
        "prior_ledger": str(prior_path),
        "inventory": [
            {"container": "pg", "user": "owner", "database": name}
            for name in ("old", "new")
        ],
        "control_database": "new",
        "business_database": "business",
        "business_container": "business-pg",
        "network": "synthetic",
        "container_prefix": "jobforge-s5-synthetic",
        "stage_lock": str(tmp_path / "stage.lock"),
        "secrets": str(secret),
        "identity": {"batch_id": "synthetic"},
        "images": images,
        "policy": copy.deepcopy(config.SEEN_V3_POLICY if external else config.POLICY),
    }
    if external:
        original = tmp_path / "seen-originals"
        for name in ARTIFACT_NAMES | {"manifest.json", "original-freeze.json"}:
            path = original / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("Synthetic routing fixture; no authored formal cases.")
        settings["formal_v4" if formal else "seen_v3"] = {
            "package": str(original),
            "freeze": str(original / "original-freeze.json"),
        }
        package = SimpleNamespace(cases=dict.fromkeys(settings["case_ids"]))
        monkeypatch.setattr(config, "evaluation_package", lambda _: package)
        monkeypatch.setattr(workflow, "evaluation_package", lambda _: package)
    if formal:
        frozen = {"synthetic": "already-frozen-metadata"}
        monkeypatch.setattr(workflow, "evaluation_freeze", lambda _: frozen)

        def validate(freeze: Any, role: str, blueprint: Any) -> None:
            assert (
                freeze == frozen
                and role == strategy
                and blueprint == source["definition"]
            )

        monkeypatch.setattr(workflow, "validate_blueprint", validate)
    configuration = tmp_path / "config.json"
    save_new(configuration, settings)
    monkeypatch.setattr(config, "validate_sources", lambda _: build)
    monkeypatch.setattr(workflow, "validate_sources", lambda _: build)
    batch = tmp_path / "batch"
    now = datetime.now(UTC)
    launch = {
        "cases": [
            {"case_id": c, "ordinal": i} for i, c in enumerate(settings["case_ids"], 1)
        ],
        "profile_hash": "synthetic",
        "batch_account_id": "synthetic",
        "worker_id": "synthetic",
        "valid_from": now.isoformat(),
        "valid_until": (now + timedelta(hours=6)).isoformat(),
        "config_sha256": {},
    }
    setup = {
        "matches_config": True,
        "migrations_ready": True,
        "sampled_at": (now + timedelta(seconds=1)).isoformat(),
        "history": {"runs": 0},
        "accounts": [],
    }
    calls: list[list[str]] = []
    containers: dict[str, bool] = {}

    def command(argv: list[str], **_: Any) -> bytes:
        calls.append(argv)
        if argv[:3] == ["docker", "ps", "-a"]:
            return "\n".join(containers).encode()
        if "--name" in argv:
            name = argv[argv.index("--name") + 1]
            containers[name] = True
            if failure == "lost_start" and name.endswith("-business"):
                raise TimeoutError("synthetic lost Docker acknowledgement")
        if argv[:2] == ["docker", "stop"]:
            if failure == "cleanup" and argv[-1].endswith("-business"):
                raise RuntimeError("synthetic cleanup unavailable")
            containers[argv[-1]] = False
        if argv[:2] == ["go", "run"]:
            prepared = batch / "prepared"
            prepared.mkdir()
            save_new(prepared / "launch.json", launch)
            for name in (
                "control.disabled.json",
                "control.enabled.json",
                "worker.json",
                "executor.json",
            ):
                save_new(prepared / name, {"enabled_profiles": [], "tenants": {}})
        elif argv[-1] == "inspect-support":
            return json.dumps(setup).encode()
        elif argv[-1] == "prepare-state":
            target = batch / "state/synthetic"
            target.mkdir()
            save_new(target / "prepared.json", {"synthetic": True})
        elif argv[:2] == ["docker", "inspect"]:
            running = containers.get(argv[2], False)
            return json.dumps(
                {"Running": running, "Pid": 123 if running else 0}
            ).encode()
        return b""

    monkeypatch.setattr(workflow, "command", command)
    monkeypatch.setattr(
        workflow,
        "register",
        lambda *a, **k: {
            "strategy_blueprint": source["definition"],
            "profile": {
                "strategy": strategy,
                "executor_version": "linux-v2-fixed-adjudication-runtime-2"
                if fixed
                else "linux-v2-adjudication-runtime-5",
                "max_input_tokens": 1048576,
                "pricing": {
                    "input_miss_microyuan": 2000000,
                    "input_hit_microyuan": 40000,
                    "output_microyuan": 8000000,
                    "denominator": 1000000,
                },
            },
        },
    )
    current: dict[str, Any] = {
        **{k: 0 for k in ledger.COUNTS},
        "database": "new",
        "container": "pg",
        "user": "owner",
    }
    old = dict(current, database="old", known=100)

    def collect(_: Any) -> dict[str, Any]:
        return {
            "phases": [old, current],
            "totals": {k: old[k] + current[k] for k in ledger.COUNTS},
        }

    monkeypatch.setattr(ledger, "collect", collect)
    monkeypatch.setattr(ledger, "query", lambda _: current)
    plan = workflow.prepare(configuration, batch)
    assert plan["paid_release"] is False
    assert plan["batch_root"] == str(batch.resolve())
    if external:
        assert plan["scope"] == (
            "unseen_v4_formal_acceptance"
            if formal
            else "seen_v3_regression_not_unseen_acceptance"
        )
        assert all(
            str(path.resolve()) in plan["bound_files"]
            for path in config.evaluation_files(settings)
        )
        assert not any(
            "seen-originals" in arg
            for arg in workflow.commands(settings, batch)["launch"]
        )
    assert (
        config.digest(batch / "prepared/health-gate.json")
        == read(batch / "prepared/launch.json")["config_sha256"]["health-gate.json"]
    )
    assert not any("-paid" in arg for call in calls for arg in call)
    config.verify_bound(plan, batch)
    review = {
        "manifest_sha256": config.digest(batch / "manifest.json"),
        "maintainer": {
            "decision": "authorized",
            "identity": "synthetic-maintainer",
            "batch_cap_microyuan": 4000000,
            "stage_cap_microyuan": 28000000,
            "evidence": {"path": str(proof), "sha256": config.digest(proof)},
        },
        "independent_review": {
            "decision": "accepted",
            "identity": "synthetic-reviewer",
            "evidence": {"path": str(proof), "sha256": config.digest(proof)},
        },
    }
    review_path = tmp_path / "review.json"
    save_new(review_path, review)
    launches: list[list[str]] = []

    class Process:
        returncode = 1 if failure == "no_export" else 0

        def __init__(self, argv: list[str], **_: Any) -> None:
            launches.append(argv)
            containers[settings["container_prefix"] + "-paid"] = False
            current["runs"] = count

        def poll(self) -> int:
            return 0

        def wait(self, **_: Any) -> int:
            return 0

    monkeypatch.setattr(workflow.subprocess, "Popen", Process)
    monkeypatch.setattr(workflow, "audit", lambda *_: None)

    def snapshot(
        _: Any, __: Any, target: Path
    ) -> tuple[dict[str, Any], dict[str, Any]]:
        score: dict[str, Any] = {
            "correct_count": 8 if fixed else 19 if external else 39,
            "full_case_evidence_count": 8 if fixed else 19 if external else 39,
            "hard_failure_cases": {"unauthorized_access": 0},
            "proposal_audit_complete": True,
            "denominator": count,
        }
        if seen:
            failed_id = "S5V3-004" if cohort == "seen_guard_failed" else "S5V3-020"
            score["cases"] = [
                {
                    "case_id": case_id,
                    "correct": case_id != failed_id,
                    "full_case_evidence": case_id != failed_id,
                }
                for case_id in settings["case_ids"]
            ]
        save_new(target / "score.json", score)
        return {}, score

    monkeypatch.setattr(workflow, "snapshot", snapshot)
    if failure != "none":
        with pytest.raises((TimeoutError, RuntimeError, ValueError)):
            workflow.run(batch, review_path)
        assert not (batch / "result.json").exists()
        assert (batch / "attempted.json").exists()
        assert Path(settings["stage_lock"]).exists() == (failure == "cleanup")
        if failure == "cleanup":
            assert (batch / "cleanup-error.json").exists()
            assert containers[settings["container_prefix"] + "-business"] is True
        elif failure == "lost_start":
            assert not any(containers.values())
            assert not launches
        else:
            assert (
                read(batch / "workflow-error.json")["reason"]
                == "LAUNCHER_FAILED_BEFORE_EXPORT"
            )
            assert not any(containers.values())
            assert len(launches) == 1
        return
    workflow.run(batch, review_path)
    assert len(launches) == 1
    result = read(batch / "result.json")
    assert result["model_quality_applicable"] is not fixed
    assert result["model_quality_passed"] is (None if fixed else True)
    if fixed:
        assert result["baseline_collection_complete"] is True
        assert result["correct"] == 8
    if seen:
        assert result["scope"] == "seen_v3_regression_not_unseen_acceptance"
        assert result["model_quality_passed"] is True
        assert result["freeze_guard_cases_passed"] is (cohort != "seen_guard_failed")
    if formal:
        assert result["scope"] == "unseen_v4_formal_acceptance"
        assert "freeze_guard_cases_passed" not in result
    assert not Path(settings["stage_lock"]).exists()
    before = {
        p.relative_to(batch): p.read_bytes() for p in batch.rglob("*") if p.is_file()
    }
    with pytest.raises(ValueError, match="BATCH_ALREADY_ATTEMPTED"):
        workflow.run(batch, review_path)
    assert len(launches) == 1
    assert before == {
        p.relative_to(batch): p.read_bytes() for p in batch.rglob("*") if p.is_file()
    }
    (batch / "prepared/worker.json").write_text("changed")
    with pytest.raises(ValueError, match="BOUND_FILE_CHANGED"):
        config.verify_bound(plan, batch)


def test_cleanup_attempts_every_owned_stop_after_listing_loss(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """A failed global read cannot prevent independent owned-process cleanup."""
    running = {"owned-control": True, "owned-business": True}

    def command(argv: list[str], **_: Any) -> bytes:
        if argv[:2] == ["docker", "ps"]:
            raise TimeoutError("synthetic lost container listing")
        name = argv[2] if argv[1] == "inspect" else argv[-1]
        if argv[1] == "stop":
            running[name] = False
            return b""
        return json.dumps(
            {"Running": running[name], "Pid": 123 if running[name] else 0}
        ).encode()

    monkeypatch.setattr(workflow, "command", command)
    with pytest.raises(RuntimeError, match="CLEANUP_REQUIRES_REVIEW"):
        workflow.cleanup(None, list(running))
    assert not any(running.values())
