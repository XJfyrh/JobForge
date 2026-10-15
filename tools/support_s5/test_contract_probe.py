"""Verify a prompt-only delta over actual runtime request construction."""

from __future__ import annotations

import copy
import json
from datetime import UTC, datetime, timedelta
from pathlib import Path

import pytest
from jobforge_agent.deepseek import prepare_chat_request
from jobforge_agent.dispatch import RunCallContext
from jobforge_agent.support_agent_v8 import FINAL_SYSTEM, SupportAgentV8Adapter
from jobforge_agent.support_contract import (
    CLAIM_VARIANTS,
    MODEL_FIELDS,
    POINTERS,
    REQUESTED_FIELDS,
)

from tools.support_evaluation.evidence import EvidenceError, validate_run_sources
from tools.support_s5.host import contract_probe as probe
from tools.support_s5.test_bundle import bundle_fixture


def test_diagnostic_shape_descriptor_matches_unchanged_contract() -> None:
    """The Go health check reuses current enums and pointers without policy gold."""
    path = Path(probe.__file__).parents[2] / "support_diagnosis/probe-contract.json"
    expected = json.loads(
        json.dumps(
            {
                "model_fields": sorted(MODEL_FIELDS),
                "requested_fields": REQUESTED_FIELDS,
                "pointers": POINTERS,
                "claim_variants": CLAIM_VARIANTS,
            }
        )
    )
    assert json.loads(path.read_bytes()) == expected


def test_ablation_changes_only_the_registered_sentence_and_preserves_pairs() -> None:
    """C2 removes one extra reminder; facts, original C1 and limits remain exact."""
    plan = probe.load_probe_plan(probe.ABLATION_PLAN)
    package, registration, row = source_fixture()
    legacy, _ = probe.project_case(
        row, registration["bindings"][0], registration["profile"], package
    )
    bodies, _ = probe.project_case(
        row, registration["bindings"][0], registration["profile"], package, plan
    )
    assert bodies["C1"] == legacy["V8C"]
    assert probe.CANDIDATE_SYSTEM.count(probe.DESCRIPTION_REMINDER) == 1
    left, right = (json.loads(bodies[g]) for g in ("C1", "C2"))
    assert right["messages"][0]["content"] == left["messages"][0]["content"].replace(
        probe.DESCRIPTION_REMINDER, ""
    )
    right["messages"][0]["content"] = left["messages"][0]["content"]
    assert right == left
    trials = probe.schedule(plan)
    assert len(trials) == 18 and len({t["trial_id"] for t in trials}) == 18
    assert {t["case_id"] for t in trials} == {"DEV-016", "DEV-017", "DEV-020"}
    assert all(
        sum(t["case_id"] == cid and t["group"] == group for t in trials) == 3
        for cid in plan["case_ids"]
        for group in ("C1", "C2")
    )
    assert [
        (t["group"], t["repetition"]) for t in trials if t["case_id"] == "DEV-016"
    ] == [("C1", 1), ("C2", 1), ("C2", 2), ("C1", 2), ("C1", 3), ("C2", 3)]


def test_timing_probe_only_appends_critical_requirement_to_original_v8() -> None:
    """The narrower candidate must not carry C1's other two semantic changes."""
    plan = probe.load_probe_plan(probe.TIMING_PLAN)
    package, registration, row = source_fixture()
    legacy, _ = probe.project_case(
        row, registration["bindings"][0], registration["profile"], package
    )
    bodies, _ = probe.project_case(
        row, registration["bindings"][0], registration["profile"], package, plan
    )
    assert bodies["V8"] == legacy["V8"]
    left, right = (json.loads(bodies[g]) for g in ("V8", "V8T"))
    text = right["messages"][0]["content"]
    assert (
        text
        == FINAL_SYSTEM
        + "\nCRITICAL AND TIMING COMPLETENESS:\n"
        + (probe.RESTORED_CONTRACT.split("\n3. ", 1)[1])
    )
    assert "Missing relationships are hierarchical" not in text
    assert probe.DESCRIPTION_REMINDER not in text
    right["messages"][0]["content"] = left["messages"][0]["content"]
    assert right == left
    trials = probe.schedule(plan)
    assert len(trials) == 12 and len({t["trial_id"] for t in trials}) == 12
    assert {t["case_id"] for t in trials} == {"DEV-026", "DEV-027"}
    assert [
        (t["group"], t["repetition"]) for t in trials if t["case_id"] == "DEV-027"
    ] == [("V8T", 1), ("V8", 1), ("V8", 2), ("V8T", 2), ("V8T", 3), ("V8", 3)]


@pytest.mark.parametrize(
    "mutation", ["kind", "duplicate", "unknown", "target", "repetitions", "extra"]
)
@pytest.mark.parametrize("plan_path", [probe.ABLATION_PLAN, probe.TIMING_PLAN])
def test_ablation_rejects_unregistered_plan(
    tmp_path: Path, mutation: str, plan_path: Path
) -> None:
    """Configuration cannot widen identities, protocol or endpoint authority."""
    plan = probe.read(plan_path)
    if mutation == "kind":
        plan["kind"] = "arbitrary"
    elif mutation == "duplicate":
        plan["case_ids"][1] = plan["case_ids"][0]
    elif mutation == "unknown":
        plan["case_ids"][1] = "DEV-041"
    elif mutation == "target":
        plan["case_ids"] = ["DEV-015"]
    elif mutation == "repetitions":
        plan["repetitions"] = True
    else:
        plan["endpoint"] = "arbitrary"
    path = tmp_path / "plan.json"
    probe.save(path, plan)
    with pytest.raises(EvidenceError):
        probe.load_probe_plan(path)


def source_fixture():
    """Bind synthetic exported calls to the real v8 adapter, not the probe."""
    package, registration, row = bundle_fixture(fixed=False)
    checkpoint, _ = validate_run_sources(
        row, registration["bindings"][0], registration["profile"], package
    )
    checkpoint = json.loads(json.dumps(checkpoint, sort_keys=True))
    indices = {s["record"]["step_id"]: i for i, s in enumerate(row["steps"])}
    for call in row["calls"]["items"]:
        if call["subcall"] != "chat":
            continue
        prefix = {
            **checkpoint,
            "steps": checkpoint["steps"][: indices[call["step_id"]]],
            "next_step": {"kind": call["step_kind"]},
        }
        request = prepare_chat_request(
            SupportAgentV8Adapter().proposal_messages(prefix, correction=False),
            context=RunCallContext(
                registration["profile"]["profile_hash"],
                row["run"]["snapshot_hash"],
                "",
                agent=True,
                final_adjudication=call["step_kind"] == "model_proposal",
            ),
        )
        call["parameter_hash"] = request.parameter_hash
    return package, registration, row


def test_probe_preserves_fact_bytes_and_original_request() -> None:
    """The candidate may alter only system text, never evidence or model limits."""
    package, registration, row = source_fixture()
    before = copy.deepcopy(row)
    bodies, proof = probe.project_case(
        row, registration["bindings"][0], registration["profile"], package
    )
    assert row == before
    left, right = (json.loads(bodies[g]) for g in ("V8", "V8C"))
    assert left["messages"][0]["content"] == FINAL_SYSTEM
    assert right["messages"][0]["content"] == FINAL_SYSTEM + probe.RESTORED_CONTRACT
    assert left["messages"][1]["content"] == right["messages"][1]["content"]
    right["messages"][0]["content"] = left["messages"][0]["content"]
    assert right == left
    assert len(proof["original_requests"]) >= 2
    assert all(cid not in probe.CANDIDATE_SYSTEM for cid in probe.CASE_IDS)
    facts = json.loads(left["messages"][1]["content"])
    assert facts["final_stage"] is True
    assert (
        not {"draft", "gold", "required_claims", "previous_tools", "remaining_tools"}
        & facts.keys()
    )


@pytest.mark.parametrize(
    "mutation", ["hash", "source", "missing_final", "duplicate_final", "version"]
)
def test_probe_rejects_invalid_original_evidence(mutation: str) -> None:
    """A missing, duplicated or cross-bound source cannot become a trial."""
    package, registration, row = source_fixture()
    final = next(c for c in row["calls"]["items"] if c["step_kind"] == "model_proposal")
    if mutation == "hash":
        final["parameter_hash"] = "f" * 64
    elif mutation == "source":
        row["steps"][0]["record"]["commit_hash"] = "f" * 64
    elif mutation == "missing_final":
        row["calls"]["items"].remove(final)
    elif mutation == "duplicate_final":
        row["calls"]["items"].append(copy.deepcopy(final))
    else:
        registration["profile"]["executor_version"] = "linux-v2-adjudication-runtime-4"
    with pytest.raises(ValueError):
        probe.project_case(
            row, registration["bindings"][0], registration["profile"], package
        )


@pytest.mark.parametrize("plan_path", [None, probe.ABLATION_PLAN, probe.TIMING_PLAN])
def test_probe_fixes_schedule_and_never_scores_missing_trials_as_passed(
    tmp_path: Path, monkeypatch, plan_path: Path | None
) -> None:
    """Unattempted trials stay in the fixed denominator and artifacts are immutable."""
    package, registration, row = source_fixture()
    plan = probe.load_probe_plan(plan_path) if plan_path else None
    case_ids = probe.CASE_IDS if plan is None else plan["case_ids"]
    groups = probe.probe_groups(plan)
    count = len(probe.schedule(plan))
    bodies, proof = probe.project_case(
        row, registration["bindings"][0], registration["profile"], package, plan
    )
    projected = {cid: bodies for cid in case_ids}
    proofs = [dict(proof, case_id=cid) for cid in case_ids]
    monkeypatch.setattr(probe, "projections", lambda *_: (projected, proofs))
    monkeypatch.setattr(probe, "load_package", lambda: package)
    batch = tmp_path / "original"
    batch.mkdir()
    (batch / "evidence.json").write_text("{}")
    (batch / "registration.json").write_text("{}")
    runner = tmp_path / "runner"
    runner.write_bytes(b"test runner")
    preflight = tmp_path / "preflight.json"
    probe.save(preflight, {"ready": True, "pricing": {"hash": "a" * 64}})
    ledger = tmp_path / "prior.json"
    probe.save(
        ledger,
        {
            "totals": {
                "known": 14628055,
                "held": 0,
                "inflight": 0,
                "unknown_chat_calls": 0,
                "unfinished_attempts": 0,
            }
        },
    )
    output = tmp_path / "experiment"
    prepared = probe.prepare(
        batch,
        output,
        runner,
        preflight,
        ledger,
        "jobforge_s5_diagnosis_probe",
        "test",
        plan_path,
    )
    assert prepared["paid_calls"] == 0 and prepared["trials"] == count
    manifest = probe.read(output / "manifest.json")
    assert {g: sum(t["group"] == g for t in manifest["trials"]) for g in groups} == {
        g: count // 2 for g in groups
    }
    assert len({t["trial_id"] for t in manifest["trials"]}) == count
    snapshot = tmp_path / "export.json"
    probe.save(
        snapshot,
        {
            "manifest_sha256": prepared["manifest_sha256"],
            "experiment_id": "experiment",
            "database_name": manifest["database_name"],
            "database_id": "test",
            "parent": None,
            "calls": [],
        },
    )
    scored = probe.score(batch, output, snapshot)
    assert scored["denominator"] == count and len(scored["rows"]) == count
    assert all(r["outcome"] == "not_executed" for r in scored["rows"])
    assert scored["supports_candidate"] is False and scored["thresholds_met"] is False
    assert not any(scored["metrics"][g]["correct"] for g in groups)
    if plan_path == probe.TIMING_PLAN:
        records = {
            t["trial_id"]: {
                "state": "known",
                "known_cost": 0,
                "reasoning_tokens": None,
                "elapsed_ms": 1,
            }
            for t in manifest["trials"]
        }
        with monkeypatch.context() as observations:
            observations.setattr(probe, "ledger_calls", lambda *_: records)
            observations.setattr(
                probe,
                "score_final",
                lambda value, *_: {
                    k: value
                    for k in ("protocol", "sources", "correct", "full_evidence")
                },
            )
            for improved_case, regressed_case, supported in (
                (None, None, False),
                ("DEV-026", None, False),
                ("DEV-027", None, True),
                ("DEV-027", "DEV-026", False),
            ):
                observations.setattr(
                    probe,
                    "observed_final",
                    lambda _, trial, __, improved=improved_case, regressed=regressed_case: (
                        not (
                            trial["repetition"] == 1
                            and (
                                trial["group"] == "V8"
                                and trial["case_id"] == improved
                                or trial["group"] == "V8T"
                                and trial["case_id"] == regressed
                            )
                        ),
                        "final",
                        None,
                    ),
                )
                assert (
                    probe.score(batch, output, snapshot)["supports_candidate"]
                    is supported
                )
    descriptor = (
        Path(probe.__file__).parents[2] / "support_diagnosis/probe-contract.json"
    )
    assert probe.read(output / "candidate-sources.json")[
        "tools/support_diagnosis/probe-contract.json"
    ] == probe.digest(descriptor.read_bytes())
    original_read = Path.read_bytes
    with monkeypatch.context() as changed:
        changed.setattr(
            Path,
            "read_bytes",
            lambda p: original_read(p) + (b" " if p == descriptor else b""),
        )
        with pytest.raises(EvidenceError, match="PROBE_CODE_CHANGED"):
            probe.score(batch, output, snapshot)
    with pytest.raises(FileExistsError):
        probe.prepare(
            batch,
            output,
            runner,
            preflight,
            ledger,
            "jobforge_s5_diagnosis_probe",
            "test",
        )
    first = output / manifest["trials"][0]["request_file"]
    first.write_bytes(first.read_bytes() + b" ")
    with pytest.raises(EvidenceError, match="PROBE_REQUEST_CHANGED"):
        probe.score(batch, output, snapshot)


def test_run_requires_real_startup_authorization_before_any_attempt(
    tmp_path: Path,
) -> None:
    """A preparation or technical acceptance cannot start the diagnostic sender."""
    plan = tmp_path / "startup-manifest.json"
    probe.save(
        plan,
        {"policy": {"batch_cap_microyuan": 4000000, "stage_cap_microyuan": 28000000}},
    )
    review = tmp_path / "review.json"
    probe.save(
        review,
        {
            "manifest_sha256": probe.digest(plan.read_bytes()),
            "maintainer": {
                "decision": "pending",
                "identity": "maintainer",
                "batch_cap_microyuan": 4000000,
                "stage_cap_microyuan": 28000000,
            },
            "independent_review": {"decision": "accepted", "identity": "reviewer"},
        },
    )
    with pytest.raises(ValueError, match="STARTUP_REVIEW_REQUIRED"):
        probe.run(tmp_path, review)
    assert not (tmp_path / "attempted.json").exists()


@pytest.mark.parametrize("mutation", ["copied_root", "expired", "not_started"])
def test_run_binds_root_and_authorization_window(tmp_path: Path, mutation: str) -> None:
    """Copied requests or an inactive approval cannot reach locks, PG or sender."""
    now = datetime.now(UTC)
    starts, ends = now - timedelta(minutes=1), now + timedelta(minutes=5)
    if mutation == "expired":
        ends = now - timedelta(seconds=1)
    elif mutation == "not_started":
        starts = now + timedelta(minutes=1)
    root = tmp_path / "original"
    root.mkdir()
    proof = tmp_path / "proof.txt"
    proof.write_bytes(b"synthetic review fixture")
    evidence = {"path": str(proof), "sha256": probe.digest(proof.read_bytes())}
    plan = root / "startup-manifest.json"
    probe.save(
        plan,
        {
            "experiment_root": str(root),
            "valid_from": starts.isoformat(),
            "valid_until": ends.isoformat(),
            "policy": {"batch_cap_microyuan": 4000000, "stage_cap_microyuan": 28000000},
        },
    )
    review = tmp_path / "review.json"
    probe.save(
        review,
        {
            "manifest_sha256": probe.digest(plan.read_bytes()),
            "maintainer": {
                "decision": "authorized",
                "identity": "maintainer",
                "batch_cap_microyuan": 4000000,
                "stage_cap_microyuan": 28000000,
                "evidence": evidence,
            },
            "independent_review": {
                "decision": "accepted",
                "identity": "reviewer",
                "evidence": evidence,
            },
        },
    )
    if mutation == "copied_root":
        copied = tmp_path / "copied"
        copied.mkdir()
        (copied / plan.name).write_bytes(plan.read_bytes())
        root = copied
    expected = (
        "PROBE_ROOT_BINDING" if mutation == "copied_root" else "PROBE_APPROVAL_WINDOW"
    )
    with pytest.raises(EvidenceError, match=expected):
        probe.run(root, review)
    assert not (root / "attempted.json").exists()
