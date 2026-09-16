"""Validate the allowlisted development package without services or model calls.

This checks data bindings, not model outputs or policy-predicate correctness.
Source meanings are reviewed annotations; the validator does not infer them
from keywords, labels, or case identifiers.
"""

from __future__ import annotations

import hashlib
import json
import re
from collections import Counter
from datetime import datetime
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[2] / "examples" / "support-agent"
DATASET_VERSION = "support-dev-2026-09-16-v2"
POLICY_VERSION = "delivery-policy-dev-v2"
POLICY_FILES = tuple(f"P{number:02d}.md" for number in range(1, 11))
ARTIFACTS = frozenset(
    {
        "runtime/seed.json",
        "runtime/dataset-manifest.json",
        "runtime/policies/manifest.json",
        "evaluation/dev_gold.jsonl",
        "evaluation/case-map.jsonl",
        "evaluation/scoring-proposal.json",
        "evaluation/semantic-anchors.json",
        "evaluation/retrieval_gold.jsonl",
        "retrieval/queries.jsonl",
    }
    | {f"runtime/policies/{name}" for name in POLICY_FILES}
)
ALLOWLIST = ARTIFACTS | {"manifest.json"}
MAX_FILE_BYTES = 256 * 1024
DISPUTE_TYPES = frozenset(
    {
        "non_receipt",
        "wrong_address",
        "unauthorized_recipient",
        "unauthorized_safe_place",
    }
)
ANCHOR_COUNTS = {
    "customer_dispute": 6,
    "vague_problem": 1,
    "carrier_source_key": 2,
    "carrier_correction": 2,
}


def require(condition: bool, message: str) -> None:
    """Reject an invalid package even when Python assertions are disabled."""
    if not condition:
        raise ValueError(message)


def sha256(raw: bytes) -> str:
    """Return the lowercase digest of the original bytes."""
    return hashlib.sha256(raw).hexdigest()


def _unique_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        require(key not in result, "duplicate JSON key")
        result[key] = value
    return result


def _reject_constant(value: str) -> None:
    raise ValueError("non-finite JSON number")


def parse_json(raw: bytes) -> Any:
    """Decode strict UTF-8 JSON, including duplicate-key rejection."""
    return json.loads(
        raw.decode("utf-8", errors="strict"),
        object_pairs_hook=_unique_object,
        parse_constant=_reject_constant,
    )


def read_artifact(root: Path, relative: str) -> bytes:
    """Read one bounded, explicit development path without discovery."""
    require(relative in ALLOWLIST, "path outside development allowlist")
    path = (root / relative).resolve()
    require(path.is_relative_to(root.resolve()), "artifact escapes development root")
    with path.open("rb") as stream:
        raw = stream.read(MAX_FILE_BYTES + 1)
    require(len(raw) <= MAX_FILE_BYTES, "artifact exceeds size bound")
    raw.decode("utf-8", errors="strict")
    return raw


def instant(value: str) -> datetime:
    """Parse an aware business instant without using the execution clock."""
    result = datetime.fromisoformat(value.replace("Z", "+00:00"))
    require(result.tzinfo is not None, "timestamp lacks timezone")
    return result


def keyed(rows: list[dict[str, Any]], fields: tuple[str, ...]) -> dict[tuple, dict]:
    """Index rows by their complete identity, rejecting duplicates."""
    result = {tuple(row[field] for field in fields): row for row in rows}
    require(len(result) == len(rows), "duplicate composite identity")
    return result


def _keys(value: Any, expected: set[str]) -> None:
    require(type(value) is dict and set(value) == expected, "invalid object fields")


def validate_anchor(anchor: dict[str, Any], document: dict[str, Any]) -> None:
    """Check one reviewed anchor against a bound ticket or delivery document.

    This validates identity and source bytes only, not proof that a document
    was returned to a particular Run. A future scorer must establish that
    authorization and evidence provenance independently before calling this.
    """
    _keys(anchor, {"kind", "source", "meaning"})
    kind, source, meaning = anchor["kind"], anchor["source"], anchor["meaning"]
    require(kind in ANCHOR_COUNTS, "unknown source meaning kind")
    ticket_source = kind in {"customer_dispute", "vague_problem"}
    fields = {
        "document_kind",
        "tenant_id",
        "entity_kind",
        "entity_id",
        "revision_field",
        "revision",
        "source_pointer",
        "full_text_sha256",
        "utf8_span",
    }
    _keys(source, fields if ticket_source else fields | {"event_id"})
    require(
        type(source["revision"]) is int and source["revision"] > 0,
        "invalid source revision",
    )
    if ticket_source:
        require(
            source["document_kind"] == "ticket_binding"
            and source["entity_kind"] == "ticket"
            and source["revision_field"] == "revision",
            "wrong ticket source kind",
        )
        require(source["source_pointer"] == "/description", "wrong ticket pointer")
        entity, identity_field, revision_field = document, "ticket_id", "revision"
        value = entity["description"]
    else:
        require(
            source["document_kind"] == "delivery_evidence"
            and source["entity_kind"] == "delivery"
            and source["revision_field"] == "aggregate_revision",
            "wrong carrier source kind",
        )
        require(
            document.get("kind") == "delivery" and document.get("missing") is False,
            "carrier anchor requires actual delivery evidence",
        )
        entity = document["delivery"]
        identity_field, revision_field = "delivery_id", "aggregate_revision"
        match = re.fullmatch(
            r"/delivery/events/(0|[1-9][0-9]*)/note", source["source_pointer"]
        )
        require(match is not None, "invalid event note pointer")
        index = int(match.group(1)) if match else -1
        events = entity["events"]
        event_index = keyed(events, ("event_id",))
        require(0 <= index < len(events), "event pointer out of range")
        event = events[index]
        require(
            event["event_id"] == source["event_id"], "event pointer identity mismatch"
        )
        value = event["note"]
    require(
        entity["tenant_id"] == source["tenant_id"]
        and entity[identity_field] == source["entity_id"]
        and entity[revision_field] == source["revision"],
        "source identity mismatch",
    )
    require(type(value) is str, "source text is not a string")
    raw = value.encode("utf-8", errors="strict")
    require(sha256(raw) == source["full_text_sha256"], "source text hash mismatch")
    span = source["utf8_span"]
    _keys(span, {"start", "end", "text"})
    start, end = span["start"], span["end"]
    require(
        type(start) is int and type(end) is int and 0 <= start < end <= len(raw),
        "invalid UTF-8 byte span",
    )
    raw[:start].decode("utf-8", errors="strict")
    raw[:end].decode("utf-8", errors="strict")
    require(
        raw[start:end].decode("utf-8", errors="strict") == span["text"],
        "anchor span text mismatch",
    )
    if kind == "customer_dispute":
        _keys(meaning, {"supported_types"})
        types = meaning["supported_types"]
        require(
            type(types) is list
            and 1 <= len(types) <= 2
            and all(type(item) is str for item in types),
            "invalid dispute types",
        )
        require(
            len(set(types)) == len(types) and set(types) <= DISPUTE_TYPES,
            "unsupported dispute meaning",
        )
    elif kind == "vague_problem":
        _keys(meaning, {"field"})
        require(
            meaning["field"] == "ticket.problem_description", "invalid vague meaning"
        )
    elif kind == "carrier_source_key":
        _keys(meaning, {"source_key"})
        require(
            type(meaning["source_key"]) is str
            and re.fullmatch(
                r"[A-Za-z0-9][A-Za-z0-9._:-]{0,127}", meaning["source_key"]
            )
            is not None,
            "invalid carrier source key",
        )
    else:
        _keys(meaning, {"corrected_event_id"})
        corrected = event_index.get((meaning["corrected_event_id"],))
        if corrected is None:
            raise ValueError("correction target does not exist")
        require(
            source["event_id"] != meaning["corrected_event_id"],
            "correction target does not exist or is the same event",
        )
        require(
            event["status"] == "recovered"
            and corrected["status"] in {"lost", "damaged", "returned_to_sender"}
            and instant(event["occurred_at"]) > instant(corrected["occurred_at"]),
            "invalid structured correction relation",
        )


def expected_labels_hash(gold: list[dict[str, Any]]) -> str:
    """Hash review labels without authoring metadata or runtime transformations."""
    labels = [
        {key: row[key] for key in ("case_id", "tenant_id", "expected")} for row in gold
    ]
    return sha256(json.dumps(labels, sort_keys=True, separators=(",", ":")).encode())


def validate_package(root: Path = ROOT) -> dict[str, Any]:
    """Validate the fixed development package and return bounded metadata."""
    raw = {path: read_artifact(root, path) for path in sorted(ALLOWLIST)}
    parsed: dict[str, Any] = {
        path: (
            [parse_json(line) for line in content.splitlines()]
            if path.endswith(".jsonl")
            else parse_json(content)
        )
        for path, content in raw.items()
        if path.endswith((".json", ".jsonl"))
    }
    manifest = parsed["manifest.json"]
    require(
        set(manifest["artifact_sha256"]) == ARTIFACTS, "artifact hash coverage mismatch"
    )
    for path in sorted(ARTIFACTS):
        require(
            sha256(raw[path]) == manifest["artifact_sha256"][path],
            f"artifact hash mismatch: {path}",
        )
    seed = parsed["runtime/seed.json"]
    dataset = parsed["runtime/dataset-manifest.json"]
    policies = parsed["runtime/policies/manifest.json"]
    scoring = parsed["evaluation/scoring-proposal.json"]
    anchor_file = parsed["evaluation/semantic-anchors.json"]
    gold = parsed["evaluation/dev_gold.jsonl"]
    mapping = parsed["evaluation/case-map.jsonl"]
    require(manifest["dataset_id"] == DATASET_VERSION, "wrong registered dataset")
    for obj in (seed, dataset, scoring, anchor_file, *gold, *mapping):
        require(obj["dataset_version"] == DATASET_VERSION, "mixed dataset versions")
    for obj in (
        manifest,
        dataset,
        policies,
        scoring,
        anchor_file,
        *gold,
        *mapping,
        *seed["policies"],
        *seed["tickets"],
    ):
        require(obj["policy_version"] == POLICY_VERSION, "mixed policy versions")
    require(policies["files"] == list(POLICY_FILES), "unexpected policy files")
    combined = b"".join(
        name.encode() + b"\n" + raw[f"runtime/policies/{name}"] + b"\n"
        for name in POLICY_FILES
    )
    corpus_hash = sha256(combined)
    require(len(combined) <= 65536, "corpus exceeds bound")
    for obj in (manifest, policies, *seed["policies"]):
        require(obj["corpus_sha256"] == corpus_hash, "corpus binding mismatch")
    paragraphs: list[tuple[str, str]] = []
    for name in POLICY_FILES:
        parts = re.split(
            r"<!-- paragraph_id: (P[0-9]{2}\.[12]) -->",
            raw[f"runtime/policies/{name}"].decode(),
        )
        require(len(parts) == 5, "policy must have two registered paragraphs")
        paragraphs.extend(zip(parts[1::2], parts[2::2], strict=True))
    require(
        {key for key, _ in paragraphs}
        == {f"P{number:02d}.{part}" for number in range(1, 11) for part in (1, 2)},
        "paragraph identity mismatch",
    )
    max_paragraph = max(len(body.strip().encode()) for _, body in paragraphs)
    require(max_paragraph <= 768, "paragraph exceeds byte bound")
    tickets = keyed(seed["tickets"], ("tenant_id", "ticket_id"))
    orders = keyed(seed["orders"], ("tenant_id", "order_id"))
    deliveries = keyed(seed["deliveries"], ("tenant_id", "delivery_id"))
    labels = keyed(gold, ("case_id",))
    maps = keyed(mapping, ("case_id",))
    require(
        len(tickets) == len(labels) == len(maps) == manifest["case_count"] == 40,
        "registered case coverage mismatch",
    )
    require(set(labels) == set(maps), "gold/map cases differ")
    require(
        set(keyed(mapping, ("tenant_id", "ticket_id"))) == set(tickets),
        "ticket/map coverage mismatch",
    )
    require(len(orders) == 38 and len(deliveries) == 37, "fact counts changed")
    require(
        Counter(t["tenant_id"] for t in tickets.values()) == manifest["tenant_counts"],
        "tenant counts differ",
    )
    for field in ("action", "conclusion"):
        require(
            Counter(g["expected"][field] for g in gold) == manifest[f"{field}_counts"],
            "gold outcome counts differ",
        )
    require(
        expected_labels_hash(gold)
        == manifest["review"]["unchanged_expected_labels_sha256"],
        "reviewed expected labels changed",
    )
    event_count = 0
    for row in mapping:
        ticket = tickets[row["tenant_id"], row["ticket_id"]]
        label = labels[(row["case_id"],)]
        require(label["tenant_id"] == ticket["tenant_id"], "label tenant mismatch")
        require(
            row["as_of"]
            == ticket["observed_at"]
            == label["observation_time"]
            == dataset["as_of"],
            "observation instant mismatch",
        )
        order = orders.get((ticket["tenant_id"], ticket["order_id"]))
        require(
            (ticket["order_id"] is None) == (order is None),
            "order association mismatch",
        )
        if order is None:
            continue
        instant(order["promised_delivery_at"])
        delivery = deliveries.get((ticket["tenant_id"], order["delivery_id"]))
        require(
            (order["delivery_id"] is None) == (delivery is None),
            "delivery association mismatch",
        )
        if delivery is None:
            continue
        require(delivery["order_id"] == order["order_id"], "delivery/order mismatch")
        keyed(delivery["events"], ("event_id",))
        event_count += len(delivery["events"])
        require(
            all(
                instant(e["occurred_at"]) <= instant(ticket["observed_at"])
                for e in delivery["events"]
            ),
            "event after observation",
        )
    require(event_count == 77, "event count changed")
    queries = parsed["retrieval/queries.jsonl"]
    retrieval_gold = parsed["evaluation/retrieval_gold.jsonl"]
    require(
        len(queries) == len(retrieval_gold) == 20
        and set(keyed(queries, ("query_id",)))
        == set(keyed(retrieval_gold, ("query_id",))),
        "retrieval query coverage mismatch",
    )
    require(
        all(
            q["policy_revision"] == POLICY_VERSION and len(q["query"].encode()) <= 512
            for q in queries
        ),
        "invalid retrieval query registration",
    )
    anchors = anchor_file["anchors"]
    require(
        Counter(a["kind"] for a in anchors) == ANCHOR_COUNTS,
        "anchor kind counts differ",
    )
    identities = []
    for anchor in anchors:
        source = anchor["source"]
        key = source["tenant_id"], source["entity_id"]
        if source["document_kind"] == "ticket_binding":
            document = tickets[key]
        else:
            document = {
                "kind": "delivery",
                "missing": False,
                "delivery": deliveries[key],
            }
        validate_anchor(anchor, document)
        identities.append(
            (
                source["tenant_id"],
                source["entity_kind"],
                source["entity_id"],
                source["revision"],
                source["source_pointer"],
            )
        )
    require(len(set(identities)) == len(identities), "duplicate source anchor")
    return {
        "kind": "offline-development-data-validation-not-model-scoring",
        "status": "passed",
        "dataset_version": DATASET_VERSION,
        "policy_version": POLICY_VERSION,
        "corpus_sha256": corpus_hash,
        "counts": {
            "tickets": 40,
            "orders": 38,
            "deliveries": 37,
            "events": event_count,
            "policies": 10,
            "paragraphs": 20,
            "retrieval_queries": 20,
            "semantic_anchors": len(anchors),
        },
        "anchor_counts": ANCHOR_COUNTS,
        "max_paragraph_utf8_bytes": max_paragraph,
        "scoring_status": scoring["status"],
        "read_hashes": {path: sha256(content) for path, content in sorted(raw.items())},
    }


if __name__ == "__main__":
    print(json.dumps(validate_package(), indent=2))
