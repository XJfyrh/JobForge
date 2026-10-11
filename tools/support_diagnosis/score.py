"""Score every diagnosis trial offline with the original policy predicates."""

from __future__ import annotations

import argparse
import json
from collections import defaultdict
from pathlib import Path
from typing import Any

from jobforge_agent import support_contract
from jobforge_agent.dispatch import DispatchError
from jobforge_agent.errors import ToolError
from jobforge_agent.http import strict_json

from tools.support_diagnosis.prepare import GROUPS, digest, read, save
from tools.support_evaluation.predicates import (
    Facts,
    claim_sources,
    claim_truth,
    coverage_key,
    derive_facts,
)
from tools.support_s5.package import load_package


def score_final(value: Any, context: dict[str, Any], facts: Facts) -> dict[str, Any]:
    """Keep original shape/truth/coverage checks without fake persisted sources."""
    result: dict[str, Any] = {
        "protocol": False,
        "sources": False,
        "action": False,
        "conclusion": False,
        "full_evidence": False,
        "correct": False,
    }
    if (
        not isinstance(value, dict)
        or set(value) != {"type", "proposal"}
        or value["type"] != "final"
    ):
        return result
    proposal = value["proposal"]
    try:
        # This isolated scorer reuses the historical closed six-field shape.
        support_contract._shape(proposal, persisted=False)
    except (DispatchError, ValueError, TypeError, KeyError):
        return result
    target = {
        "request_information": "awaiting_information",
        "escalate": "escalated",
    }.get(proposal["action"], context["T"]["status"])
    if proposal["target_ticket_status"] != target:
        return result
    result["protocol"] = True
    available = set(context["available_refs"])
    claims = proposal["claims"]
    valid_sources = all(
        set(c["refs"]) <= available
        and all(e in facts.events for e in support_contract.event_ids(c))
        for c in claims
    )
    truth = [claim_truth(c, facts) for c in claims]
    coverage = [claim_sources(c, facts, set(c["refs"])) for c in claims]
    required = facts.required <= {coverage_key(c) for c in claims}
    result.update(
        sources=valid_sources,
        claim_truth=truth,
        claim_coverage=coverage,
        required_coverage=required,
        action=proposal["action"] == facts.expected["action"],
        conclusion=proposal["conclusion"] == facts.expected["conclusion"],
    )
    result["full_evidence"] = (
        valid_sources and all(truth) and all(coverage) and required
    )
    result["correct"] = result["full_evidence"] and all(
        (
            sorted(proposal[k]) == sorted(expected)
            if k == "requested_fields"
            else proposal[k] == expected
        )
        for k, expected in facts.expected.items()
    )
    result["proposal"] = proposal
    return result


def ledger_calls(
    manifest: dict[str, Any], manifest_hash: str, ledger: dict[str, Any]
) -> dict[str, Any]:
    """Bind the read-only PG snapshot to the frozen database and trial schedule."""
    if ledger["manifest_sha256"] != manifest_hash or any(
        ledger[key] != manifest[key]
        for key in ("experiment_id", "database_name", "database_id")
    ):
        raise ValueError("ledger identity mismatch")
    parent = manifest.get("parent")
    if ledger.get("parent") != parent:
        raise ValueError("ledger parent mismatch")
    trials = {t["trial_id"]: (i, t) for i, t in enumerate(manifest["trials"], 1)}
    records = {}
    for row in ledger["calls"]:
        tid = row["trial_id"]
        if tid in records or tid not in trials:
            raise ValueError("unexpected ledger trial")
        ordinal, trial = trials[tid]
        expected_experiment = (
            parent["experiment_id"]
            if parent and ordinal <= parent["completed_trials"]
            else manifest["experiment_id"]
        )
        if (
            row["ordinal"] != ordinal
            or row["experiment_id"] != expected_experiment
            or row["request_sha256"] != trial["request_sha256"]
        ):
            raise ValueError("ledger request mismatch")
        records[tid] = row
    return records


def observed_final(
    experiment: Path,
    trial: dict[str, Any],
    record: dict[str, Any] | None,
    *,
    amended: bool = False,
) -> tuple[Any, str, bool]:
    """Local sidecars cannot assign a response to another paid PG observation."""
    report_path = experiment / "reports" / (trial["trial_id"] + ".json")
    if record is None:
        if report_path.exists():
            raise ValueError("report has no PG call")
        return None, "not_executed", False
    if not report_path.exists():
        return None, "evidence_missing", False
    report = read(report_path)
    encoded = json.dumps(
        report, ensure_ascii=False, separators=(",", ":"), allow_nan=False
    ).encode()
    if digest(encoded) != record["report_sha256"]:
        raise ValueError("PG report hash mismatch")
    raw = (experiment / "responses" / (trial["trial_id"] + ".json")).read_bytes()
    if (
        digest(raw) != record["response_sha256"]
        or record["response_sha256"] != report["response_sha256"]
    ):
        raise ValueError("PG response hash mismatch")
    if (
        report["outcome"] != record["outcome"]
        or report["reasoning_tokens"] != record["reasoning_tokens"]
    ):
        raise ValueError("PG report fields mismatch")
    choice = None
    observed = False
    try:
        choice = json.loads(raw)["choices"][0]
        observed = bool((choice["message"].get("reasoning_content") or "").strip())
    except (ValueError, TypeError, KeyError, IndexError, AttributeError):
        pass
    if "reasoning_observed" in report and report["reasoning_observed"] != observed:
        raise ValueError("reasoning observation mismatch")
    if record["state"] == "known":
        if report["usage"] is None or any(
            report["usage"][key] != record[key]
            for key in ("input_tokens", "output_tokens", "cached_input_tokens")
        ):
            raise ValueError("PG usage mismatch")
        eligible = report["outcome"] == "final" and not report["stop"]
        # Reinterpret only this observed classification in a new analysis, never
        # rewrite the historical sidecar, PG stop reason, or original score.
        if (
            amended
            and trial["group"] == "B"
            and report["outcome"] == report["stop"] == "thinking_not_observed"
        ):
            eligible = True
        if eligible and choice is not None and choice["finish_reason"] == "stop":
            try:
                content = choice["message"]["content"].encode()
                if len(content) > 16384:
                    return None, "invalid_final", observed
                return strict_json(content), report["outcome"], observed
            except (ValueError, TypeError, KeyError, IndexError, ToolError):
                return None, "invalid_final", observed
    return None, report["outcome"], observed


def score(root: Path, experiment: Path, ledger_path: Path) -> dict[str, Any]:
    """Retain all 240 planned trials, including missing or rejected outputs."""
    manifest = read(experiment / "manifest.json")
    manifest_hash = digest((experiment / "manifest.json").read_bytes())
    records = ledger_calls(manifest, manifest_hash, read(ledger_path))
    package = load_package(
        root / "formal-package-v3-r1", root / "resume-20261008/candidate-freeze.json"
    )
    rows = []
    historical = {}
    for trial in manifest["trials"]:
        request_path = experiment / trial["request_file"]
        if digest(request_path.read_bytes()) != trial["request_sha256"]:
            raise ValueError("frozen request changed")
        request = read(request_path)
        context = json.loads(request["messages"][1]["content"])
        facts = derive_facts(
            context["T"],
            context["E1"].get("order"),
            context["E2"].get("delivery"),
            package.anchors,
        )
        cid = trial["case_id"]
        expected = package.gold[cid]["expected"]
        action = {
            "none": "",
            "record_resolution": "record_conclusion",
            "escalate_human": "escalate",
        }.get(expected["action"], expected["action"])
        if (
            action != facts.expected["action"]
            or expected["conclusion"] != facts.expected["conclusion"]
        ):
            raise ValueError("gold and predicates disagree")
        original = read(experiment / "score-inputs" / f"{cid}.json")
        historical[cid] = original["original_proposal"]["conclusion"]
        record = records.get(trial["trial_id"])
        value, outcome, observed = observed_final(
            experiment, trial, record, amended=manifest["schema_version"] == 2
        )
        result = score_final(value, context, facts)
        rows.append(
            {
                **trial,
                "outcome": outcome,
                "observation": record,
                "reasoning_observed": observed,
                "source_scope": "diagnostic-corpus"
                if trial["group"] == "C"
                else "original-retrieval",
                **result,
            }
        )
    metrics = (
        "protocol",
        "sources",
        "action",
        "conclusion",
        "full_evidence",
        "correct",
    )
    totals = {}
    for group in GROUPS:
        subset = [r for r in rows if r["group"] == group]
        totals[group] = {
            "denominator": len(subset),
            **{m: sum(r[m] for r in subset) for m in metrics},
            "correct_by_repetition": [
                sum(r["correct"] for r in subset if r["repetition"] == n)
                for n in range(1, 4)
            ],
            "known_cost_microyuan": sum(
                (r["observation"] or {}).get("known_cost") or 0 for r in subset
            ),
            "usage_known_calls": sum(
                (r["observation"] or {}).get("state") == "known" for r in subset
            ),
            "reasoning_observed_calls": sum(r["reasoning_observed"] for r in subset),
            "elapsed_ms": sum(
                (r["observation"] or {}).get("elapsed_ms") or 0 for r in subset
            ),
            "output_tokens": sum(
                (r["observation"] or {}).get("output_tokens") or 0 for r in subset
            ),
        }
    paired = {}
    base = {(r["case_id"], r["repetition"]): r for r in rows if r["group"] == "A"}
    for group in GROUPS[1:]:
        results: dict[str, list[str]] = defaultdict(list)
        for row in (r for r in rows if r["group"] == group):
            before = base[row["case_id"], row["repetition"]]["correct"]
            after = row["correct"]
            key = (
                "win"
                if after and not before
                else "loss"
                if before and not after
                else "tie"
            )
            results[key].append(row["trial_id"])
        paired[group] = {
            key: {"count": len(results[key]), "trials": results[key]}
            for key in ("win", "loss", "tie")
        }
    return {
        "kind": "s5-seen-final-decision-diagnosis",
        "analysis_rule": "requested-group-intent-to-treat"
        if manifest["schema_version"] == 2
        else "original-observation-gate",
        "thresholds_met": False,
        "manifest_sha256": manifest_hash,
        "ledger_sha256": digest(ledger_path.read_bytes()),
        "denominator": len(rows),
        "groups": totals,
        "paired_against_A": paired,
        "rows": rows,
        "not_end_to_end": True,
        "historical_conclusions": historical,
    }


def main() -> None:
    """Write a new report without altering any model response or historical score."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--experiment", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--ledger", type=Path, required=True)
    args = parser.parse_args()
    report = score(args.root, args.experiment, args.ledger)
    save(args.output, report)
    print(
        json.dumps({"groups": report["groups"], "denominator": report["denominator"]})
    )


if __name__ == "__main__":
    main()
