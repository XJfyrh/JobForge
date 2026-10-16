"""Prepare and score v5 final-only replay; only the isolated Go runner sends."""

from __future__ import annotations

import argparse
import copy
import json
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from jobforge_agent.deepseek import prepare_chat_request
from jobforge_agent.dispatch import RunCallContext
from jobforge_agent.support_agent_v4 import SupportAgentV4Adapter
from jobforge_agent.support_agent_v5 import SupportAgentV5Adapter

from tools.support_diagnosis.prepare import digest, read, save
from tools.support_diagnosis.score import ledger_calls, observed_final, score_final
from tools.support_evaluation.evidence import Package, need, validate_run_sources
from tools.support_evaluation.predicates import derive_facts
from tools.support_evaluation.score import check_calls, evaluate_case
from tools.support_s5.adjudication import (
    definition_closure,
    marker_index,
    retrieval_closure,
)
from tools.support_s5.freeze import ROOT, source_hashes
from tools.support_s5.package import load_package
from tools.support_s5.score import full_case_evidence

CASE_IDS = tuple(f"S5V3-{i:03d}" for i in range(1, 21))
SOURCE_PROFILE = "844fae5cc8b41dc714c8e387c52514129279d7d502d5f546b49881e32c660c10"
SOURCE_FILES = {
    "original_evidence": "adjudication-20261011/seen-v3-agent/evidence.json",
    "original_registration": "adjudication-20261011/seen-v3-agent/registration.json",
    "historical_package": "formal-package-v3-r1/manifest.json",
    "historical_freeze": "resume-20261008/candidate-freeze.json",
}


def candidate_sources() -> dict[str, str]:
    """Bind the actual working tree, including the reused diagnostic toolchain."""
    sources = source_hashes()
    for path in (ROOT / "tools/support_diagnosis").rglob("*"):
        if path.suffix in {".py", ".go", ".sql"}:
            sources[path.relative_to(ROOT).as_posix()] = digest(path.read_bytes())
    return dict(sorted(sources.items()))


def project_case(
    row: dict[str, Any],
    binding: dict[str, Any],
    profile: dict[str, Any],
    package: Package,
) -> tuple[bytes, dict[str, Any]]:
    """Rebuild every original v4 chat hash before projecting its final with v5."""
    checkpoint, _ = validate_run_sources(row, binding, profile, package)
    check_calls(row, profile)
    # The original Go JSON encoder sorts object keys before Python receives them.
    checkpoint = json.loads(json.dumps(checkpoint, sort_keys=True))
    indices = {s["record"]["step_id"]: i for i, s in enumerate(row["steps"])}
    calls = [c for c in row["calls"]["items"] if c["subcall"] == "chat"]
    finals = [c for c in calls if c["step_kind"] == "model_proposal"]
    need(len(finals) == 1 and calls[-1] is finals[0], "REPLAY_FINAL_COVERAGE")
    marker = marker_index(checkpoint)
    final_index = indices[finals[0]["step_id"]]
    need(marker is not None and marker + 1 == final_index, "REPLAY_MARKER_BOUNDARY")
    verified = []
    for call in calls:
        prefix = copy.deepcopy(checkpoint)
        prefix["steps"] = prefix["steps"][: indices[call["step_id"]]]
        prefix["next_step"] = {"kind": call["step_kind"]}
        untouched = copy.deepcopy(prefix)
        messages = SupportAgentV4Adapter().proposal_messages(
            prefix, correction=call["step_kind"] == "protocol_correction"
        )
        context = RunCallContext(
            profile["profile_hash"],
            row["run"]["snapshot_hash"],
            "",
            agent=True,
            final_adjudication=call["step_kind"] == "model_proposal",
        )
        original = prepare_chat_request(messages, context=context)
        need(original.parameter_hash == call["parameter_hash"], "REPLAY_SOURCE_HASH")
        need(prefix == untouched, "REPLAY_SOURCE_MUTATED")
        verified.append(
            {
                "physical_call_id": call["physical_call_id"],
                "parameter_hash": original.parameter_hash,
                "body_sha256": digest(original.body),
            }
        )
    # This is a diagnostic projection of old evidence, not a new Run or profile.
    projected = SupportAgentV5Adapter().proposal_messages(prefix, correction=False)
    request = prepare_chat_request(projected, context=context)
    need(prefix == untouched, "REPLAY_SOURCE_MUTATED")
    original_score = evaluate_case(row, binding, profile, package)
    return request.body, {
        "case_id": row["case_id"],
        "source_run_id": row["run"]["run_id"],
        "marker_index": marker,
        "original_requests": verified,
        "request_sha256": digest(request.body),
        "original_final": {
            "protocol": original_score["protocol"] == "passed",
            "sources": original_score["source"] == "passed",
            "correct": original_score["correct"],
            "full_evidence": full_case_evidence(original_score),
        },
        "retrieval_closure": retrieval_closure(checkpoint, package),
        "definition_closure": definition_closure(checkpoint, package),
    }


def source_inputs(root: Path) -> tuple[Package, dict[str, Any], dict[str, Any]]:
    """Read the original, already seen package with its historical freeze."""
    package = load_package(
        root / "formal-package-v3-r1",
        root / "resume-20261008/candidate-freeze.json",
        historical=True,
    )
    phase = root / "adjudication-20261011/seen-v3-agent"
    registration = read(phase / "registration.json")
    evidence = read(phase / "evidence.json")
    need(
        registration["profile"]["profile_hash"] == SOURCE_PROFILE
        and registration["profile"]["executor_version"]
        == "linux-v2-adjudication-runtime-1",
        "REPLAY_SOURCE_PROFILE",
    )
    for rows in (registration["bindings"], evidence["cases"]):
        need(
            len(rows) == 20 and {r["case_id"] for r in rows} == set(CASE_IDS),
            "REPLAY_SOURCE_COHORT",
        )
    return package, registration, evidence


def prepare(
    root: Path,
    output: Path,
    runner: Path,
    preflight: Path,
    authorisation: Path,
    database_name: str,
    database_id: str,
) -> dict[str, Any]:
    """Materialize exactly twenty immutable requests without network access."""
    package, registration, evidence = source_inputs(root)
    checks, ledger = (
        read(preflight / "receipt.json"),
        read(preflight / "prior-ledger.json"),
    )
    totals = ledger["totals"]
    need(checks["ready"] is True, "REPLAY_PREFLIGHT")
    need(
        not any(
            totals[k]
            for k in ("held", "inflight", "unknown_chat_calls", "unfinished_attempts")
        ),
        "REPLAY_PRIOR_EXPOSURE",
    )
    approval = read(authorisation)
    need(
        approval["batch_cap_microyuan"] == 4000000
        and approval["s5_cap_microyuan"] == 28000000,
        "REPLAY_COST_AUTHORISATION",
    )
    bindings = {b["case_id"]: b for b in registration["bindings"]}
    cases = {r["case_id"]: r for r in evidence["cases"]}
    projected = [
        project_case(cases[cid], bindings[cid], registration["profile"], package)
        for cid in CASE_IDS
    ]
    verified = sum(len(v["original_requests"]) for _, v in projected)
    need(verified == 118, "REPLAY_SOURCE_CALL_COVERAGE")
    output.mkdir(exist_ok=False)
    (output / "requests").mkdir()
    trials = []
    for cid, (body, _) in zip(CASE_IDS, projected, strict=True):
        filename = f"requests/{cid}-V5.json"
        with (output / filename).open("xb") as stream:
            stream.write(body)
        trials.append(
            {
                "trial_id": f"{cid}-V5-1",
                "case_id": cid,
                "group": "V5",
                "repetition": 1,
                "request_file": filename,
                "request_sha256": digest(body),
            }
        )
    sources = {
        "candidate_sources": save(
            output / "candidate-sources.json", candidate_sources()
        ),
        "input_verification": save(
            output / "input-verification.json",
            {
                "original_requests_verified": verified,
                "cases": [v for _, v in projected],
            },
        ),
        **{
            key: digest((root / path).read_bytes())
            for key, path in SOURCE_FILES.items()
        },
        "prior_ledger": digest((preflight / "prior-ledger.json").read_bytes()),
        "preflight": digest((preflight / "receipt.json").read_bytes()),
        "preparation_source": digest(Path(__file__).read_bytes()),
        "budget_authorisation": digest(authorisation.read_bytes()),
    }
    manifest = {
        "schema_version": 3,
        "experiment_id": output.name,
        "database_name": database_name,
        "database_id": database_id,
        "created_at": datetime.now(UTC).isoformat(),
        "runner_sha256": digest(runner.read_bytes()),
        "sources": sources,
        "max_input_tokens": 1048576,
        "max_output_tokens": 16384,
        "prior_cost_microyuan": totals["known"],
        "cost_cap_microyuan": 28000000,
        "batch_cap_microyuan": 4000000,
        "pricing": {
            "hash": checks["pricing_sha256"],
            "denominator": checks["reviewed_tariff"]["denominator"],
            "input_miss_microyuan": checks["reviewed_tariff"]["miss"],
            "input_hit_microyuan": checks["reviewed_tariff"]["hit"],
            "output_microyuan": checks["reviewed_tariff"]["output"],
        },
        "trials": trials,
    }
    return {
        "manifest_sha256": save(output / "manifest.json", manifest),
        "original_requests_verified": verified,
        "trials": len(trials),
        "paid_calls": 0,
    }


def score(root: Path, experiment: Path, ledger_path: Path) -> dict[str, Any]:
    """Keep every planned replay in the denominator and preserve inherited gaps."""
    manifest = read(experiment / "manifest.json")
    need(manifest["schema_version"] == 3, "REPLAY_MANIFEST_VERSION")
    manifest_hash = digest((experiment / "manifest.json").read_bytes())
    records = ledger_calls(manifest, manifest_hash, read(ledger_path))
    for name, key in (
        ("candidate-sources.json", "candidate_sources"),
        ("input-verification.json", "input_verification"),
    ):
        need(
            digest((experiment / name).read_bytes()) == manifest["sources"][key],
            "REPLAY_ARTIFACT_HASH",
        )
    need(
        read(experiment / "candidate-sources.json") == candidate_sources(),
        "REPLAY_SOURCE_CHANGED",
    )
    for key, path in SOURCE_FILES.items():
        need(
            digest((root / path).read_bytes()) == manifest["sources"][key],
            "REPLAY_ORIGINAL_HASH",
        )
    package, registration, evidence = source_inputs(root)
    inputs = {
        r["case_id"]: r for r in read(experiment / "input-verification.json")["cases"]
    }
    cases = {r["case_id"]: r for r in evidence["cases"]}
    bindings = {b["case_id"]: b for b in registration["bindings"]}
    need(tuple(t["case_id"] for t in manifest["trials"]) == CASE_IDS, "REPLAY_COHORT")
    rows = []
    for trial in manifest["trials"]:
        cid = trial["case_id"]
        body, verified = project_case(
            cases[cid], bindings[cid], registration["profile"], package
        )
        need(verified == inputs[cid], "REPLAY_SOURCE_PROOF_CHANGED")
        need(
            body == (experiment / trial["request_file"]).read_bytes()
            and digest(body) == trial["request_sha256"],
            "REPLAY_REQUEST_CHANGED",
        )
        context = json.loads(json.loads(body)["messages"][1]["content"])
        facts = derive_facts(
            context["T"],
            context["E1"].get("order"),
            context["E2"].get("delivery"),
            package.anchors,
        )
        record = records.get(trial["trial_id"])
        value, outcome, observed = observed_final(experiment, trial, record)
        rows.append(
            {
                **trial,
                **score_final(value, context, facts),
                "outcome": outcome,
                "observation": record,
                "reasoning_observed": observed,
                "original_final": verified["original_final"],
                "inherited_retrieval_closure": verified["retrieval_closure"],
                "inherited_definition_closure": verified["definition_closure"],
                "reasoning_budget": observation_budget(record),
            }
        )
    return {
        "kind": "s5-v5-seen-final-replay",
        "thresholds_met": False,
        "not_end_to_end": True,
        "manifest_sha256": manifest_hash,
        "ledger_sha256": digest(ledger_path.read_bytes()),
        "denominator": 20,
        "metrics": {
            key: sum(r[key] for r in rows)
            for key in ("protocol", "sources", "correct", "full_evidence")
        },
        "usage_known_calls": sum(r["state"] == "known" for r in records.values()),
        "known_cost_microyuan": sum(r["known_cost"] or 0 for r in records.values()),
        "held_cost_microyuan": sum(
            r["reserved_cost"] for r in records.values() if r["state"] != "known"
        ),
        "rows": rows,
    }


def observation_budget(record: dict[str, Any] | None) -> dict[str, Any] | None:
    """Missing reasoning usage stays null; elapsed time is HTTP wall time."""
    if record is None or record["state"] != "known":
        return None
    reasoning = record["reasoning_tokens"]
    wall = record["elapsed_ms"] / 1000
    return {
        "reasoning_tokens": reasoning,
        "completion_capacity": 16384,
        "ratio": reasoning / 16384 if reasoning is not None else None,
        "wall_seconds": wall,
        "deadline_seconds": 120,
        "deadline_headroom_seconds": 120 - wall,
        "truncation_risk": wall > 96
        or (reasoning is not None and reasoning > 16384 * 0.9),
    }


def main() -> None:
    """Prepare or score offline; neither mode owns credentials or HTTP transport."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("prepare", "score"))
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--runner", type=Path)
    parser.add_argument("--preflight", type=Path)
    parser.add_argument("--authorisation", type=Path)
    parser.add_argument("--database-name")
    parser.add_argument("--database-id")
    parser.add_argument("--experiment", type=Path)
    parser.add_argument("--ledger", type=Path)
    args = parser.parse_args()
    required = (
        ("runner", "preflight", "authorisation", "database_name", "database_id")
        if args.mode == "prepare"
        else ("experiment", "ledger")
    )
    if any(getattr(args, key) is None for key in required):
        parser.error("missing arguments for selected mode")
    if args.mode == "prepare":
        result = prepare(
            args.root,
            args.output,
            args.runner,
            args.preflight,
            args.authorisation,
            args.database_name,
            args.database_id,
        )
    else:
        result = score(args.root, args.experiment, args.ledger)
        save(args.output, result)
        result = {k: v for k, v in result.items() if k != "rows"}
    print(json.dumps(result))


if __name__ == "__main__":
    main()
