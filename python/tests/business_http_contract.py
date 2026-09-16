"""Exercise real Go HTTP and PostgreSQL from the integration-test subprocess.

The search vector is a synthetic basis vector for mechanical contract coverage.
This program never calls a model and does not establish embedding quality.
It is deliberately not named for pytest discovery.
"""

from __future__ import annotations

import asyncio
import json
import sys
import time
from typing import Any
from urllib.parse import urlsplit

from jobforge_agent.business import BusinessTools, SnapshotBinding
from jobforge_agent.embedding import MODEL, MODEL_DIGEST, OllamaEmbedding
from jobforge_agent.errors import ToolError
from jobforge_agent.http import BoundedHTTP, strict_json


def read_input() -> dict[str, Any]:
    """Accept one bounded caller frame, never endpoint or credentials from argv."""
    raw = sys.stdin.buffer.read(4097)
    if len(raw) > 4096:
        raise ToolError("INVALID_CONTRACT_INPUT")
    value = strict_json(raw)
    if (
        not isinstance(value, dict)
        or set(value)
        != {"schema_version", "url", "reader_key", "other_reader_key", "binding"}
        or type(value["schema_version"]) is not int
        or value["schema_version"] != 1
        or not all(
            isinstance(value[key], str)
            for key in ("url", "reader_key", "other_reader_key")
        )
        or not isinstance(value["binding"], dict)
    ):
        raise ToolError("INVALID_CONTRACT_INPUT")
    address = urlsplit(value["url"])
    if (
        address.scheme != "http"
        or address.hostname != "127.0.0.1"
        or address.port is None
        or address.username is not None
        or address.password is not None
        or address.path
        or address.query
        or address.fragment
    ):
        raise ToolError("INVALID_CONTRACT_INPUT")
    return value


async def check(value: dict[str, Any]) -> dict[str, Any]:
    """Verify actual read-tool traffic and reject unauthorized dispatch locally."""
    binding = SnapshotBinding(**value["binding"])
    deadline = time.monotonic() + 45
    requests: list[str] = []
    model_requests: list[str] = []

    def forbid_model(path: str) -> None:
        model_requests.append(path)
        raise ToolError("MODEL_CALL_FORBIDDEN_IN_MECHANICAL_CONTRACT")

    embedding = OllamaEmbedding(before_send=forbid_model)
    tools = BusinessTools(
        value["url"],
        value["reader_key"],
        binding,
        embedding,
        before_send=requests.append,
    )
    other_tenant = BusinessTools(
        value["url"],
        value["other_reader_key"],
        binding,
        embedding,
        before_send=requests.append,
    )
    search = BoundedHTTP(
        value["url"], bearer_key=value["reader_key"], before_send=requests.append
    )
    local_rejections = 0
    try:
        for name, kind in (("get_order", "order"), ("get_delivery", "delivery")):
            result = await tools.execute(
                name, {"order_id": binding.order_id}, deadline=deadline
            )
            assert result["snapshot_id"] == binding.snapshot_id
            assert (
                result["evidence_ref"]
                == f"business-evidence:{binding.snapshot_id}:{kind}"
            )
            assert result["missing"] is False
            assert result[kind]["order_id"] == binding.order_id
            assert result[kind]["status"] == "in_transit"
            if kind == "delivery":
                assert result[kind]["aggregate_revision"] == 1
                assert len(result[kind]["events"]) == 1

        # This is a real pgvector request with an explicitly synthetic vector.
        result = await search.request(
            "POST",
            f"/business/v1/snapshots/{binding.snapshot_id}/policies/search",
            deadline=deadline,
            max_response=8192,
            body={
                "embedding_model": MODEL,
                "embedding_digest": MODEL_DIGEST,
                "query_vector": [1.0] + [0.0] * 383,
            },
        )
        assert result["snapshot_id"] == binding.snapshot_id
        matches = result["matches"]
        assert [hit["chunk_id"] for hit in matches] == ["chunk-a", "chunk-b", "chunk-c"]
        assert matches[0]["distance"] == 0.0
        for hit in matches:
            assert hit["index_id"] == binding.index_id
            assert hit["policy_version"] == binding.policy_version
            assert (
                hit["evidence_ref"]
                == f"business-policy:{binding.index_id}:{hit['chunk_id']}"
            )

        try:
            await other_tenant.execute(
                "get_order", {"order_id": binding.order_id}, deadline=deadline
            )
        except ToolError as exc:
            assert exc.code == "NOT_FOUND"
        else:
            raise AssertionError("cross-tenant snapshot was readable")

        before_rejections = len(requests)
        for name, arguments, code in (
            ("write_ticket", {}, "UNKNOWN_TOOL"),
            ("get_order", {"order_id": "unrelated-order"}, "OBJECT_NOT_AUTHORIZED"),
        ):
            try:
                await tools.execute(name, arguments, deadline=deadline)
            except ToolError as exc:
                assert exc.code == code
                local_rejections += 1
            else:
                raise AssertionError("local capability guard accepted invalid input")
        assert len(requests) == before_rejections == 4
        assert model_requests == []
        return {
            "status": "passed",
            "http_requests": len(requests),
            "local_rejections": local_rejections,
            "model_requests": len(model_requests),
        }
    finally:
        await search.aclose()
        await other_tenant.aclose()
        await tools.aclose()
        await embedding.aclose()


def main() -> int:
    """Return only a bounded result or sanitized code, without traceback payloads."""
    try:
        result = asyncio.run(check(read_input()))
    except (AssertionError, ToolError, KeyError, TypeError, ValueError, OSError):
        print(json.dumps({"error": {"code": "BUSINESS_HTTP_CONTRACT_FAILED"}}))
        return 1
    print(json.dumps(result, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
