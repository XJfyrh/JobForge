"""Read a reviewed family-disjoint 20-case package only after candidate freeze."""

from __future__ import annotations

import re
from pathlib import Path
from typing import Any

from tools.support_evaluation.evidence import Package, fields, need
from tools.support_evaluation.predicates import derive_facts
from tools.support_evaluation.validate_data import (
    ARTIFACTS,
    MAX_FILE_BYTES,
    POLICY_FILES,
    POLICY_VERSION,
    ROOT,
    instant,
    keyed,
    parse_json,
    sha256,
    validate_anchor,
)

DATASET = "support-s5-2026-10-07-v1"
DATASET_V2 = "support-s5-2026-10-07-v2"
DATASETS = {DATASET, DATASET_V2}
ARTIFACT_NAMES = (ARTIFACTS - {"evaluation/dev_gold.jsonl"}) | {
    "evaluation/gold.jsonl",
    "evaluation/family-review.json",
}


def read(root: Path, path: str) -> bytes:
    """Read only bounded allowlisted files in the explicit package root."""
    need(path in ARTIFACT_NAMES | {"manifest.json"}, "PACKAGE_PATH")
    resolved = (root / path).resolve()
    need(resolved.is_relative_to(root.resolve()), "PACKAGE_PATH")
    raw = resolved.read_bytes()
    need(len(raw) <= MAX_FILE_BYTES, "PACKAGE_SIZE")
    raw.decode("utf-8", errors="strict")
    return raw


def load_package(root: Path, freeze_path: Path, *, historical: bool = False) -> Package:
    """Require unchanged code/threshold freeze, independent review and real facts.

    Family descriptions are reviewed data, like semantic anchors. Hash and set
    checks cannot establish their meaning; the independently accepted review
    receipt must explicitly confirm scenario/template novelty beyond renaming.
    """
    freeze_raw = freeze_path.read_bytes()
    freeze = parse_json(freeze_raw)
    need(freeze["status"] == "frozen_before_unseen_creation", "CANDIDATE_NOT_FROZEN")
    from tools.support_s5.freeze import validate

    if not historical:
        validate(freeze)
    else:
        from tools.support_s5.score import THRESHOLDS

        need(freeze["thresholds"] == THRESHOLDS, "HISTORICAL_FREEZE_THRESHOLDS")
    manifest = parse_json(read(root, "manifest.json"))
    fields(
        manifest,
        {
            "schema_version",
            "dataset_id",
            "case_count",
            "authored_at",
            "freeze_sha256",
            "artifact_sha256",
            "corpus_sha256",
        },
    )
    need(
        manifest["schema_version"] == 1
        and manifest["dataset_id"] in DATASETS
        and manifest["case_count"] == 20,
        "PACKAGE_VERSION",
    )
    dataset = manifest["dataset_id"]
    need(not historical or dataset == DATASET, "HISTORICAL_DIAGNOSTIC_ONLY")
    need(
        manifest["freeze_sha256"] == sha256(freeze_raw)
        and instant(manifest["authored_at"]) > instant(freeze["frozen_at"]),
        "UNSEEN_FREEZE_ORDER",
    )
    fields(manifest["artifact_sha256"], set(ARTIFACT_NAMES))
    raw = {name: read(root, name) for name in ARTIFACT_NAMES}
    hashes = {name: sha256(data) for name, data in raw.items()}
    need(hashes == manifest["artifact_sha256"], "PACKAGE_HASH")
    parsed: dict[str, Any] = {
        name: (
            [parse_json(line) for line in data.splitlines()]
            if name.endswith(".jsonl")
            else parse_json(data)
        )
        for name, data in raw.items()
        if not name.endswith(".md")
    }
    review = parsed["evaluation/family-review.json"]
    review_fields = {
        "status",
        "reviewed_at",
        "development_manifest_sha256",
        "scenario_template_novelty_confirmed",
        "cases",
    }
    if dataset == DATASET_V2:
        review_fields |= {
            "seen_formal_manifest_sha256",
            "novelty_against_seen_formal_confirmed",
        }
    fields(review, review_fields)
    development_raw = (ROOT / "manifest.json").read_bytes()
    development = parse_json(development_raw)
    need(
        review["status"] == "independently_accepted"
        and review["scenario_template_novelty_confirmed"] is True
        and review["development_manifest_sha256"]
        == sha256(development_raw)
        == freeze["development_manifest_sha256"],
        "FAMILY_REVIEW",
    )
    need(
        instant(review["reviewed_at"]) >= instant(manifest["authored_at"]),
        "FAMILY_REVIEW_ORDER",
    )
    previous = {row["family"] for row in development["variants"]}
    if dataset == DATASET_V2:
        prior = freeze.get("seen_formal_package", {})
        need(
            prior.get("dataset_id") == DATASET
            and review["seen_formal_manifest_sha256"] == prior.get("manifest_sha256")
            and review["novelty_against_seen_formal_confirmed"] is True,
            "SEEN_FORMAL_NOVELTY_REVIEW",
        )
        previous.update(prior["template_families"])
    families = keyed(review["cases"], ("case_id",))
    seed = parsed["runtime/seed.json"]
    maps = keyed(parsed["evaluation/case-map.jsonl"], ("case_id",))
    labels = keyed(parsed["evaluation/gold.jsonl"], ("case_id",))
    need(
        len(maps) == len(labels) == len(families) == 20
        and set(maps) == set(labels) == set(families),
        "PACKAGE_COVERAGE",
    )
    tickets = keyed(seed["tickets"], ("tenant_id", "ticket_id"))
    orders = keyed(seed["orders"], ("tenant_id", "order_id"))
    deliveries = keyed(seed["deliveries"], ("tenant_id", "delivery_id"))
    need(
        len(tickets) == 20
        and {(r["tenant_id"], r["ticket_id"]) for r in maps.values()} == set(tickets),
        "PACKAGE_COVERAGE",
    )
    need(
        seed["dataset_version"]
        == parsed["runtime/dataset-manifest.json"]["dataset_version"]
        == dataset,
        "PACKAGE_VERSION",
    )
    need(
        all(t["policy_version"] == POLICY_VERSION for t in tickets.values()),
        "PACKAGE_VERSION",
    )
    anchors = parsed["evaluation/semantic-anchors.json"]["anchors"]
    for anchor in anchors:
        s = anchor["source"]
        document = (
            tickets[(s["tenant_id"], s["entity_id"])]
            if s["entity_kind"] == "ticket"
            else {
                "kind": "delivery",
                "missing": False,
                "delivery": deliveries[(s["tenant_id"], s["entity_id"])],
            }
        )
        validate_anchor(anchor, document)
    for key, row in maps.items():
        family = families[key]
        fields(
            family,
            {
                "case_id",
                "template_family",
                "scenario_definition",
                "development_overlap_review",
            },
        )
        need(
            row["template_family"] == family["template_family"]
            and row["template_family"] not in previous
            and family["scenario_definition"]
            and family["development_overlap_review"],
            "FAMILY_OVERLAP",
        )
        need(
            row["dataset_version"] == dataset
            and row["policy_version"] == POLICY_VERSION,
            "PACKAGE_VERSION",
        )
        ticket = tickets[(row["tenant_id"], row["ticket_id"])]
        order = orders.get((ticket["tenant_id"], ticket["order_id"]))
        delivery = (
            deliveries.get((ticket["tenant_id"], order["delivery_id"]))
            if order
            else None
        )
        need((ticket["order_id"] is None) == (order is None), "PACKAGE_ASSOCIATION")
        need(
            order is None or (order["delivery_id"] is None) == (delivery is None),
            "PACKAGE_ASSOCIATION",
        )
        need(
            delivery is None
            or order is not None
            and delivery["order_id"] == order["order_id"],
            "PACKAGE_ASSOCIATION",
        )
        gold = labels[key]
        need(
            row["as_of"]
            == ticket["observed_at"]
            == gold["observation_time"]
            == "2026-09-16T12:00:00Z",
            "PACKAGE_OBSERVATION",
        )
        need(
            gold["dataset_version"] == dataset
            and gold["tenant_id"] == ticket["tenant_id"],
            "PACKAGE_VERSION",
        )
        facts = derive_facts(ticket, order, delivery, anchors)
        mapping = {
            "record_resolution": "record_conclusion",
            "escalate_human": "escalate",
            "request_information": "request_information",
            "none": "",
        }
        expected: dict[str, Any] = {
            **gold["expected"],
            "action": mapping[gold["expected"]["action"]],
        }
        expected["decision"] = (
            "no_action" if gold["expected"]["action"] == "none" else "proposal"
        )
        expected["requested_fields"] = sorted(expected["requested_fields"])
        need(facts.expected == expected, "POLICY_GOLD_CONTRACT_MISMATCH")
    corpus = b"".join(
        name.encode() + b"\n" + raw["runtime/policies/" + name] + b"\n"
        for name in POLICY_FILES
    )
    need(
        sha256(corpus) == manifest["corpus_sha256"] and len(corpus) <= 65536,
        "PACKAGE_CORPUS",
    )
    paragraphs = {}
    for name in POLICY_FILES:
        parts = re.split(
            r"<!-- paragraph_id: (P[0-9]{2}\.[12]) -->",
            raw["runtime/policies/" + name].decode(),
        )
        need(len(parts) == 5, "PACKAGE_PARAGRAPHS")
        paragraphs.update(
            {
                key: text.strip()
                for key, text in zip(parts[1::2], parts[2::2], strict=True)
            }
        )
    # Common source/claim scorer retains its protected raw export unchanged.
    hashes["evaluation/dev_gold.jsonl"] = hashes["evaluation/gold.jsonl"]
    return Package(
        hashes,
        sha256(corpus),
        {k[0]: v for k, v in maps.items()},
        {k[0]: v for k, v in labels.items()},
        tickets,
        orders,
        deliveries,
        anchors,
        paragraphs,
    )
