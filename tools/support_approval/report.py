"""Independently assess original proposal quality and S4 action mechanisms offline."""

from __future__ import annotations

import argparse
import json
from collections import Counter
from pathlib import Path
from typing import Any

from jobforge.run_actions import ActionCallsResponse, ActionReceipt, EffectView

from tools.support_approval.plan import validate
from tools.support_approval.quality import evaluate_pending
from tools.support_evaluation.evidence import canonical, fingerprint, load_package, need
from tools.support_evaluation.export import json_bytes
from tools.support_evaluation.validate_data import instant


def contract_hash(domain: str, value: dict[str, Any]) -> str:
    """Hash the fixed action canonical JSON without binary float conversion."""
    return fingerprint(domain, "1", canonical(json.dumps(value, ensure_ascii=False)))


def authorization_hash(auth: dict[str, Any]) -> str:
    """Use the shared Go v1 field sequence and canonical version-vector digest."""
    keys = (
        "key_id",
        "operation",
        "tenant_id",
        "business_request_id",
        "business_request_created_at",
        "operation_id",
        "run_id",
        "approval_id",
        "actor_id",
        "decided_at",
        "proposal_hash",
        "parameters_hash",
        "snapshot_id",
        "snapshot_hash",
    )
    vector = contract_hash(
        "jobforge.business.version-vector.v1", auth["version_vector"]
    )
    return fingerprint(
        "jobforge.business.authorization.v1",
        "1",
        *(str(auth[key]) for key in keys),
        vector,
        *(
            str(auth[key])
            for key in (
                "authorized_at",
                "permission_expires_at",
                "authorization_expires_at",
                "run_deadline",
            )
        ),
    )


def call_facts(value: dict[str, Any]) -> dict[str, Any]:
    """Ignore only the API sampling timestamp in a derived comparison."""
    return {key: data for key, data in value.items() if key != "captured_at"}


def checked_quality(
    row: dict[str, Any],
    frozen: dict[str, Any],
    archive: Path,
    profile: dict[str, Any],
) -> dict[str, Any] | None:
    """Score only the exact persisted pending projection, never row declarations."""
    if not (archive / "quality-input.json").is_file():
        return None
    try:
        candidate = json.loads((archive / "quality-input.json").read_bytes())
        pending = json.loads((archive / "pending.json").read_bytes())
        need(
            all(
                candidate[key] == pending[key]
                for key in ("run", "result", "steps", "calls", "effect", "action_calls")
            )
            and candidate.get("approval") == pending.get("approval"),
            "QUALITY_PENDING_CHANGED",
        )
        need(
            candidate["case_id"] == row["case_id"] == frozen["case_id"]
            and candidate["run"]["run_id"] == row["run_id"]
            and candidate["run"]["profile_id"] == profile["profile_id"]
            and candidate["run"]["profile_hash"] == profile["profile_hash"],
            "QUALITY_IDENTITY_CHANGED",
        )
        if candidate["run"]["state"] == "awaiting_approval":
            approval = candidate["approval"]
            need(
                approval["run_id"] == row["run_id"]
                and approval["status"] == "pending"
                and approval["proposal_ref"] == candidate["result"]["ref"]
                and approval["proposal"]
                == candidate["steps"][-1]["record"]["output"]["proposal"],
                "QUALITY_PROPOSAL_CHANGED",
            )
        return evaluate_pending(candidate, frozen["binding"], profile, load_package())
    except (ValueError, KeyError, TypeError, IndexError, OSError) as error:
        return {"approval_eligible": False, "errors": [evidence_error(error)]}


def evidence_error(error: Exception) -> str:
    """Expose a bounded contract diagnostic without copying protected content."""
    return (
        str(error)
        if (
            isinstance(error, ValueError)
            and str(error).isupper()
            and len(str(error)) <= 64
        )
        else "INCOMPLETE_EVIDENCE"
    )


def batch_audit(
    plan: dict[str, Any],
    rows: list[dict[str, Any]],
    output: Path,
    traces: Path | None = None,
) -> dict[str, Any]:
    """Account for uncertain ACKs, all actual Runs and full ledger exposure."""
    result: dict[str, Any] = {
        "state": "failed",
        "errors": [],
        "actual_new_runs": None,
        "usage": None,
        "ledger_runs": [],
    }
    try:
        control = json.loads((output / "control-audit-final.json").read_bytes())
        business = json.loads((output / "business-actions-final.json").read_bytes())
        need(control["batch_account_id"] == plan["batch_account_id"], "BATCH_CHANGED")
        actual = control["runs"]
        result["actual_new_runs"] = len(actual)
        need(
            len({r["run_id"] for r in actual}) == len(actual) <= 12,
            "RUN_CAP_OR_DUPLICATE",
        )
        acknowledged = {
            r[key] for r in rows for key in ("run_id", "retry_run_id") if r.get(key)
        }
        need(
            acknowledged <= {r["run_id"] for r in actual}, "ACK_RUN_MISSING_FROM_LEDGER"
        )
        expected = {r["binding"]["business_request_key"]: r for r in plan["runs"]}
        authorized: dict[str, dict[str, Any]] = {}
        known_tokens = known_cost = held_tokens = held_cost = unknown_chat = 0
        for entry in actual:
            run = entry["run"]
            frozen = expected.get(entry["business_request_key"])
            need(frozen is not None, "UNPLANNED_BUSINESS_REQUEST")
            assert frozen is not None
            need(
                run["run_id"] == entry["run_id"]
                and run["tenant_id"] == frozen["binding"]["tenant_id"]
                and run["ticket_id"] == frozen["binding"]["ticket_id"]
                and run["profile_id"] == plan["profile"]["profile_id"]
                and run["profile_hash"] == plan["profile"]["profile_hash"],
                "LEDGER_RUN_BINDING",
            )
            source = next(r for r in rows if r["case_id"] == frozen["case_id"])
            parent = run["retry_of_run_id"]
            if parent is None:
                need(
                    source["run_id"] in {None, run["run_id"]}, "SUBMISSION_ACK_CHANGED"
                )
            else:
                need(
                    frozen["case_id"] in plan["retry_sources"]
                    and source.get("retry_status") == "attempted"
                    and parent == source["run_id"]
                    and source.get("retry_run_id") in {None, run["run_id"]},
                    "UNPLANNED_RETRY",
                )
            result["ledger_runs"].append(
                {
                    "case_id": frozen["case_id"],
                    "run_id": run["run_id"],
                    "retry_of_run_id": parent,
                }
            )
            authrow = entry["authorization"]
            if authrow is not None:
                decision = json.loads(
                    (output / frozen["case_id"] / "decision.json").read_bytes()
                )["approval"]
                need(
                    decision["status"] == "approved"
                    and decision["run_id"]
                    == authrow["authorizing_run_id"]
                    == source["run_id"]
                    and decision["proposal_hash"]
                    == authrow["action"]["authorization"]["proposal_hash"],
                    "UNAPPROVED_AUTHORIZATION",
                )
                authorized[authrow["operation_id"]] = authrow
            for call in entry["model_calls"]:
                need(
                    call["run_id"] == run["run_id"]
                    and call["profile_hash"] == run["profile_hash"],
                    "LEDGER_CALL_BINDING",
                )
                if call["status"] == "known":
                    known_tokens += call["known_tokens"]
                    known_cost += call["known_cost_microyuan"]
                else:
                    held_tokens += call["reserved_tokens"]
                    held_cost += call["reserved_cost_microyuan"]
                    unknown_chat += call["subcall"] == "chat"
        result["usage"] = {
            "known_tokens": known_tokens,
            "known_cost_microyuan": known_cost,
            "held_tokens": held_tokens,
            "held_cost_microyuan": held_cost,
            "unknown_chat_calls": unknown_chat,
        }
        account = control["batch_account"]
        need(
            all(
                account[key] == value
                for key, value in result["usage"].items()
                if key != "unknown_chat_calls"
            ),
            "FINAL_ACCOUNT_EXPOSURE",
        )
        need(
            account["used_cost_microyuan"] == known_cost + held_cost
            and account["limit_cost_microyuan"] <= plan["max_cost_microyuan"]
            and known_cost + held_cost <= plan["max_cost_microyuan"],
            "FINAL_COST_CAP",
        )
        result["usage"].update(
            batch_frozen=account["frozen"], batch_stop_code=account.get("stop_code")
        )
        need(unknown_chat == 0 and not account["frozen"], "MODEL_AUDIT_STOPPED")
        receipts = business["resolutions"]
        need(
            len({r["operation_id"] for r in receipts}) == len(receipts),
            "DUPLICATE_BATCH_EFFECT",
        )
        for record in receipts:
            authrow = authorized.get(record["operation_id"])
            need(authrow is not None, "UNAPPROVED_BATCH_EFFECT")
            assert authrow is not None
            need(
                record["authorization_hash"] == authrow["authorization_hash"]
                and record["parameters"] == authrow["action"]["parameters"],
                "BATCH_EFFECT_BINDING",
            )
        if traces is not None:
            need(
                all(
                    json.loads(path.read_bytes())["operation_id"] in authorized
                    for path in traces.glob("*.dispatch.json")
                ),
                "UNPLANNED_ACTION_SEND",
            )
        result["state"] = "passed"
    except (
        ValueError,
        KeyError,
        TypeError,
        IndexError,
        StopIteration,
        OSError,
    ) as error:
        result["errors"] = [evidence_error(error)]
    return result


def mechanism(
    row: dict[str, Any], archive: Path, plan: dict[str, Any], traces: Path
) -> dict[str, Any]:
    """Require SDK, actual business receipt and control/fault bindings to agree."""
    result: dict[str, Any] = {
        "state": "unexercised",
        "errors": [],
        "physical_writes": None,
        "physical_queries": None,
    }
    if row["status"] != "finished":
        return result
    try:
        pending, terminal, final = (
            json.loads((archive / (name + ".json")).read_bytes())
            for name in ("pending", "terminal", "final")
        )
        decision = json.loads((archive / "decision.json").read_bytes())
        audit = json.loads((archive / "control-audit.json").read_bytes())
        actual = next(
            value for value in audit["runs"] if value["run_id"] == row["run_id"]
        )
        authrow = actual["authorization"]
        need(
            terminal["run"] == final["run"] and terminal["result"] == final["result"],
            "TERMINAL_CHANGED",
        )
        need(
            call_facts(pending["calls"]) == call_facts(final["calls"]),
            "MODEL_CALLS_CHANGED_AFTER_APPROVAL",
        )
        need(
            final["steps"][: len(pending["steps"])] == pending["steps"],
            "ACCEPTED_PREFIX_CHANGED",
        )
        ActionCallsResponse.from_dict(final["action_calls"])
        EffectView.from_dict(final["effect"])
        proposal = pending["steps"][-1]["record"]["output"]["proposal"]
        need(
            decision["approval"]["proposal"] == proposal
            and pending["approval"]["proposal_hash"]
            == decision["approval"]["proposal_hash"]
            and decision["approval"]["run_id"] == row["run_id"]
            and decision["approval"]["actor_id"] == "s4-acceptance-operator",
            "APPROVAL_CHANGED",
        )
        calls = final["action_calls"]["items"]
        if row["intent"] == "reject_valid_proposal":
            need(
                authrow is None
                and not calls
                and final["run"]["state"] == "succeeded"
                and final["result"]["disposition"] == "rejected"
                and final["effect"]["state"] == "none",
                "REJECTION_WROTE",
            )
            result.update(state="passed", physical_writes=0, physical_queries=0)
            return result
        need(authrow is not None, "AUTHORIZATION_MISSING")
        action = authrow["action"]
        auth, parameters = action["authorization"], action["parameters"]
        expected = {"ticket_id": pending["run"]["ticket_id"], **proposal}
        need(parameters == expected, "APPROVED_PARAMETERS_CHANGED")
        need(
            auth["run_id"] == row["run_id"]
            and auth["tenant_id"] == pending["run"]["tenant_id"]
            and auth["snapshot_hash"] == pending["run"]["snapshot_hash"]
            and auth["version_vector"] == pending["run"]["version_vector"]
            and auth["key_id"] == plan["definition"]["action"]["key_id"]
            and auth["proposal_hash"] == decision["approval"]["proposal_hash"]
            and auth["approval_id"] == decision["approval"]["approval_id"],
            "AUTHORIZATION_BINDING",
        )
        need(
            contract_hash("jobforge.business.parameters.v1", parameters)
            == auth["parameters_hash"],
            "PARAMETERS_HASH",
        )
        need(
            authorization_hash(auth) == authrow["authorization_hash"],
            "AUTHORIZATION_HASH",
        )
        dispatches = [
            json.loads(path.read_bytes()) for path in traces.glob("*.dispatch.json")
        ]
        dispatches = [
            event
            for event in dispatches
            if event["operation_id"] == auth["operation_id"]
        ]
        writes = sum(event["method"] == "POST" for event in dispatches)
        queries = sum(event["method"] == "GET" for event in dispatches)
        result.update(physical_writes=writes, physical_queries=queries)
        need(
            writes <= sum(call["kind"] == "action_write" for call in calls) <= 4
            and sum(call["kind"] == "receipt_query" for call in calls) <= 4
            and len(calls) <= 8,
            "UNRESERVED_ACTION_SEND",
        )
        need(
            queries
            == sum(call["kind"] == "receipt_query" for call in calls)
            + len(actual["receipt_queries"]),
            "QUERY_TRACE_COVERAGE",
        )
        need(
            all(
                call["operation_id"] == auth["operation_id"]
                and call["authorization_hash"] == authrow["authorization_hash"]
                and call["provider_metering"] == "not_applicable"
                for call in calls
            ),
            "ACTION_CALL_BINDING",
        )
        business = json.loads((archive / "business-actions.json").read_bytes())
        receipts = [
            record
            for record in business["resolutions"]
            if record["operation_id"] == auth["operation_id"]
        ]
        if row["intent"] in {"ticket_revision_conflict", "authorization_cancel_retry"}:
            need(
                not receipts and final["effect"]["state"] == "unknown",
                "UNEXPECTED_BUSINESS_EFFECT",
            )
            if row["intent"] == "ticket_revision_conflict":
                need(
                    writes == 1
                    and final["run"]["state"] == "failed"
                    and final["run"]["error"]["code"] == "ACTION_CONFLICT",
                    "VERSION_CONFLICT_MISSING",
                )
                mutation = json.loads((archive / "loader-revision.json").read_bytes())
                need(
                    len(mutation["changed"]) == 1
                    and mutation["changed"][0]["revision"]
                    == auth["version_vector"]["ticket"]["revision"] + 1,
                    "ACTUAL_LOADER_CHANGE_MISSING",
                )
            else:
                need(
                    writes == 0
                    and queries == 1
                    and final["run"]["state"] == "cancelled",
                    "PREPOST_CANCEL_RESENT",
                )
                retry = json.loads((archive / "retry.json").read_bytes())
                need(
                    retry["run"]["state"] == "failed"
                    and retry["run"]["error"]["code"] == "ACTION_OUTCOME_UNKNOWN"
                    and not retry["calls"]["items"]
                    and not retry["action_calls"]["items"],
                    "UNKNOWN_RETRY_RESENT",
                )
        else:
            need(
                writes == 1
                and len(receipts) == 1
                and final["effect"]["state"] == "applied",
                "ATOMIC_EFFECT_MISSING_OR_REPEATED",
            )
            receipt = receipts[0]["receipt"]
            ActionReceipt.from_dict(receipt)
            need(
                final["effect"]["receipt"] == receipt
                and receipts[0]["parameters"] == parameters
                and receipt["authorization_hash"] == authrow["authorization_hash"]
                and receipt["proposal_hash"] == auth["proposal_hash"]
                and receipt["parameters_hash"] == auth["parameters_hash"]
                and receipt["approval_id"] == auth["approval_id"],
                "REAL_RECEIPT_BINDING",
            )
            need(
                receipt["tenant_id"] == auth["tenant_id"]
                and receipt["business_request_id"] == auth["business_request_id"]
                and receipt["ticket_id"] == parameters["ticket_id"]
                and receipt["before_revision"]
                == auth["version_vector"]["ticket"]["revision"]
                and receipt["ticket_status"] == parameters["target_ticket_status"]
                and receipt["after_revision"] == receipt["before_revision"] + 1
                and receipt["retain_until"] - receipt["applied_at"]
                >= 30 * 86400 * 1_000_000
                and auth["authorized_at"]
                <= receipt["applied_at"]
                < auth["authorization_expires_at"],
                "RECEIPT_FACTS_CHANGED",
            )
            need(
                contract_hash(
                    "jobforge.business.receipt.v1",
                    {
                        key: value
                        for key, value in receipt.items()
                        if key != "receipt_hash"
                    },
                )
                == receipt["receipt_hash"],
                "RECEIPT_HASH",
            )
            if row["intent"].startswith("commit_cancel"):
                need(
                    final["run"]["state"] == "cancelled"
                    and final["result"]["disposition"] == "unknown"
                    and terminal["effect"]["state"] == "unknown",
                    "CANCEL_REVIVED",
                )
                if row["intent"] == "commit_cancel_retry":
                    retry = json.loads((archive / "retry.json").read_bytes())
                    need(
                        retry["run"]["state"] == "succeeded"
                        and retry["result"]["disposition"] == "applied"
                        and not retry["calls"]["items"]
                        and not retry["action_calls"]["items"]
                        and retry["effect"]["receipt"] == receipt,
                        "APPLIED_RETRY_REEXECUTED",
                    )
            else:
                need(
                    final["run"]["state"] == "succeeded"
                    and final["result"]["disposition"] == "applied",
                    "ACTION_NOT_COMPLETED",
                )
        if row["intent"] in {
            "commit_loss_natural_recovery",
            "commit_cancel_reconcile",
            "commit_cancel_retry",
            "authorization_cancel_retry",
        }:
            need("fault" in row, "FAULT_PROOF_MISSING")
            barrier = json.loads((archive / "barrier.json").read_bytes())
            before = json.loads((archive / "pre-fault-run.json").read_bytes())
            need(
                before["run_id"] == row["run_id"]
                and before["state"] == "running"
                and before["profile_hash"] == pending["run"]["profile_hash"]
                and instant(barrier["observed_at"])
                <= instant(row["fault"]["worker_signal_sent_at"])
                < instant(before["lease_until"]),
                "LIVE_FAULT_BOUNDARY_MISSING",
            )
            if row["intent"] == "authorization_cancel_retry":
                need(
                    barrier["boundary"] == "authorization_saved"
                    and barrier["execution"]["run_id"] == row["run_id"]
                    and barrier["execution"]["attempt_no"] == before["attempt_no"]
                    and barrier["step"]["profile_hash"]
                    == pending["run"]["profile_hash"],
                    "ACTUAL_AUTHORIZATION_BOUNDARY_MISSING",
                )
            else:
                need(
                    barrier["boundary"] == "business_committed"
                    and barrier["run_id"] == row["run_id"]
                    and barrier["operation_id"] == auth["operation_id"]
                    and barrier["receipt"] == final["effect"]["receipt"],
                    "ACTUAL_COMMIT_BOUNDARY_MISSING",
                )
            need(
                row["fault"]["worker_returncode"] == -9
                and row["fault"]["child_group_at_boundary"] == "no_child"
                and instant(row["fault"]["worker_wait_completed_at"])
                >= instant(row["fault"]["worker_signal_sent_at"]),
                "PROCESS_DEATH_UNPROVEN",
            )
            if row["intent"] == "commit_loss_natural_recovery":
                old = next(
                    attempt
                    for attempt in actual["attempts"]
                    if attempt["attempt_no"] == before["attempt_no"]
                )
                need(
                    final["run"]["recovery_count"] == 1
                    and queries == 2
                    and instant(old["finished_at"]) >= instant(before["lease_until"])
                    and barrier["receipt"] == final["effect"]["receipt"],
                    "NATURAL_RECOVERY_UNPROVEN",
                )
        if row["intent"] == "ticket_revision_conflict":
            barrier = json.loads((archive / "barrier.json").read_bytes())
            need(
                barrier["boundary"] == "authorization_saved"
                and barrier["execution"]["run_id"] == row["run_id"],
                "ACTUAL_AUTHORIZATION_BOUNDARY_MISSING",
            )
        result["state"] = "passed"
    except (
        ValueError,
        KeyError,
        TypeError,
        IndexError,
        StopIteration,
        OSError,
    ) as error:
        result["state"] = "failed"
        result["errors"] = [
            str(error)
            if isinstance(error, ValueError)
            and str(error).isupper()
            and len(str(error)) <= 64
            else "INCOMPLETE_MECHANISM_EVIDENCE"
        ]
    return result


def report(plan: dict[str, Any], output: Path, traces: Path) -> dict[str, Any]:
    """Keep all ten source rows and both planned retry opportunities in the denominator."""
    validate(plan, check_sources=False)
    need(
        load_package().hashes == plan["package_sha256"],
        "FROZEN_BUSINESS_PACKAGE_CHANGED",
    )
    rows = json.loads((output / "rows.json").read_bytes())["runs"]
    need(
        [(row["case_id"], row["intent"]) for row in rows]
        == [(row["case_id"], row["intent"]) for row in plan["runs"]],
        "EXECUTION_LIST_CHANGED",
    )
    results = []
    for row, frozen in zip(rows, plan["runs"], strict=True):
        archive = output / row["case_id"]
        quality = checked_quality(row, frozen, archive, plan["profile"])
        measured = mechanism(row, archive, plan, traces)
        if row["status"] == "finished" and not (
            quality and quality["approval_eligible"]
        ):
            measured["state"] = "failed"
            measured["errors"].append("QUALITY_APPROVAL_INELIGIBLE")
        results.append(
            {
                "case_id": row["case_id"],
                "run_id": row["run_id"],
                "intent": row["intent"],
                "status": row["status"],
                "error_code": row["error_code"],
                "quality": quality,
                "mechanism": measured,
            }
        )
    batch = batch_audit(plan, rows, output, traces)
    return {
        "schema_version": 1,
        "scorer_version": "support-approval-mechanisms-v1",
        "source_denominator": 10,
        "planned_new_runs": 12,
        "actual_new_runs": batch["actual_new_runs"],
        "mechanism_counts": dict(Counter(row["mechanism"]["state"] for row in results)),
        "batch_effect_audit": batch["state"],
        "batch_errors": batch["errors"],
        "final_ledger": batch,
        "quality_eligible": sum(
            bool(row["quality"] and row["quality"]["approval_eligible"])
            for row in results
        ),
        "cost_basis": "observed_usage_tariff_estimate_not_settled_invoice",
        "cases": results,
    }


def main() -> None:
    """Write a new report outside existing raw evidence; no network or mutation."""
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("plan", "archive", "traces", "out"):
        parser.add_argument("--" + name, type=Path, required=True)
    args = parser.parse_args()
    result = report(json.loads(args.plan.read_bytes()), args.archive, args.traces)
    with args.out.open("xb") as target:
        target.write(json_bytes(result))


if __name__ == "__main__":
    main()
