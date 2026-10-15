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


def test_probe_fixes_schedule_and_never_scores_missing_trials_as_passed(
    tmp_path: Path, monkeypatch
) -> None:
    """Unattempted trials stay in the fixed denominator and artifacts are immutable."""
    package, registration, row = source_fixture()
    bodies, proof = probe.project_case(
        row, registration["bindings"][0], registration["profile"], package
    )
    projected = {cid: bodies for cid in probe.CASE_IDS}
    proofs = [dict(proof, case_id=cid) for cid in probe.CASE_IDS]
    monkeypatch.setattr(probe, "projections", lambda _: (projected, proofs))
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
        batch, output, runner, preflight, ledger, "jobforge_s5_diagnosis_probe", "test"
    )
    assert prepared["paid_calls"] == 0 and prepared["trials"] == 12
    manifest = probe.read(output / "manifest.json")
    assert {
        g: sum(t["group"] == g for t in manifest["trials"]) for g in ("V8", "V8C")
    } == {"V8": 6, "V8C": 6}
    assert len({t["trial_id"] for t in manifest["trials"]}) == 12
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
    assert scored["denominator"] == 12 and len(scored["rows"]) == 12
    assert all(r["outcome"] == "not_executed" for r in scored["rows"])
    assert scored["supports_candidate"] is False and scored["thresholds_met"] is False
    assert not any(scored["metrics"][g]["correct"] for g in ("V8", "V8C"))
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
