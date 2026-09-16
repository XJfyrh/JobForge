"""Bounded offline export validation and accepted-step provenance checks."""

from __future__ import annotations

import hashlib
import json
import math
import re
from dataclasses import dataclass
from decimal import Decimal
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit

from jobforge.run_calls import RunCalls
from jobforge.run_models import Run, RunResult, RunStep, RunUsage
from jobforge_agent.runtime_input import input_hash, validate_step_result
from jobforge_agent.support_adapter import validate_support_step
from jobforge_agent.support_agent import validate_agent_step
from jobforge_agent.support_contract import support_sources, validate_persisted_proposal

from tools.support_evaluation.validate_data import (
    DATASET_VERSION,
    POLICY_FILES,
    POLICY_VERSION,
    ROOT,
    instant,
    parse_json,
    read_artifact,
    sha256,
    validate_package,
)

SCORER_VERSION = "support-offline-v1"
MAX_REGISTRATION = 64 * 1024
MAX_EVIDENCE = 24 * 1024 * 1024
MAX_CASE = 512 * 1024
HASH = re.compile(r"[0-9a-f]{64}\Z")
UUID = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\Z")
IDENTIFIER = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:-]{0,127}\Z")


class EvidenceError(ValueError):
    """A fixed diagnostic code; no source text is copied into reports."""


def need(condition: bool, code: str) -> None:
    """Fail closed with a bounded fixed code."""
    if not condition:
        raise EvidenceError(code)


def fields(value: Any, names: set[str], code: str = "EXPORT_SHAPE") -> None:
    """Enforce an exact object field set without implicit nullable defaults."""
    need(type(value) is dict and set(value) == names, code)


def digest(value: Any) -> bool:
    """Recognize a canonical SHA256 identifier."""
    return type(value) is str and HASH.fullmatch(value) is not None


def bounded(value: Any, maximum: int) -> None:
    """Reject overlong or deeply nested protected JSON, never truncate it."""

    def walk(item: Any, depth: int) -> None:
        need(depth <= 64, "JSON_DEPTH")
        if type(item) is dict:
            for key, child in item.items():
                need(type(key) is str and "\x00" not in key, "JSON_VALUE")
                walk(child, depth + 1)
        elif type(item) is list:
            for child in item:
                walk(child, depth + 1)
        elif type(item) is str:
            need("\x00" not in item, "JSON_VALUE")
        elif type(item) is float:
            need(math.isfinite(item), "JSON_VALUE")

    walk(value, 0)
    need(
        len(json.dumps(value, ensure_ascii=False, allow_nan=False).encode()) <= maximum,
        "SIZE_LIMIT",
    )


def read_json(path: Path, maximum: int) -> tuple[dict[str, Any], str]:
    """Read only the explicitly supplied local file with a fixed byte ceiling."""
    with path.open("rb") as source:
        raw = source.read(maximum + 1)
    need(len(raw) <= maximum, "SIZE_LIMIT")
    value = parse_json(raw)
    bounded(value, maximum)
    need(type(value) is dict, "EXPORT_SHAPE")
    return value, sha256(raw)


def canonical(raw: str) -> str:
    """Match Go CanonicalCheckpointJSON's exact decimal and string encoding."""
    # Parse once through the common strict reader for duplicates/UTF-8 and once
    # with Decimal so an original accepted number never goes through float.
    need(type(raw) is str and len(raw.encode()) <= 16384, "STEP_SIZE")
    bounded(parse_json(raw.encode()), 16384)

    def number(text: str) -> Decimal:
        parts = re.split("[eE]", text)
        need(len(parts) == 1 or -308 <= int(parts[1]) <= 308, "JSON_NUMBER")
        value = Decimal(text)
        need(value.is_finite() and math.isfinite(float(value)), "JSON_NUMBER")
        return value

    value = json.loads(raw, parse_float=number, parse_int=number)

    def encode(item: Any) -> str:
        if isinstance(item, Decimal):
            need(item.is_finite() and math.isfinite(float(item)), "JSON_NUMBER")
            text = format(item, "f")
            if "." in text:
                text = text.rstrip("0").rstrip(".")
            return "0" if item == 0 else text
        if type(item) is dict:
            return (
                "{"
                + ",".join(
                    encode(key) + ":" + encode(item[key]) for key in sorted(item)
                )
                + "}"
            )
        if type(item) is list:
            return "[" + ",".join(encode(child) for child in item) + "]"
        text = json.dumps(item, ensure_ascii=False, separators=(",", ":"))
        return (
            text.replace("<", "\\u003c")
            .replace(">", "\\u003e")
            .replace("&", "\\u0026")
            .replace("\u2028", "\\u2028")
            .replace("\u2029", "\\u2029")
        )

    return encode(value)


def fingerprint(*values: str) -> str:
    """Use the existing length-prefixed Run fingerprint, not Python hash()."""
    result = hashlib.sha256()
    for value in values:
        raw = value.encode()
        result.update(len(raw).to_bytes(8, "big"))
        result.update(raw)
    return result.hexdigest()


@dataclass(frozen=True)
class Package:
    """Validated immutable data and source annotations; gold is comparison only."""

    hashes: dict[str, str]
    corpus_hash: str
    cases: dict[str, dict[str, Any]]
    gold: dict[str, dict[str, Any]]
    tickets: dict[tuple[str, str], dict[str, Any]]
    orders: dict[tuple[str, str], dict[str, Any]]
    deliveries: dict[tuple[str, str], dict[str, Any]]
    anchors: list[dict[str, Any]]
    paragraphs: dict[str, str]


def load_package(root: Path = ROOT) -> Package:
    """Verify the reviewer manifest before exposing any scoring data."""
    validation = validate_package(root)
    seed = parse_json(read_artifact(root, "runtime/seed.json"))

    def rows(path: str) -> dict[str, dict[str, Any]]:
        values = [parse_json(line) for line in read_artifact(root, path).splitlines()]
        return {row["case_id"]: row for row in values}

    paragraphs = {}
    for name in POLICY_FILES:
        parts = re.split(
            r"<!-- paragraph_id: (P[0-9]{2}\.[12]) -->",
            read_artifact(root, "runtime/policies/" + name).decode(),
        )
        paragraphs.update(
            {
                key: text.strip()
                for key, text in zip(parts[1::2], parts[2::2], strict=True)
            }
        )
    return Package(
        validation["read_hashes"],
        validation["corpus_sha256"],
        rows("evaluation/case-map.jsonl"),
        rows("evaluation/dev_gold.jsonl"),
        {(r["tenant_id"], r["ticket_id"]): r for r in seed["tickets"]},
        {(r["tenant_id"], r["order_id"]): r for r in seed["orders"]},
        {(r["tenant_id"], r["delivery_id"]): r for r in seed["deliveries"]},
        parse_json(read_artifact(root, "evaluation/semantic-anchors.json"))["anchors"],
        paragraphs,
    )


def registration(value: dict[str, Any], package: Package) -> dict[str, dict[str, Any]]:
    """Check all predeclared identities, versions and fixed profile capabilities."""
    fields(
        value,
        {
            "schema_version",
            "scorer_version",
            "evidence_origin",
            "dataset_version",
            "policy_version",
            "gold_sha256",
            "scoring_sha256",
            "anchors_sha256",
            "corpus_sha256",
            "profile",
            "bindings",
        },
    )
    need(
        type(value["schema_version"]) is int
        and value["schema_version"] == 1
        and value["scorer_version"] == SCORER_VERSION,
        "SCORER_VERSION",
    )
    need(
        value["evidence_origin"] in {"run_api_export", "synthetic_test"},
        "EVIDENCE_ORIGIN",
    )
    need(
        value["dataset_version"] == DATASET_VERSION
        and value["policy_version"] == POLICY_VERSION
        and value["corpus_sha256"] == package.corpus_hash,
        "DATA_VERSION",
    )
    for key, path in (
        ("gold_sha256", "evaluation/dev_gold.jsonl"),
        ("scoring_sha256", "evaluation/scoring-proposal.json"),
        ("anchors_sha256", "evaluation/semantic-anchors.json"),
    ):
        need(value[key] == package.hashes[path], "DATA_HASH")
    profile = value["profile"]
    fields(
        profile,
        {
            "profile_id",
            "profile_hash",
            "strategy",
            "proposal_schema",
            "executor_version",
            "expected_response_model",
            "provider_audit_policy",
            "price_hash",
            "budget_batch_id",
            "pricing",
            "max_input_tokens",
            "max_output_tokens",
            "origins",
        },
    )
    for key in ("profile_id", "budget_batch_id"):
        need(
            type(profile[key]) is str
            and IDENTIFIER.fullmatch(profile[key]) is not None,
            "PROFILE_BINDING",
        )
    need(
        digest(profile["profile_hash"]) and digest(profile["price_hash"]),
        "PROFILE_BINDING",
    )
    pricing = profile["pricing"]
    fields(
        pricing,
        {
            "denominator",
            "input_miss_microyuan",
            "input_hit_microyuan",
            "output_microyuan",
        },
    )
    need(
        all(type(v) is int and 0 <= v <= 9007199254740991 for v in pricing.values())
        and pricing["denominator"] > 0,
        "PRICE_BINDING",
    )
    need(
        type(profile["max_input_tokens"]) is int
        and 1 <= profile["max_input_tokens"] <= 9007199254739967
        and type(profile["max_output_tokens"]) is int
        and profile["max_output_tokens"] == 1024,
        "PROFILE_BOUNDS",
    )
    fields(profile["origins"], {"business", "ollama", "deepseek"})
    for origin in profile["origins"].values():
        need(type(origin) is str and len(origin) <= 256, "PROFILE_ORIGIN")
        parts = urlsplit(origin)
        need(
            parts.scheme in {"http", "https"}
            and bool(parts.hostname)
            and parts.username is None
            and parts.password is None
            and not parts.path
            and not parts.query
            and not parts.fragment,
            "PROFILE_ORIGIN",
        )
    need(
        (
            profile["strategy"],
            profile["proposal_schema"],
            profile["executor_version"],
            profile["expected_response_model"],
            profile["provider_audit_policy"],
        )
        in {
            (
                "support_fixed_v1",
                "support-proposal-v1",
                "linux-v2-audit-runtime-1",
                "deepseek-flash",
                "deepseek-audit-v1",
            ),
            (
                "support_agent_v1",
                "support-proposal-v1",
                "linux-v2-agent-runtime-1",
                "deepseek-flash",
                "deepseek-audit-v1",
            ),
        },
        "PROFILE_CAPABILITIES",
    )
    need(
        type(value["bindings"]) is list and len(value["bindings"]) == 40,
        "CASE_COVERAGE",
    )
    result: dict[str, dict[str, Any]] = {}
    intents: set[tuple[str, str]] = set()
    for binding in value["bindings"]:
        fields(
            binding,
            {
                "case_id",
                "tenant_id",
                "ticket_id",
                "business_request_key",
                "as_of",
                "index_id",
                "index_profile_hash",
                "index_content_hash",
                "budget_limits",
            },
        )
        case = package.cases.get(binding["case_id"])
        need(case is not None and binding["case_id"] not in result, "CASE_COVERAGE")
        assert case is not None
        need(
            all(binding[k] == case[k] for k in ("tenant_id", "ticket_id")),
            "CASE_IDENTITY",
        )
        for key in ("index_id",):
            need(
                type(binding[key]) is str and UUID.fullmatch(binding[key]) is not None,
                "SOURCE_BINDING",
            )
        for key in ("index_profile_hash", "index_content_hash"):
            need(digest(binding[key]), "SOURCE_BINDING")
        intent = binding["business_request_key"]
        need(
            type(intent) is str and IDENTIFIER.fullmatch(intent) is not None,
            "INTENT_BINDING",
        )
        need((binding["tenant_id"], intent) not in intents, "INTENT_BINDING")
        intents.add((binding["tenant_id"], intent))
        ticket = package.tickets[(binding["tenant_id"], binding["ticket_id"])]
        need(
            instant(binding["as_of"]) == instant(ticket["observed_at"]),
            "OBSERVATION_BINDING",
        )
        fields(binding["budget_limits"], {"family", "tenant", "batch"})
        for limits in binding["budget_limits"].values():
            RunUsage.from_dict(limits)
        result[binding["case_id"]] = binding
    need(set(result) == set(package.cases), "CASE_COVERAGE")
    return result


def same_source(actual: Any, expected: Any) -> bool:
    """Compare actual versioned facts, allowing only equivalent timestamp offsets."""
    if type(actual) is dict and type(expected) is dict:
        return set(actual) == set(expected) and all(
            same_source(actual[k], expected[k]) for k in actual
        )
    if type(actual) is list and type(expected) is list:
        return len(actual) == len(expected) and all(
            same_source(a, b) for a, b in zip(actual, expected, strict=True)
        )
    if (
        type(actual) is str
        and type(expected) is str
        and "T" in actual
        and "T" in expected
    ):
        try:
            return instant(actual) == instant(expected)
        except ValueError:
            pass
    return type(actual) is type(expected) and actual == expected


def validate_run_sources(
    row: dict[str, Any],
    binding: dict[str, Any],
    profile: dict[str, Any],
    package: Package,
) -> tuple[dict[str, Any], dict[str, Any]]:
    """Validate raw public API projections and reconstruct the accepted checkpoint."""
    run = row["run"]
    Run.from_dict(run)
    RunResult.from_dict(row["result"])
    RunCalls.from_dict(row["calls"])
    for key in ("tenant_id", "ticket_id", "business_request_key"):
        need(run[key] == binding[key], "RUN_BINDING")
    for key in ("profile_id", "profile_hash", "budget_batch_id"):
        need(run[key] == profile[key], "PROFILE_BINDING")
    need(row["calls"]["run_id"] == run["run_id"], "CALL_RUN_BINDING")
    vector = run["version_vector"]
    ticket = package.tickets[(binding["tenant_id"], binding["ticket_id"])]
    need(
        vector["ticket"] == {"id": ticket["ticket_id"], "revision": ticket["revision"]},
        "TICKET_VERSION",
    )
    need(
        vector["policy"]
        == {
            "version": POLICY_VERSION,
            "revision": 2,
            "corpus_sha256": package.corpus_hash,
        },
        "POLICY_VERSION",
    )
    need(
        vector["index"]
        == {
            "id": binding["index_id"],
            "profile_hash": binding["index_profile_hash"],
            "content_hash": binding["index_content_hash"],
        },
        "INDEX_BINDING",
    )
    need(
        type(row["steps"]) is list
        and len(row["steps"]) <= 32
        and len(row["steps"]) == run["cursor_version"],
        "STEP_COVERAGE",
    )
    checkpoint: dict[str, Any] = {
        "snapshot": {
            "tenant_id": run["tenant_id"],
            "ticket_id": run["ticket_id"],
            "snapshot_id": run["snapshot_id"],
            "snapshot_hash": run["snapshot_hash"],
            "version_vector_json": vector,
            "ticket_binding_json": ticket,
            "index_id": binding["index_id"],
            "index_profile_hash": binding["index_profile_hash"],
        },
        "steps": [],
    }
    prior, ids = "", set()
    for sequence, entry in enumerate(row["steps"], 1):
        fields(entry, {"record", "output_json"}, "STEP_SHAPE")
        record, raw = entry["record"], entry["output_json"]
        RunStep.from_dict(record)
        need(type(raw) is str and len(raw.encode()) <= 16384, "STEP_SIZE")
        need(parse_json(raw.encode()) == record["output"], "OUTPUT_BYTES_MISMATCH")
        need(
            record["sequence"] == sequence
            and record["cursor_version"] == sequence
            and record["step_id"] not in ids,
            "STEP_SEQUENCE",
        )
        ids.add(record["step_id"])
        need(
            record["profile_hash"] == profile["profile_hash"]
            and record["snapshot_hash"] == run["snapshot_hash"],
            "STEP_BINDING",
        )
        need(
            record["input_hash"]
            == input_hash(
                profile["profile_hash"], run["snapshot_hash"], sequence - 1, prior
            ),
            "STEP_INPUT_HASH",
        )
        need(
            record["output_ref"] == f"run-step:{run['run_id']}:{sequence}",
            "STEP_RESULT_REF",
        )
        expected_hash = fingerprint(
            "jobforge.run.commit.v1",
            record["step_id"],
            str(sequence),
            record["kind"],
            str(sequence - 1),
            record["input_hash"],
            profile["profile_id"],
            profile["profile_hash"],
            run["snapshot_id"],
            run["snapshot_hash"],
            canonical(raw),
        )
        need(record["commit_hash"] == expected_hash, "STEP_COMMIT_HASH")
        validate_step_result(record["output"], record["kind"])
        (
            validate_agent_step
            if profile["strategy"] == "support_agent_v1"
            else validate_support_step
        )(checkpoint, record["kind"])
        if record["kind"] == "read_ticket":
            need(same_source(record["output"]["content"], ticket), "TICKET_SOURCE")
            checkpoint["snapshot"]["ticket_binding_json"] = record["output"]["content"]
        checkpoint["steps"].append(
            {"step": {"kind": record["kind"]}, "result_json": record["output"]}
        )
        prior = expected_hash
    sources = support_sources(
        checkpoint, repeated_search=profile["strategy"] == "support_agent_v1"
    )
    for kind, table, identity in (
        ("get_order", package.orders, "order"),
        ("get_delivery", package.deliveries, "delivery"),
    ):
        if kind not in sources.contents:
            continue
        content = sources.contents[kind]
        expected = table.get((binding["tenant_id"], vector[identity]["id"]))
        need(same_source(content.get(identity), expected), "BUSINESS_SOURCE")
    if "search_policy" in sources.contents:
        for hit in sources.contents["search_policy"]["matches"]:
            need(
                hit["text"] == package.paragraphs[hit["chunk_id"]]
                and hit["source"] == hit["chunk_id"].split(".")[0] + ".md",
                "POLICY_SOURCE",
            )
    proposal: dict[str, Any] = {}
    if (
        checkpoint["steps"]
        and checkpoint["steps"][-1]["step"]["kind"] == "submit_proposal"
    ):
        proposal = checkpoint["steps"][-1]["result_json"]["proposal"]
        need(
            proposal == checkpoint["steps"][-2]["result_json"]["proposal"],
            "PROPOSAL_REPLACED",
        )
        validate_persisted_proposal(
            proposal,
            {**checkpoint, "steps": checkpoint["steps"][:-2]},
            repeated_search=profile["strategy"] == "support_agent_v1",
        )
        result = row["result"]
        need(result["available"], "RESULT_BINDING")
        if proposal["decision"] == "proposal":
            need(
                result["ref"] == run["proposal_ref"] == "run-proposal:" + run["run_id"],
                "RESULT_BINDING",
            )
            need(
                run["state"] == "awaiting_approval"
                and result["kind"] == "proposal"
                and run["outcome"] is None
                and run["error"] is None,
                "RUN_OUTCOME",
            )
        else:
            need(
                result["ref"] == row["steps"][-1]["record"]["output_ref"]
                and run["proposal_ref"] is None,
                "RESULT_BINDING",
            )
            need(
                run["state"] == "succeeded"
                and run["outcome"] == "no_action"
                and result["kind"] == "no_action"
                and run["error"] is None,
                "RUN_OUTCOME",
            )
    else:
        need(
            run["state"] not in {"succeeded", "awaiting_approval"}
            and run["proposal_ref"] is None
            and run["outcome"] is None
            and not row["result"]["available"],
            "RUN_OUTCOME",
        )
    return checkpoint, proposal
