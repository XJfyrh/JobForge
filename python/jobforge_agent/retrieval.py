"""Run exactly the registered free retrieval checks and preserve every result."""

from __future__ import annotations

import asyncio
import hashlib
import time
from pathlib import Path
from typing import Any

from jobforge_agent.business import BusinessTools, SnapshotBinding
from jobforge_agent.embedding import OllamaEmbedding
from jobforge_agent.errors import ToolError
from jobforge_agent.http import BoundedHTTP, strict_json
from jobforge_agent.preparation import (
    REPO_ROOT,
    PreparationReport,
    bounded_file,
    load_registered_corpus,
)

QUERIES_PATH = REPO_ROOT / "examples" / "support-agent" / "retrieval" / "queries.jsonl"


def load_registered_queries(policy_version: str) -> tuple[list[dict[str, Any]], str]:
    """Read 20 bounded query rows; evaluation gold is never opened here."""
    raw = bounded_file(QUERIES_PATH, 32 * 1024)
    rows = [strict_json(line) for line in raw.splitlines()]
    if len(rows) != 20:
        raise ToolError("INVALID_QUERY_SET")
    for number, row in enumerate(rows, 1):
        if (
            not isinstance(row, dict)
            or set(row) != {"query_id", "query", "policy_revision"}
            or row["query_id"] != f"RQ-{number:02d}"
            or row["policy_revision"] != policy_version
            or not isinstance(row["query"], str)
            or not row["query"].strip()
        ):
            raise ToolError("INVALID_QUERY_SET")
        try:
            if len(row["query"].encode("utf-8")) > 512:
                raise ToolError("INVALID_QUERY_SET")
        except UnicodeError as exc:
            raise ToolError("INVALID_QUERY_SET") from exc
    return rows, hashlib.sha256(raw).hexdigest()


def snapshot_binding(
    snapshot: dict[str, Any], snapshot_id: str, profile: dict[str, Any]
) -> SnapshotBinding:
    """Validate authoritative snapshot metadata against the registered corpus."""
    index = snapshot.get("index")
    ticket = snapshot.get("ticket")
    if (
        snapshot.get("snapshot_id") != snapshot_id
        or snapshot.get("schema_version") != 1
        or not isinstance(index, dict)
        or not isinstance(ticket, dict)
        or not isinstance(index.get("profile_hash"), str)
        or not isinstance(index.get("index_id"), str)
        or index.get("profile") != profile
        or index.get("tenant_id") != snapshot.get("tenant_id")
        or ticket.get("tenant_id") != snapshot.get("tenant_id")
        or "order_id" not in ticket
    ):
        raise ToolError("PROFILE_UNAVAILABLE")
    return SnapshotBinding(
        snapshot_id=snapshot_id,
        order_id=ticket["order_id"],
        profile_hash=index["profile_hash"],
        index_id=index["index_id"],
        policy_version=profile["policy_version"],
    )


async def retrieve(
    output_dir: Path,
    local_origin: str,
    business_origin: str,
    bearer_key: str,
    snapshot_id: str,
) -> Path:
    """Attempt at most one embedding and one search per query, without retry."""
    profile, _chunks = load_registered_corpus()
    queries, query_hash = load_registered_queries(profile["policy_version"])
    # Validate the UUID before constructing an HTTP path, independent of metadata.
    SnapshotBinding(snapshot_id, None, "0" * 64, snapshot_id, profile["policy_version"])
    report = PreparationReport(output_dir, profile, "retrieval")
    report.value.update(
        {
            "snapshot_id": snapshot_id,
            "query_set_sha256": query_hash,
            "rows": [
                {"query_id": row["query_id"], "status": "not_attempted"}
                for row in queries
            ],
        }
    )
    report.save()
    metadata = BoundedHTTP(
        business_origin, bearer_key=bearer_key, before_send=report.before_send
    )
    embedding = OllamaEmbedding(local_origin, before_send=report.before_send)
    tools: BusinessTools | None = None
    deadline = time.monotonic() + 1200
    try:
        async with asyncio.timeout(1200):
            snapshot = await metadata.request(
                "GET",
                f"/business/v1/snapshots/{snapshot_id}",
                deadline=min(deadline, time.monotonic() + 10),
                max_response=8 * 1024,
            )
            binding = snapshot_binding(snapshot, snapshot_id, profile)
            report.value["index_id"] = binding.index_id
            report.value["profile_hash"] = binding.profile_hash
            tools = BusinessTools(
                business_origin,
                bearer_key,
                binding,
                embedding,
                before_send=report.before_send,
            )
            for row, result in zip(queries, report.value["rows"], strict=True):
                result["status"] = "unknown"
                report.save()
                try:
                    response = await tools.execute(
                        "search_policy", {"query": row["query"]}, deadline=deadline
                    )
                    result.update(
                        {"status": "returned", "matches": response["matches"]}
                    )
                except ToolError as exc:
                    result.update({"status": "failed", "error_code": exc.code})
                    report.value["failed_operations"] += 1
                report.save()
            report.value["state"] = "completed"
            report.save()
    except (ToolError, TimeoutError, OSError) as exc:
        report.value["error_code"] = (
            exc.code if isinstance(exc, ToolError) else "RETRIEVAL_FAILED"
        )
        report.value["failed_operations"] += 1
        report.save()
        raise ToolError(report.value["error_code"]) from exc
    finally:
        if tools is not None:
            await tools.aclose()
        await embedding.aclose()
        await metadata.aclose()
    return report.path
