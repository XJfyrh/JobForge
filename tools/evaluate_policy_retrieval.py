"""Score the fixed 20-query report, retaining failed and unattempted rows."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]


def score(report: dict[str, Any], gold: list[dict[str, Any]]) -> dict[str, Any]:
    """Compute policy hit@3 and reciprocal rank over the complete fixed set."""
    expected = [f"RQ-{number:02d}" for number in range(1, 21)]
    if [row["query_id"] for row in gold] != expected:
        raise ValueError("invalid gold registration")
    rows = report.get("rows", [])
    if [row.get("query_id") for row in rows] != expected:
        raise ValueError("report must retain all 20 query rows")
    scored = []
    for row, label in zip(rows, gold, strict=True):
        rank = None
        if row.get("status") == "returned":
            matches = row.get("matches", [])
            if not 1 <= len(matches) <= 3:
                raise ValueError("invalid top-k result")
            for number, match in enumerate(matches, 1):
                policy = match["chunk_id"].split(".")[0]
                if policy in label["relevant_policy_ids"]:
                    rank = number
                    break
        scored.append(
            {
                "query_id": row["query_id"],
                "status": row.get("status"),
                "first_relevant_rank": rank,
                "hit_at_3": rank is not None,
                "error_code": row.get("error_code"),
            }
        )
    return {
        "schema_version": 1,
        "kind": "fixed-policy-retrieval-evaluation",
        "prepare_id": report["prepare_id"],
        "index_id": report.get("index_id"),
        "profile_hash": report.get("profile_hash"),
        "query_set_sha256": report.get("query_set_sha256"),
        "total_queries": 20,
        "returned": sum(row["status"] == "returned" for row in scored),
        "hits_at_3": sum(row["hit_at_3"] for row in scored),
        "hit_at_3": sum(row["hit_at_3"] for row in scored) / 20,
        "mrr_at_3": sum(
            1 / row["first_relevant_rank"] if row["first_relevant_rank"] else 0
            for row in scored
        )
        / 20,
        "rows": scored,
    }


def main() -> None:
    """Read only evaluator artifacts; this command never calls tools or models."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    with args.report.open("rb") as stream:
        raw = stream.read(1024 * 1024 + 1)
    if len(raw) > 1024 * 1024:
        raise ValueError("report too large")
    gold_path = ROOT / "examples/support-agent/evaluation/retrieval_gold.jsonl"
    gold = [json.loads(line) for line in gold_path.read_text("utf-8").splitlines()]
    result = score(json.loads(raw), gold)
    args.output.write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")
    print(
        json.dumps(
            {key: value for key, value in result.items() if key != "rows"},
            sort_keys=True,
        )
    )


if __name__ == "__main__":
    main()
