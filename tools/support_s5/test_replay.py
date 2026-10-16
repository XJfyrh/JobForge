"""Offline v4 request reconstruction and v5 projection with synthetic sources."""

from __future__ import annotations

import copy
import json
from pathlib import Path
from types import SimpleNamespace

import pytest
from jobforge_agent.deepseek import prepare_chat_request
from jobforge_agent.dispatch import RunCallContext
from jobforge_agent.support_agent_v4 import SupportAgentV4Adapter
from jobforge_agent.support_agent_v5 import FINAL_EXCLUDED_FACTS, FINAL_SYSTEM

from tools.support_evaluation.evidence import EvidenceError, validate_run_sources
from tools.support_s5 import replay
from tools.support_s5.replay import project_case
from tools.support_s5.test_adjudication import agent_fixture


def replay_fixture():
    """Bind synthetic source calls to the same canonical bytes as the runtime."""
    package, reg, row = agent_fixture(draft_correct=False, final="correct")
    checkpoint, _ = validate_run_sources(
        row, reg["bindings"][0], reg["profile"], package
    )
    checkpoint = json.loads(json.dumps(checkpoint, sort_keys=True))
    indices = {e["record"]["step_id"]: i for i, e in enumerate(row["steps"])}
    for call in row["calls"]["items"]:
        if call["subcall"] != "chat":
            continue
        prefix = {
            **checkpoint,
            "steps": checkpoint["steps"][: indices[call["step_id"]]],
            "next_step": {"kind": call["step_kind"]},
        }
        messages = SupportAgentV4Adapter().proposal_messages(prefix, correction=False)
        request = prepare_chat_request(
            messages,
            context=RunCallContext(
                reg["profile"]["profile_hash"],
                row["run"]["snapshot_hash"],
                "",
                agent=True,
                final_adjudication=call["step_kind"] == "model_proposal",
            ),
        )
        call["parameter_hash"] = request.parameter_hash
    return package, reg, row


def test_replay_preserves_sources_and_uses_real_v5_final_projection() -> None:
    """The old draft is wrong but neither it nor tool affordances reach v5."""
    package, reg, row = replay_fixture()
    before = copy.deepcopy(row)
    raw, proof = project_case(row, reg["bindings"][0], reg["profile"], package)
    assert row == before
    body = json.loads(raw)
    assert body["messages"][0]["content"] == FINAL_SYSTEM
    facts = json.loads(body["messages"][1]["content"])
    assert facts["final_stage"] is True
    assert not set(FINAL_EXCLUDED_FACTS) & facts.keys()
    assert "draft" not in facts
    assert body["thinking"] == {"type": "enabled"}
    assert body["reasoning_effort"] == "high" and body["max_tokens"] == 16384
    assert "temperature" not in body
    assert len(proof["original_requests"]) == 5
    assert proof["original_final"]["correct"]


@pytest.mark.parametrize(
    "mutation", ["hash", "source", "missing_final", "duplicate_final"]
)
def test_replay_rejects_unverified_originals(mutation: str) -> None:
    """A projected request cannot erase a broken source or call binding."""
    package, reg, row = replay_fixture()
    final = next(c for c in row["calls"]["items"] if c["step_kind"] == "model_proposal")
    if mutation == "hash":
        final["parameter_hash"] = "f" * 64
    elif mutation == "source":
        row["steps"][0]["record"]["commit_hash"] = "f" * 64
    elif mutation == "missing_final":
        row["calls"]["items"].remove(final)
    else:
        row["calls"]["items"].append(copy.deepcopy(final))
    with pytest.raises((EvidenceError, ValueError)):
        project_case(row, reg["bindings"][0], reg["profile"], package)


def prepared_fixture(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    """Synthetic orchestration inputs, distinct from projection/source tests."""
    root, preflight = tmp_path / "root", tmp_path / "preflight"
    root.mkdir()
    preflight.mkdir()
    for path in replay.SOURCE_FILES.values():
        target = root / path
        target.parent.mkdir(parents=True, exist_ok=True)
        replay.save(target, {"synthetic": path})
    replay.save(
        preflight / "receipt.json",
        {
            "ready": True,
            "pricing_sha256": "a" * 64,
            "reviewed_tariff": {
                "denominator": 1000000,
                "miss": 2000000,
                "hit": 40000,
                "output": 8000000,
            },
        },
    )
    replay.save(
        preflight / "prior-ledger.json",
        {
            "totals": {
                "known": 9275641,
                "held": 0,
                "inflight": 0,
                "unknown_chat_calls": 0,
                "unfinished_attempts": 0,
            },
        },
    )
    approval, runner = tmp_path / "approval.json", tmp_path / "runner"
    replay.save(
        approval, {"batch_cap_microyuan": 4000000, "s5_cap_microyuan": 28000000}
    )
    runner.write_bytes(b"synthetic runner, never executed")
    rows = [{"case_id": cid} for cid in replay.CASE_IDS]
    monkeypatch.setattr(replay, "candidate_sources", lambda: {"synthetic": "a" * 64})
    monkeypatch.setattr(
        replay,
        "source_inputs",
        lambda _: (
            SimpleNamespace(anchors={}),
            {"bindings": rows, "profile": {}},
            {"cases": rows},
        ),
    )

    def projected(row, *_):
        cid = row["case_id"]
        body = json.dumps(
            {"messages": [{}, {"content": json.dumps({"T": {}, "E1": {}, "E2": {}})}]}
        ).encode()
        return body, {
            "case_id": cid,
            "original_requests": [{}] * (5 if cid in replay.CASE_IDS[-2:] else 6),
            "original_final": {},
            "retrieval_closure": {},
            "definition_closure": {},
        }

    monkeypatch.setattr(replay, "project_case", projected)
    monkeypatch.setattr(replay, "derive_facts", lambda *_: None)
    experiment = tmp_path / "synthetic-replay"
    result = replay.prepare(
        root, experiment, runner, preflight, approval, "synthetic", "synthetic"
    )
    assert result["original_requests_verified"] == 118 and result["paid_calls"] == 0
    manifest = replay.read(experiment / "manifest.json")
    ledger = tmp_path / "ledger.json"
    replay.save(
        ledger,
        {
            **{
                key: manifest[key]
                for key in ("experiment_id", "database_id", "database_name")
            },
            "manifest_sha256": result["manifest_sha256"],
            "parent": None,
            "calls": [],
        },
    )
    return root, experiment, ledger


def test_replay_unexecuted_trials_are_retained(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """No observations means twenty failures, never a smaller passing cohort."""
    root, experiment, ledger = prepared_fixture(tmp_path, monkeypatch)
    result = replay.score(root, experiment, ledger)
    assert result["denominator"] == len(result["rows"]) == 20
    assert result["metrics"]["correct"] == result["usage_known_calls"] == 0
    assert not result["thresholds_met"] and result["not_end_to_end"]
    assert {r["outcome"] for r in result["rows"]} == {"not_executed"}


@pytest.mark.parametrize("key", list(replay.SOURCE_FILES))
@pytest.mark.parametrize("mutation", ["manifest_hash", "original_bytes"])
def test_replay_binds_all_original_input_bytes(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    key: str,
    mutation: str,
) -> None:
    """Even equivalent JSON cannot replace a reviewed evidence/contract file."""
    root, experiment, ledger = prepared_fixture(tmp_path, monkeypatch)
    if mutation == "manifest_hash":
        path = experiment / "manifest.json"
        manifest = replay.read(path)
        manifest["sources"][key] = "f" * 64
        path.write_text(json.dumps(manifest), encoding="utf-8")
        snapshot = replay.read(ledger)
        snapshot["manifest_sha256"] = replay.digest(path.read_bytes())
        ledger.write_text(json.dumps(snapshot), encoding="utf-8")
    else:
        path = root / replay.SOURCE_FILES[key]
        path.write_bytes(path.read_bytes() + b" ")
    with pytest.raises(EvidenceError, match="REPLAY_ORIGINAL_HASH"):
        replay.score(root, experiment, ledger)


def test_unknown_reasoning_is_not_zero() -> None:
    """A known bill need not include a separately reported reasoning count."""
    assert replay.observation_budget(None) is None
    assert replay.observation_budget({"state": "unknown"}) is None
    measured = replay.observation_budget(
        {"state": "known", "reasoning_tokens": None, "elapsed_ms": 1000}
    )
    assert measured is not None and measured["ratio"] is None
    assert measured["reasoning_tokens"] is None
