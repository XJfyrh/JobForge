"""Prepare configured seen-case prompt probes, outside production candidates."""

from __future__ import annotations

import argparse
import copy
import json
import os
import re
import subprocess
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from jobforge_agent.deepseek import prepare_chat_request
from jobforge_agent.dispatch import RunCallContext
from jobforge_agent.support_agent_v8 import FINAL_SYSTEM, SupportAgentV8Adapter

from tools.support_diagnosis.prepare import digest, read, save
from tools.support_diagnosis.score import ledger_calls, observed_final, score_final
from tools.support_evaluation.evidence import (
    Package,
    load_package,
    need,
    validate_run_sources,
)
from tools.support_evaluation.predicates import derive_facts
from tools.support_s5.replay import candidate_sources, observation_budget

CASE_IDS = ("DEV-003", "DEV-004", "DEV-016", "DEV-017", "DEV-026", "DEV-027")
FAILED_CASE_IDS = frozenset({"DEV-004", "DEV-017", "DEV-027"})
CANDIDATE_VERSION = "support-agent-prompt-v8-contract-c1"
# Restore three requirements omitted by v8 compression, without importing gold,
# changing policy priority, or changing the captured user message.
RESTORED_CONTRACT = """
CLAIM COMPLETENESS AND EVIDENCE REVIEW:
1. Every timing claim cites E1#/order/promised_delivery_at and its applicable timing policy. Every outstanding timing claim ALSO cites T#/observed_at AND E2#/delivery/events: the aggregate list proves the absence of a delivered event; a single event or delivery status cannot establish that absence. Event-node references added by code do not replace the aggregate list.
2. Missing relationships are hierarchical. No related order requires ticket.order_id; an order without a delivery association requires order.delivery_id; only an existing delivery with no usable events requires delivery.usable_tracking_events. An existing delivered aggregate without a delivered event requires delivery.delivered_event. A missing upstream relationship blocks claims and requests for its downstream fields. A vague actual problem description is an independent gap, not a downstream tracking field.
3. An active critical finding needs BOTH a critical claim and a separate timing claim when timing is supported by the captured facts. The critical claim establishes the exception; the timing claim establishes whether the promise was missed. A future promise still needs its outstanding_not_overdue timing claim even when the critical exception makes the conclusion insufficient. Each claim needs its own supporting fields and policy. Do not add timing to a conflict or dispute merely to fill slots.
"""
CANDIDATE_SYSTEM = FINAL_SYSTEM + RESTORED_CONTRACT
DESCRIPTION_REMINDER = " A vague actual problem description is an independent gap, not a downstream tracking field."
ABLATION_SYSTEM = CANDIDATE_SYSTEM.replace(DESCRIPTION_REMINDER, "")
ABLATION_PLAN = Path(__file__).with_name("description-probe.json")
TIMING_KIND = "critical-timing-completeness"
TIMING_VERSION = "support-agent-prompt-v8-critical-timing-c1"
TIMING_SYSTEM = (
    FINAL_SYSTEM
    + "\nCRITICAL AND TIMING COMPLETENESS:\n"
    + RESTORED_CONTRACT.split("\n3. ", 1)[1]
)
TIMING_PLAN = Path(__file__).with_name("critical-timing-probe.json")


def load_probe_plan(path: Path) -> dict[str, Any]:
    """Limit configuration to two registered prompt questions and seen cases."""
    plan = read(path)
    need(
        set(plan) == {"kind", "case_ids", "repetitions"}
        and plan["kind"] in {"description-gap-ablation", TIMING_KIND},
        "PROBE_PLAN_KIND",
    )
    cases = plan["case_ids"]
    need(
        isinstance(cases, list)
        and bool(cases)
        and all(
            isinstance(cid, str) and re.fullmatch(r"DEV-(00[1-9]|0[1-3][0-9]|040)", cid)
            for cid in cases
        )
        and len(set(cases)) == len(cases)
        and (
            {"DEV-026", "DEV-027"}.issubset(cases)
            if plan["kind"] == TIMING_KIND
            else "DEV-016" in cases
        ),
        "PROBE_PLAN_CASES",
    )
    need(
        type(plan["repetitions"]) is int and plan["repetitions"] > 0,
        "PROBE_PLAN_REPETITIONS",
    )
    return plan


def probe_groups(plan: dict[str, Any] | None) -> tuple[str, str]:
    """Select the two registered systems without configuring model or execution."""
    if plan is None:
        return ("V8", "V8C")
    return ("V8", "V8T") if plan["kind"] == TIMING_KIND else ("C1", "C2")


def embedded_plan(plan: dict[str, Any]) -> dict[str, Any]:
    """Keep the original schema5 description plan bytes backward compatible."""
    value = {k: plan[k] for k in ("case_ids", "repetitions")}
    if plan["kind"] == TIMING_KIND:
        value["kind"] = TIMING_KIND
    return value


def experiment_plan(
    manifest: dict[str, Any], experiment: Path
) -> dict[str, Any] | None:
    """Bind the declared cohort to the original reviewed configuration bytes."""
    need(manifest["schema_version"] in (4, 5), "PROBE_MANIFEST_VERSION")
    if manifest["schema_version"] == 4:
        need("probe" not in manifest, "PROBE_PLAN_UNEXPECTED")
        return None
    path = experiment / "probe-plan.json"
    need(
        digest(path.read_bytes()) == manifest["sources"]["probe_plan"],
        "PROBE_PLAN_HASH",
    )
    plan = load_probe_plan(path)
    need(
        manifest["probe"] == embedded_plan(plan),
        "PROBE_PLAN_BINDING",
    )
    return plan


def probe_sources() -> dict[str, str]:
    """Also bind the generated descriptor embedded by the isolated sender."""
    sources = candidate_sources()
    descriptor = Path(__file__).parents[2] / "support_diagnosis/probe-contract.json"
    sources["tools/support_diagnosis/probe-contract.json"] = digest(
        descriptor.read_bytes()
    )
    sources["tools/support_s5/host/description-probe.json"] = digest(
        ABLATION_PLAN.read_bytes()
    )
    sources["tools/support_s5/host/critical-timing-probe.json"] = digest(
        TIMING_PLAN.read_bytes()
    )
    return dict(sorted(sources.items()))


def schedule(plan: dict[str, Any] | None = None) -> list[dict[str, Any]]:
    """Fix every observation before sending and alternate prompt order."""
    groups = probe_groups(plan)
    return [
        {
            "trial_id": f"{cid}-{group}-{repetition}",
            "case_id": cid,
            "group": group,
            "repetition": repetition,
            "request_file": f"requests/{cid}-{group}.json",
        }
        for repetition in range(1, 2 if plan is None else plan["repetitions"] + 1)
        for index, cid in enumerate(CASE_IDS if plan is None else plan["case_ids"])
        for group in (groups if (index + repetition - 1) % 2 == 0 else groups[::-1])
    ]


def project_case(
    row: dict[str, Any],
    binding: dict[str, Any],
    profile: dict[str, Any],
    package: Package,
    plan: dict[str, Any] | None = None,
) -> tuple[dict[str, bytes], dict[str, Any]]:
    """Verify every historical chat before changing only the final system text."""
    need(
        profile["executor_version"] == "linux-v2-adjudication-runtime-5",
        "PROBE_SOURCE_VERSION",
    )
    checkpoint, _ = validate_run_sources(row, binding, profile, package)
    checkpoint = json.loads(json.dumps(checkpoint, sort_keys=True))
    indices = {s["record"]["step_id"]: i for i, s in enumerate(row["steps"])}
    chats = [c for c in row["calls"]["items"] if c["subcall"] == "chat"]
    finals = [c for c in chats if c["step_kind"] == "model_proposal"]
    need(len(finals) == 1 and chats[-1] is finals[0], "PROBE_FINAL_COVERAGE")
    verified = []
    baseline = None
    for call in chats:
        prefix = copy.deepcopy(checkpoint)
        prefix["steps"] = prefix["steps"][: indices[call["step_id"]]]
        prefix["next_step"] = {"kind": call["step_kind"]}
        untouched = copy.deepcopy(prefix)
        messages = SupportAgentV8Adapter().proposal_messages(
            prefix, correction=call["step_kind"] == "protocol_correction"
        )
        context = RunCallContext(
            profile["profile_hash"],
            row["run"]["snapshot_hash"],
            "",
            agent=True,
            final_adjudication=call["step_kind"] == "model_proposal",
        )
        request = prepare_chat_request(messages, context=context)
        need(request.parameter_hash == call["parameter_hash"], "PROBE_SOURCE_HASH")
        need(prefix == untouched, "PROBE_SOURCE_MUTATED")
        verified.append(
            {
                "physical_call_id": call["physical_call_id"],
                "sha256": digest(request.body),
            }
        )
        if call is finals[0]:
            baseline = request.body
    if baseline is None:
        raise ValueError("PROBE_FINAL_MISSING")
    need(messages[0]["content"] == FINAL_SYSTEM, "PROBE_BASELINE_PROMPT")
    groups = probe_groups(plan)
    systems = (
        (FINAL_SYSTEM, CANDIDATE_SYSTEM)
        if plan is None
        else (FINAL_SYSTEM, TIMING_SYSTEM)
        if plan["kind"] == TIMING_KIND
        else (CANDIDATE_SYSTEM, ABLATION_SYSTEM)
    )
    bodies = {}
    left = json.loads(baseline)
    for group, system in zip(groups, systems, strict=True):
        projected_messages = copy.deepcopy(list(messages))
        projected_messages[0] = {"role": "system", "content": system}
        raw = prepare_chat_request(projected_messages, context=context).body
        right = json.loads(raw)
        right["messages"][0]["content"] = left["messages"][0]["content"]
        need(left == right, "PROBE_CHANGED_NON_PROMPT_INPUT")
        bodies[group] = raw
    return bodies, {
        "case_id": row["case_id"],
        "source_run_id": row["run"]["run_id"],
        "original_requests": verified,
        "baseline_request_sha256": digest(bodies[groups[0]]),
        "candidate_request_sha256": digest(bodies[groups[1]]),
        "user_message_sha256": digest(left["messages"][1]["content"].encode()),
    }


def inputs(batch: Path) -> tuple[Package, dict[str, Any], dict[str, Any]]:
    """Use the sealed, seen v8 development batch, including all its failures."""
    package = load_package()
    registration = read(batch / "registration.json")
    evidence = read(batch / "evidence.json")
    need(
        {r["case_id"] for r in evidence["cases"]} == set(package.cases),
        "PROBE_ORIGINAL_DENOMINATOR",
    )
    return package, registration, evidence


def projections(
    batch: Path,
    plan: dict[str, Any] | None = None,
) -> tuple[dict[str, dict[str, bytes]], list[dict[str, Any]]]:
    """Return the predetermined failed/control cohort; never select by new scores."""
    package, registration, evidence = inputs(batch)
    bindings = {r["case_id"]: r for r in registration["bindings"]}
    cases = {r["case_id"]: r for r in evidence["cases"]}
    projected, proofs = {}, []
    for cid in CASE_IDS if plan is None else plan["case_ids"]:
        bodies, proof = project_case(
            cases[cid], bindings[cid], registration["profile"], package, plan
        )
        projected[cid] = bodies
        proofs.append(proof)
    return projected, proofs


def prepare(
    batch: Path,
    output: Path,
    runner: Path,
    preflight: Path,
    prior_ledger: Path,
    database_name: str,
    database_id: str,
    plan_path: Path | None = None,
) -> dict[str, Any]:
    """Freeze requests and a cost proposal; this is not a paid authorization."""
    plan = None if plan_path is None else load_probe_plan(plan_path)
    projected, proofs = projections(batch) if plan is None else projections(batch, plan)
    checks, ledger = read(preflight), read(prior_ledger)
    need(checks["ready"] is True, "PROBE_PREFLIGHT")
    need(
        not any(
            ledger["totals"][k]
            for k in ("held", "inflight", "unknown_chat_calls", "unfinished_attempts")
        ),
        "PROBE_PRIOR_EXPOSURE",
    )
    output.mkdir(exist_ok=False)
    (output / "requests").mkdir()
    trials = schedule(plan)
    for trial in trials:
        raw = projected[trial["case_id"]][trial["group"]]
        path = output / trial["request_file"]
        if path.exists():
            need(path.read_bytes() == raw, "PROBE_REPEATED_REQUEST_CHANGED")
        else:
            path.write_bytes(raw)
        trial["request_sha256"] = digest(raw)
    sources = {
        "original_evidence": digest((batch / "evidence.json").read_bytes()),
        "original_registration": digest((batch / "registration.json").read_bytes()),
        "prior_ledger": digest(prior_ledger.read_bytes()),
        "preflight": digest(preflight.read_bytes()),
        "preparation_source": digest(Path(__file__).read_bytes()),
        "candidate_sources": save(output / "candidate-sources.json", probe_sources()),
        "input_verification": save(output / "input-verification.json", proofs),
    }
    manifest = {
        "schema_version": 4 if plan is None else 5,
        "experiment_id": output.name,
        "database_name": database_name,
        "database_id": database_id,
        "created_at": datetime.now(UTC).isoformat(),
        "runner_sha256": digest(runner.read_bytes()),
        "sources": sources,
        "max_input_tokens": 1048576,
        "max_output_tokens": 16384,
        "prior_cost_microyuan": ledger["totals"]["known"],
        "cost_cap_microyuan": 28000000,
        "batch_cap_microyuan": 4000000,
        "pricing": checks["pricing"],
        "trials": trials,
    }
    if plan is not None:
        assert plan_path is not None
        (output / "probe-plan.json").write_bytes(plan_path.read_bytes())
        sources["probe_plan"] = digest((output / "probe-plan.json").read_bytes())
        manifest["probe"] = embedded_plan(plan)
    return {
        "manifest_sha256": save(output / "manifest.json", manifest),
        "candidate_version": CANDIDATE_VERSION
        if plan is None
        else TIMING_VERSION
        if plan["kind"] == TIMING_KIND
        else "support-agent-prompt-v8-contract-c2",
        "original_requests_verified": sum(len(p["original_requests"]) for p in proofs),
        "trials": len(trials),
        "paid_calls": 0,
        "paid_authorization": "startup_review_required",
    }


def score(batch: Path, experiment: Path, ledger_path: Path) -> dict[str, Any]:
    """Keep every planned trial and require complete candidate observations."""
    manifest = read(experiment / "manifest.json")
    plan = experiment_plan(manifest, experiment)
    expected = schedule(plan)
    need(
        [
            {k: t[k] for k in e}
            for t, e in zip(manifest["trials"], expected, strict=True)
        ]
        == expected,
        "PROBE_COHORT",
    )
    for name in ("original_evidence", "original_registration"):
        path = batch / (
            "evidence.json" if name == "original_evidence" else "registration.json"
        )
        need(
            digest(path.read_bytes()) == manifest["sources"][name],
            "PROBE_ORIGINAL_CHANGED",
        )
    for filename, key in (
        ("candidate-sources.json", "candidate_sources"),
        ("input-verification.json", "input_verification"),
    ):
        need(
            digest((experiment / filename).read_bytes()) == manifest["sources"][key],
            "PROBE_PROOF_HASH",
        )
    need(
        read(experiment / "candidate-sources.json") == probe_sources(),
        "PROBE_CODE_CHANGED",
    )
    projected, proofs = projections(batch) if plan is None else projections(batch, plan)
    need(proofs == read(experiment / "input-verification.json"), "PROBE_PROOF_CHANGED")
    records = ledger_calls(
        manifest, digest((experiment / "manifest.json").read_bytes()), read(ledger_path)
    )
    package = load_package()
    rows = []
    for trial in manifest["trials"]:
        raw = projected[trial["case_id"]][trial["group"]]
        need(
            raw == (experiment / trial["request_file"]).read_bytes()
            and digest(raw) == trial["request_sha256"],
            "PROBE_REQUEST_CHANGED",
        )
        context = json.loads(json.loads(raw)["messages"][1]["content"])
        facts = derive_facts(
            context["T"],
            context.get("E1", {}).get("order"),
            context.get("E2", {}).get("delivery"),
            package.anchors,
        )
        record = records.get(trial["trial_id"])
        value, outcome, observed = observed_final(experiment, trial, record)
        rows.append(
            {
                **trial,
                **score_final(value, context, facts),
                "outcome": outcome,
                "reasoning_observed": observed,
                "observation": record,
                "reasoning_budget": observation_budget(record),
            }
        )
    baseline_group, candidate_group = probe_groups(plan)
    groups = {
        group: [r for r in rows if r["group"] == group]
        for group in (baseline_group, candidate_group)
    }
    metrics = {
        g: {
            k: sum(r[k] for r in group)
            for k in ("protocol", "sources", "correct", "full_evidence")
        }
        for g, group in groups.items()
    }
    complete = len(records) == len(expected) and all(
        r["state"] == "known" for r in records.values()
    )
    improved = any(
        r["correct"]
        and not next(
            b
            for b in groups[baseline_group]
            if (b["case_id"], b["repetition"]) == (r["case_id"], r["repetition"])
        )["correct"]
        for r in groups[candidate_group]
        if r["case_id"]
        in (
            FAILED_CASE_IDS
            if plan is None
            else {"DEV-027"}
            if plan["kind"] == TIMING_KIND
            else {"DEV-016"}
        )
    )
    return {
        "kind": "s5-v8-seen-contract-probe"
        if plan is None
        else "s5-v8-critical-timing-completeness"
        if plan["kind"] == TIMING_KIND
        else "s5-v8-description-gap-ablation",
        "thresholds_met": False,
        "not_end_to_end": True,
        "manifest_sha256": digest((experiment / "manifest.json").read_bytes()),
        "denominator": len(expected),
        "metrics": metrics,
        "supports_candidate": complete
        and metrics[candidate_group]["correct"] == len(expected) // 2
        and metrics[candidate_group]["full_evidence"] == len(expected) // 2
        and improved,
        "interpretation": "all_candidate_observations_correct_and_full_with_fresh_paired_improvement_required; not_stability_proof",
        "known_cost_microyuan": sum(r["known_cost"] or 0 for r in records.values()),
        "held_cost_microyuan": sum(
            r["reserved_cost"] for r in records.values() if r["state"] != "known"
        ),
        "rows": rows,
    }


def run(experiment: Path, review: Path) -> dict[str, Any]:
    """Use the shared startup review and stage inventory around the fixed sender."""
    from tools.support_s5.checkpoint import atomic_json
    from tools.support_s5.host import ledger
    from tools.support_s5.host.config import command, validate_review

    experiment = experiment.resolve()
    plan_path = experiment / "startup-manifest.json"
    plan = read(plan_path)
    validate_review(plan_path, review)
    need(experiment == Path(plan["experiment_root"]).resolve(), "PROBE_ROOT_BINDING")
    starts, expires = (
        datetime.fromisoformat(plan[k]) for k in ("valid_from", "valid_until")
    )
    need(starts <= datetime.now(UTC) < expires, "PROBE_APPROVAL_WINDOW")
    for path, expected in plan["bound_files"].items():
        need(digest(Path(path).read_bytes()) == expected, "PROBE_BOUND_FILE_CHANGED")
    need(
        command(["git", "rev-parse", "HEAD"]).decode().strip() == plan["source_commit"]
        and not command(["git", "status", "--porcelain"]).strip(),
        "PROBE_REQUIRES_BOUND_CLEAN_COMMIT",
    )
    settings = read(Path(plan["settings"]))
    manifest = read(experiment / "manifest.json")
    experiment_plan(manifest, experiment)
    need(not (experiment / "attempted.json").exists(), "PROBE_ALREADY_ATTEMPTED")
    lock = Path(settings["stage_lock"])
    head = Path(str(lock) + ".inventory.json")
    with lock.open("x", encoding="utf-8") as output:
        json.dump(
            {"pid": os.getpid(), "manifest_sha256": digest(plan_path.read_bytes())},
            output,
        )
    registered = False
    try:
        active = command(["docker", "ps", "--format", "{{.Names}}"])
        need(
            not any(
                "jobforge-s5-" in n and ("-paid" in n or "worker" in n)
                for n in active.decode().splitlines()
            ),
            "PROBE_CONCURRENT_SENDER",
        )
        baseline = ledger.collect(settings["inventory"])
        ledger.validate_history(read(Path(settings["prior_ledger"])), baseline)
        ledger.validate_stage(read(head), baseline, manifest["database_name"])
        need(
            not any(
                baseline["totals"][k]
                for k in (
                    "held",
                    "unknown_chat_calls",
                    "inflight",
                    "unfinished_attempts",
                )
            ),
            "PROBE_LEDGER_NOT_CLOSED",
        )
        current = next(
            p for p in baseline["phases"] if p["database"] == manifest["database_name"]
        )
        need(not any(current[k] for k in ledger.COUNTS), "PROBE_DATABASE_NOT_EMPTY")
        need(
            baseline["totals"]["known"] == manifest["prior_cost_microyuan"],
            "PROBE_PRIOR_COST_CHANGED",
        )
        save(
            experiment / "attempted.json",
            {
                "startup_manifest_sha256": digest(plan_path.read_bytes()),
                "pid": os.getpid(),
            },
        )
        atomic_json(head, baseline)
        registered = True
        env = {
            k: v for k, v in os.environ.items() if not k.upper().startswith("JOBFORGE_")
        }
        env["JOBFORGE_DIAGNOSIS_DSN"] = (
            Path(settings["dsn_file"]).read_text(encoding="utf-8").strip()
        )
        with (
            (experiment / "sender-stdout.log").open("xb") as stdout,
            (experiment / "sender-stderr.log").open("xb") as stderr,
        ):
            remaining = (expires - datetime.now(UTC)).total_seconds()
            need(remaining > 0, "PROBE_APPROVAL_WINDOW")
            process = subprocess.run(
                [
                    settings["runner"],
                    "--manifest",
                    str(experiment / "manifest.json"),
                    "--credentials",
                    settings["credentials"],
                    "--deadline",
                    plan["valid_until"],
                ],
                env=env,
                stdout=stdout,
                stderr=stderr,
                check=False,
                timeout=remaining + 15,
            )
        save(
            experiment / "sender-exit.json",
            {
                "exit_code": process.returncode,
                "recorded_at": datetime.now(UTC).isoformat(),
            },
        )
    finally:
        if registered:
            closed = ledger.collect(settings["inventory"])
            ledger.validate_history(read(head), closed)
            atomic_json(head, closed)
            save(experiment / "closed-ledger.json", closed)
            if any(
                closed["totals"][k]
                for k in (
                    "held",
                    "unknown_chat_calls",
                    "inflight",
                    "unfinished_attempts",
                )
            ):
                save(
                    experiment / "cleanup-error.json",
                    {"code": "UNRESOLVED_EXPOSURE", "stage_lock_retained": True},
                )
                raise ValueError("PROBE_UNRESOLVED_EXPOSURE")
        lock.unlink()
    exports = list(experiment.glob("ledger-export-*.json"))
    need(len(exports) == 1, "PROBE_SINGLE_EXPORT_REQUIRED")
    scored = score(Path(settings["batch"]), experiment, exports[0])
    save(experiment / "score.json", scored)
    result = {
        "engineering_complete": True,
        "model_quality_passed": False,
        "scope": "diagnostic_not_development_acceptance",
        "supports_candidate": scored["supports_candidate"],
        "post_batch_review": "pending",
    }
    save(experiment / "result.json", result)
    return result


def main() -> None:
    """Prepare/score offline or invoke the reviewed Go sender for paid HTTP."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("prepare", "score", "run"))
    parser.add_argument("--batch", type=Path)
    parser.add_argument("--output", type=Path)
    for name in ("runner", "preflight", "prior-ledger", "experiment", "ledger", "plan"):
        parser.add_argument("--" + name, type=Path)
    parser.add_argument("--database-name")
    parser.add_argument("--database-id")
    parser.add_argument("--review", type=Path)
    args = parser.parse_args()
    if args.mode == "run":
        if args.experiment is None or args.review is None:
            parser.error("missing startup review or experiment")
        print(json.dumps(run(args.experiment, args.review)))
        return
    if args.batch is None or args.output is None:
        parser.error("missing offline batch or output")
    if args.mode == "prepare":
        if any(
            getattr(args, k) is None
            for k in (
                "runner",
                "preflight",
                "prior_ledger",
                "database_name",
                "database_id",
            )
        ):
            parser.error("missing preparation arguments")
        print(
            json.dumps(
                prepare(
                    args.batch,
                    args.output,
                    args.runner,
                    args.preflight,
                    args.prior_ledger,
                    args.database_name,
                    args.database_id,
                    args.plan,
                )
            )
        )
    else:
        if args.experiment is None or args.ledger is None:
            parser.error("missing score arguments")
        save(args.output, score(args.batch, args.experiment, args.ledger))


if __name__ == "__main__":
    main()
