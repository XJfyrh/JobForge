"""A failed retrieval must remain in the evaluator denominator."""

import importlib.util
from pathlib import Path

MODULE = Path(__file__).resolve().parents[1] / "evaluate_policy_retrieval.py"
SPEC = importlib.util.spec_from_file_location("retrieval_scoring", MODULE)
assert SPEC and SPEC.loader
scoring = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(scoring)


def test_failed_and_unattempted_rows_reduce_scores() -> None:
    """Keep all twenty rows in the score denominator, regardless of status."""
    gold = [
        {"query_id": f"RQ-{n:02d}", "relevant_policy_ids": ["P01"]}
        for n in range(1, 21)
    ]
    rows = [{"query_id": row["query_id"], "status": "not_attempted"} for row in gold]
    rows[0] = {
        "query_id": "RQ-01",
        "status": "returned",
        "matches": [{"chunk_id": "P02.1"}, {"chunk_id": "P01.1"}],
    }
    rows[1] = {"query_id": "RQ-02", "status": "failed", "error_code": "TIMEOUT"}
    result = scoring.score({"prepare_id": "test", "rows": rows}, gold)
    assert result["total_queries"] == 20
    assert result["hit_at_3"] == 0.05
    assert result["mrr_at_3"] == 0.025
    assert len(result["rows"]) == 20
