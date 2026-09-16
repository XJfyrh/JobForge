"""Synthetic unit-test evidence builders; never an actual Run exporter or score.

These functions are imported only by tests. They construct declared synthetic
traces to exercise provenance validation. No gold value is copied into a model
result, and passing these fixtures never establishes actual execution.
"""

from __future__ import annotations

import copy
import json
from pathlib import Path
from typing import Any

from jobforge_agent.provider_audit import capture_chat_report
from jobforge_agent.runtime_input import input_hash
from jobforge_agent.support_contract import proposal_from_model

from tools.support_evaluation.evidence import Package, canonical, fingerprint
from tools.support_evaluation.predicates import Facts, derive_facts
from tools.support_evaluation.validate_data import DATASET_VERSION, POLICY_VERSION

REPO = Path(__file__).resolve().parents[2]
TIME = "2026-09-16T12:02:00Z"


def uid(number: int) -> str:
    """Return a deterministic fixture UUID with no production significance."""
    return f"00000000-0000-4000-8000-{number:012d}"


def registered(package: Package) -> dict[str, Any]:
    """Build an explicitly synthetic fixed registration without reading gold."""
    return {
        "schema_version": 1,
        "scorer_version": "support-offline-v1",
        "evidence_origin": "synthetic_test",
        "dataset_version": DATASET_VERSION,
        "policy_version": POLICY_VERSION,
        "gold_sha256": package.hashes["evaluation/dev_gold.jsonl"],
        "scoring_sha256": package.hashes["evaluation/scoring-proposal.json"],
        "anchors_sha256": package.hashes["evaluation/semantic-anchors.json"],
        "corpus_sha256": package.corpus_hash,
        "profile": {
            "profile_id": "synthetic-scoring-profile",
            "profile_hash": "a" * 64,
            "price_hash": "b" * 64,
            "strategy": "support_fixed_v1",
            "proposal_schema": "support-proposal-v1",
            "executor_version": "linux-v2-audit-runtime-1",
            "expected_response_model": "deepseek-flash",
            "provider_audit_policy": "deepseek-audit-v1",
            "budget_batch_id": "synthetic-batch",
            "pricing": {
                "denominator": 1,
                "input_miss_microyuan": 2,
                "input_hit_microyuan": 1,
                "output_microyuan": 8,
            },
            "max_input_tokens": 1048576,
            "max_output_tokens": 1024,
            "origins": {
                "business": "http://business.example",
                "ollama": "http://ollama.example",
                "deepseek": "https://deepseek.example",
            },
        },
        "bindings": [
            {
                "case_id": case,
                "tenant_id": row["tenant_id"],
                "ticket_id": row["ticket_id"],
                "business_request_key": "synthetic-intent-" + case,
                "as_of": package.tickets[(row["tenant_id"], row["ticket_id"])][
                    "observed_at"
                ],
                "index_id": uid(80 if row["tenant_id"] == "tenant-north" else 81),
                "index_profile_hash": "c" * 64,
                "index_content_hash": "d" * 64,
                "budget_limits": {
                    scope: {
                        "chat": 100,
                        "logical_tools": 160,
                        "query_embedding": 100,
                        "profile_metadata_http": 200,
                        "business_tool_http": 200,
                        "physical_http": 500,
                        "protocol_corrections": 40,
                        "tokens": 10000000,
                        "cost_microyuan": 5000000,
                    }
                    for scope in ("family", "tenant", "batch")
                },
            }
            for index, (case, row) in enumerate(package.cases.items())
        ],
    }


def model_claims(facts: Facts) -> list[dict[str, Any]]:
    """Make source-derived test assertions, explicitly outside real acceptance."""
    claims: list[dict[str, Any]] = []
    for kind in sorted(facts.required):
        if kind == "conflict":
            variant, ids = sorted(facts.conflicts, key=lambda item: item[0])[0]
            refs = ["P05.1"]
            if variant == "order_delivery":
                refs += ["E1#/order/status", "E2#/delivery/status"]
            else:
                refs += ["E2#/delivery/events"]
            claims.append(
                {"kind": kind, "type": variant, "event_ids": sorted(ids), "refs": refs}
            )
        elif kind == "dispute":
            event = next(
                key
                for key, value in facts.events.items()
                if value["status"] == "delivered"
            )
            claims.append(
                {
                    "kind": kind,
                    "type": sorted(facts.dispute_types)[0],
                    "delivered_event_id": event,
                    "refs": ["T#/description", "P04.1"],
                }
            )
        elif kind == "critical":
            event = sorted(facts.critical_ids)[0]
            claims.append(
                {
                    "kind": kind,
                    "status": facts.events[event]["status"],
                    "event_id": event,
                    "refs": ["E2#/delivery/status", "P06.1"],
                }
            )
        elif kind == "correction":
            recovery, corrected = sorted(facts.corrections)[0]
            claims.append(
                {
                    "kind": kind,
                    "recovery_event_id": recovery,
                    "corrected_event_id": corrected,
                    "refs": ["P06.2"],
                }
            )
        elif kind == "timing":
            refs = [
                "E1#/order/promised_delivery_at",
                "P07.1" if facts.timing == "outstanding_overdue_ge48" else "P02.1",
            ]
            if facts.timing and facts.timing.startswith("outstanding"):
                refs += ["T#/observed_at", "E2#/delivery/events"]
            claims.append(
                {
                    "kind": kind,
                    "test": facts.timing,
                    "event_id": sorted(facts.timing_ids)[0],
                    "refs": refs,
                }
            )
        elif kind.startswith("missing:"):
            field = kind.split(":", 1)[1]
            refs = {
                "ticket.order_id": ["T#/order_id"],
                "order.delivery_id": ["E1#/order/delivery_id"],
                "delivery.usable_tracking_events": ["E2#/delivery/events"],
                "delivery.delivered_event": [
                    "E2#/delivery/events",
                    "E2#/delivery/status",
                ],
                "ticket.problem_description": ["T#/description"],
            }[field]
            claims.append({"kind": "missing", "field": field, "refs": refs + ["P03.1"]})
        else:
            mode = kind.split(":", 1)[1]
            claims.append(
                {
                    "kind": "ticket_status",
                    "mode": mode,
                    "refs": [
                        "T#/status",
                        "P08.1" if mode == "informational_no_action" else "P08.2",
                    ],
                }
            )
    return claims


def case_row(
    package: Package,
    registration: dict[str, Any],
    index: int,
    *,
    model: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """Construct a synthetic API/hash-chain trace for one policy predicate case."""
    binding, profile = dict(registration["bindings"][index]), registration["profile"]
    binding.update(snapshot_id=uid(index + 100), snapshot_hash=f"{index + 100:064x}")
    tenant = binding["tenant_id"]
    ticket = copy.deepcopy(package.tickets[(tenant, binding["ticket_id"])])
    order = copy.deepcopy(package.orders.get((tenant, ticket["order_id"])))
    delivery = (
        copy.deepcopy(package.deliveries.get((tenant, order["delivery_id"])))
        if order
        else None
    )
    facts = derive_facts(ticket, order, delivery, package.anchors)
    model = (
        copy.deepcopy(model)
        if model is not None
        else {**facts.expected, "claims": model_claims(facts)}
    )
    run = copy.deepcopy(
        json.loads((REPO / "api/run/v2/fixtures.json").read_text())["run"]
    )
    run.update(
        run_id=uid(index + 200),
        tenant_id=tenant,
        ticket_id=ticket["ticket_id"],
        profile_id=profile["profile_id"],
        profile_hash=profile["profile_hash"],
        budget_batch_id=profile["budget_batch_id"],
        business_request_key=binding["business_request_key"],
        snapshot_id=binding["snapshot_id"],
        snapshot_hash=binding["snapshot_hash"],
        state="awaiting_approval" if model["decision"] == "proposal" else "succeeded",
        outcome=None if model["decision"] == "proposal" else "no_action",
        error=None,
        attempt_no=1,
    )
    vector = {
        "schema_version": 1,
        "ticket": {"id": ticket["ticket_id"], "revision": ticket["revision"]},
        "order": {
            "id": ticket["order_id"],
            "exists": order is not None,
            "revision": order["revision"] if order else None,
        },
        "delivery": {
            "id": order["delivery_id"] if order else None,
            "exists": delivery is not None,
            "aggregate_revision": delivery["aggregate_revision"] if delivery else None,
        },
        "policy": {
            "version": POLICY_VERSION,
            "revision": 2,
            "corpus_sha256": package.corpus_hash,
        },
        "index": {
            "id": binding["index_id"],
            "profile_hash": binding["index_profile_hash"],
            "content_hash": binding["index_content_hash"],
        },
    }
    run["version_vector"] = vector
    snapshot = {
        "tenant_id": tenant,
        "ticket_id": ticket["ticket_id"],
        "snapshot_id": binding["snapshot_id"],
        "snapshot_hash": binding["snapshot_hash"],
        "version_vector_json": vector,
        "ticket_binding_json": ticket,
        "index_id": binding["index_id"],
        "index_profile_hash": binding["index_profile_hash"],
    }
    checkpoint: dict[str, Any] = {"snapshot": snapshot, "steps": []}
    row: dict[str, Any] = {
        "case_id": binding["case_id"],
        "status": "run",
        "error_code": "",
        "run": run,
        "steps": [],
        "result": None,
        "calls": {
            "run_id": run["run_id"],
            "captured_at": TIME,
            "batch_frozen": False,
            "batch_stop_code": None,
            "items": [],
        },
        "safety": {
            "complete": True,
            "trace_sha256": "e" * 64,
            "business_audit_sha256": "f" * 64,
            "requests": [],
            "writes": [],
        },
    }
    base_call = json.loads((REPO / "api/run/v2/calls-fixtures.json").read_text())[
        "free"
    ]["items"][0]
    prior = ""
    kinds = (
        ["read_ticket", "get_order"]
        + (["get_delivery"] if order else [])
        + ["search_policy", "model_proposal", "submit_proposal"]
    )
    for sequence, kind in enumerate(kinds, 1):
        step_id = uid(10000 + index * 100 + sequence)
        output: dict[str, Any] = {
            "schema_version": 1,
            "tool_invocation_id": "",
            "physical_call_id": "",
            "evidence_refs": [],
            "content": None,
            "proposal": None,
            "correction_required": False,
        }
        subcalls = (
            [kind]
            if kind in {"get_order", "get_delivery"}
            else ["profile_version", "profile_tags", "query_embedding", "search_policy"]
            if kind == "search_policy"
            else ["chat"]
            if kind == "model_proposal"
            else []
        )
        for subcall in subcalls:
            ordinal = len(row["calls"]["items"]) + 1
            physical = uid(20000 + index * 100 + ordinal)
            call = copy.deepcopy(base_call)
            call.update(
                physical_call_id=physical,
                step_id=step_id,
                step_kind=kind,
                ordinal=ordinal,
                subcall=subcall,
                profile_id=profile["profile_id"],
                profile_hash=profile["profile_hash"],
                price_hash=profile["price_hash"],
                observed_at="2026-09-16T12:00:30Z",
                transport_outcome="response",
                http_status=200,
                error_code="",
                business_outcome="accepted",
            )
            if subcall == "chat":
                call["reserved"] = {
                    "input_tokens": 1048576,
                    "output_tokens": 1024,
                    "total_tokens": 1049600,
                    "cost_microyuan": 2105344,
                }
                body = {
                    "id": "fixture",
                    "object": "chat.completion",
                    "created": 1,
                    "model": "deepseek-flash",
                    "system_fingerprint": "fixture",
                    "choices": [
                        {
                            "index": 0,
                            "finish_reason": "stop",
                            "message": {
                                "role": "assistant",
                                "content": json.dumps(model),
                            },
                        }
                    ],
                    "usage": {
                        "prompt_tokens": 10,
                        "completion_tokens": 5,
                        "total_tokens": 15,
                        "prompt_cache_hit_tokens": 0,
                        "prompt_cache_miss_tokens": 10,
                    },
                }
                report = capture_chat_report(
                    json.dumps(body).encode(),
                    http_status=200,
                    physical_call_id=physical,
                    expected_response_model="deepseek-flash",
                ).to_dict()
                call.update(
                    provider_audit=report["provider_audit"],
                    audit_hash=report["provider_audit"]["audit_hash"],
                    observed_usage=report["usage"],
                    settled_usage=report["usage"],
                    report_hash="1" * 64,
                    usage_known=True,
                    known_tokens=15,
                    known_cost_microyuan=60,
                    held_tokens=0,
                    held_cost_microyuan=0,
                    report_recorded_at="2026-09-16T12:00:30Z",
                    settled_at="2026-09-16T12:00:30Z",
                    audit_status="recorded",
                )
            elif subcall == "query_embedding":
                call["reserved"] = {
                    "input_tokens": 512,
                    "output_tokens": 0,
                    "total_tokens": 512,
                    "cost_microyuan": 0,
                }
                call["held_tokens"] = 512
            if subcall != "chat":
                call["audit_status"] = "not_applicable"
            row["calls"]["items"].append(call)
            endpoint = (
                "deepseek"
                if subcall == "chat"
                else "ollama"
                if subcall in {"profile_version", "profile_tags", "query_embedding"}
                else "business"
            )
            row["safety"]["requests"].append(
                {
                    "physical_call_id": physical,
                    "tenant_id": tenant,
                    "snapshot_id": binding["snapshot_id"],
                    "profile_hash": profile["profile_hash"],
                    "subcall": subcall,
                    "method": "POST"
                    if subcall in {"chat", "query_embedding", "search_policy"}
                    else "GET",
                    "endpoint_alias": endpoint,
                    "origin": profile["origins"][endpoint],
                    "resource_id": ticket["order_id"]
                    if subcall in {"get_order", "get_delivery"}
                    else binding["index_id"]
                    if subcall == "search_policy"
                    else "deepseek-flash"
                    if subcall == "chat"
                    else "all-minilm:22m",
                    "model": "deepseek-flash" if subcall == "chat" else None,
                    "thinking": "disabled" if subcall == "chat" else None,
                    "max_tokens": 1024 if subcall == "chat" else None,
                }
            )
            output["physical_call_id"] = physical
        if kind in {"get_order", "get_delivery", "search_policy"}:
            output["tool_invocation_id"] = uid(30000 + index * 100 + sequence)
        if kind == "read_ticket":
            output.update(
                content=ticket,
                evidence_refs=[f"business-evidence:{binding['snapshot_id']}:ticket"],
            )
        elif kind in {"get_order", "get_delivery"}:
            name = kind.removeprefix("get_")
            fact = order if name == "order" else delivery
            ref = f"business-evidence:{binding['snapshot_id']}:{name}"
            content = {
                "snapshot_id": binding["snapshot_id"],
                "evidence_ref": ref,
                "kind": name,
                "missing": fact is None,
            }
            if fact is None:
                content["missing_reason"] = "not_associated"
            else:
                content[name] = fact
            output.update(content=content, evidence_refs=[ref])
        elif kind == "search_policy":
            aliases = sorted(
                {
                    ref
                    for claim in model["claims"]
                    for ref in claim["refs"]
                    if ref.startswith("P")
                }
            )
            hits = [
                {
                    "index_id": binding["index_id"],
                    "chunk_id": alias,
                    "policy_version": POLICY_VERSION,
                    "evidence_ref": f"business-policy:{binding['index_id']}:{alias}",
                    "source": alias.split(".")[0] + ".md",
                    "text": package.paragraphs[alias],
                    "distance": 0.1,
                }
                for alias in aliases
            ]
            output.update(
                content={"snapshot_id": binding["snapshot_id"], "matches": hits},
                evidence_refs=[hit["evidence_ref"] for hit in hits],
            )
        elif kind == "model_proposal":
            output["proposal"] = proposal_from_model(model, checkpoint)
        else:
            output["proposal"] = checkpoint["steps"][-1]["result_json"]["proposal"]
        raw = json.dumps(output, ensure_ascii=False, separators=(",", ":"))
        ih = input_hash(
            profile["profile_hash"], binding["snapshot_hash"], sequence - 1, prior
        )
        prior = fingerprint(
            "jobforge.run.commit.v1",
            step_id,
            str(sequence),
            kind,
            str(sequence - 1),
            ih,
            profile["profile_id"],
            profile["profile_hash"],
            binding["snapshot_id"],
            binding["snapshot_hash"],
            canonical(raw),
        )
        record = {
            "step_id": step_id,
            "sequence": sequence,
            "kind": kind,
            "input_hash": ih,
            "profile_hash": profile["profile_hash"],
            "snapshot_hash": binding["snapshot_hash"],
            "commit_hash": prior,
            "output_ref": f"run-step:{run['run_id']}:{sequence}",
            "output": output,
            "cursor_version": sequence,
            "created_at": TIME,
        }
        row["steps"].append({"record": record, "output_json": raw})
        checkpoint["steps"].append({"step": {"kind": kind}, "result_json": output})
    run["cursor_version"] = len(kinds)
    run["proposal_ref"] = (
        "run-proposal:" + run["run_id"] if model["decision"] == "proposal" else None
    )
    row["result"] = {
        "available": True,
        "kind": "proposal" if model["decision"] == "proposal" else "no_action",
        "ref": run["proposal_ref"] or row["steps"][-1]["record"]["output_ref"],
    }
    usage = run["budget"]["run_usage"]
    usage.update(
        physical_http=len(row["calls"]["items"]),
        chat=1,
        protocol_corrections=0,
        tokens=527,
        cost_microyuan=60,
        query_embedding=1,
        profile_metadata_http=2,
        business_tool_http=len(row["calls"]["items"]) - 4,
    )
    for scope in ("family", "tenant", "batch"):
        run["budget"][scope].update(
            limits=copy.deepcopy(binding["budget_limits"][scope]),
            used=copy.deepcopy(usage),
            known_tokens=15,
            known_cost_microyuan=60,
            held_tokens=512,
            held_cost_microyuan=0,
        )
    return row


def unattempted(case_id: str) -> dict[str, Any]:
    """Keep an explicit denominator row with no fabricated execution evidence."""
    return {
        "case_id": case_id,
        "status": "unattempted",
        "error_code": "",
        "run": None,
        "steps": [],
        "result": None,
        "calls": None,
        "safety": None,
    }
